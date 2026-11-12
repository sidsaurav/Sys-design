package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The payments service. Its copies share one ledger, as copies of a service share a database.
type ledger struct {
	mu      sync.Mutex
	charged map[string]int  // payment → the number of times it has been charged
	keys    map[string]bool // the idempotency keys that have been used
}

// charge takes the money. A request that carries a key is carried out once, however often it
// arrives (Chapter 1.5).
func (l *ledger) charge(payment, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if key != "" {
		if l.keys[key] {
			return
		}
		l.keys[key] = true
	}
	l.charged[payment]++
}

// A copy of payments. A faulty one does the work, and then loses one answer in three.
func paymentsCopy(l *ledger, faulty bool) *httptest.Server {
	var reads, charges atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := &reads
		if r.Method == http.MethodPost {
			n = &charges
			l.charge(r.URL.Query().Get("payment"), r.Header.Get("Idempotency-Key"))
		}
		if faulty && n.Add(1)%3 == 0 {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close() // the caller will never know what happened
				return
			}
		}
		fmt.Fprint(w, "ok")
	}))
}

// A rule says when a proxy may send a request to a second copy.
type rule struct {
	what  string
	again func(r *http.Request) bool
}

// HTTP's own promise: a request with one of these methods may be repeated without further
// effect (RFC 9110, Chapter 1.3).
func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

var rules = []rule{
	{"never", func(*http.Request) bool { return false }},
	{"whenever a try fails", func(*http.Request) bool { return true }},
	{"only for the methods that HTTP calls idempotent", func(r *http.Request) bool { return idempotent(r.Method) }},
	{"…or when the request carries an Idempotency-Key", func(r *http.Request) bool {
		return idempotent(r.Method) || r.Header.Get("Idempotency-Key") != ""
	}},
}

// The control plane holds what every proxy needs to know: where the copies are, and the rule.
// It also collects what the proxies report.
type controlPlane struct {
	copies []string
	rule   atomic.Pointer[rule]
	mu     sync.Mutex
	seen   map[string]*route
}

type route struct {
	requests, again, failed int
	took                    []time.Duration
}

// A proxy runs beside one caller, and carries that caller's requests to the service.
type proxy struct {
	plane *controlPlane
	next  atomic.Int64
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	body, _ := io.ReadAll(r.Body)
	copies, tries := p.plane.copies, 1
	if p.plane.rule.Load().again(r) {
		tries = len(copies)
	}
	first, again, answered := int(p.next.Add(1)), 0, false
	for i := 0; i < tries && !answered; i++ {
		req, _ := http.NewRequest(r.Method, copies[(first+i)%len(copies)]+r.URL.RequestURI(), bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := plain.Do(req)
		if err != nil {
			again++
			continue
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		resp.Body.Close()
		answered = true
	}
	if !answered {
		again-- // the last try was not followed by another
		http.Error(w, "no copy answered", http.StatusBadGateway)
	}
	p.plane.record(r.Method+" "+r.URL.Path, time.Since(began), again, !answered)
}

func (c *controlPlane) record(name string, took time.Duration, again int, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rt := c.seen[name]
	if rt == nil {
		rt = &route{}
		c.seen[name] = rt
	}
	rt.requests++
	rt.again += again
	if failed {
		rt.failed++
	}
	rt.took = append(rt.took, took)
}

const (
	callers  = 40   // forty services call payments, each through a proxy of its own
	payments = 1500 // charges, and as many reads
)

func part2() {
	fmt.Println("PART 2 — \"payments\" has three copies. One does the work, and then loses one answer in three.")
	fmt.Printf("Forty callers make %s reads and %s charges between them, each through a proxy of its own.\n", commas(payments), commas(payments))
	fmt.Println("Every proxy takes its rule from one control plane. Only the rule changes from row to row.")
	fmt.Println()
	fmt.Printf("  %-48s %10s %12s %10s %13s\n", "a proxy tries another copy", "reads that", "charges that", "…but were", "payments")
	fmt.Printf("  %-48s %10s %12s %10s %13s\n", "", "failed", "failed", "made", "charged twice")

	var last *controlPlane
	for n, ru := range rules {
		l := &ledger{charged: map[string]int{}, keys: map[string]bool{}}
		servers := []*httptest.Server{paymentsCopy(l, false), paymentsCopy(l, false), paymentsCopy(l, true)}
		plane := &controlPlane{seen: map[string]*route{}}
		for _, s := range servers {
			plane.copies = append(plane.copies, s.URL)
		}
		plane.rule.Store(&ru)
		keyed := n == len(rules)-1 // in the last row the callers send a key with each charge

		var readsFailed, chargesFailed atomic.Int64
		var mu sync.Mutex
		unsure := []string{} // the charges whose callers were told that they had failed
		var wg sync.WaitGroup
		for c := 0; c < callers; c++ {
			beside := &proxy{plane: plane}
			beside.next.Store(int64(c))
			side := httptest.NewServer(beside)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer side.Close()
				for i := c; i < payments; i += callers {
					// The caller's whole knowledge of the network: its own proxy's address.
					if resp, err := http.Get(side.URL + "/balance"); err != nil || resp.StatusCode != http.StatusOK {
						readsFailed.Add(1)
					} else {
						resp.Body.Close()
					}
					id := fmt.Sprintf("p%d", i)
					req, _ := http.NewRequest(http.MethodPost, side.URL+"/charge?payment="+id, nil)
					if keyed {
						req.Header.Set("Idempotency-Key", id)
					}
					if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
						chargesFailed.Add(1)
						mu.Lock()
						unsure = append(unsure, id)
						mu.Unlock()
					} else {
						resp.Body.Close()
					}
				}
			}()
		}
		wg.Wait()
		for _, s := range servers {
			s.Close()
		}
		made, twice := 0, 0
		for _, id := range unsure {
			if l.charged[id] > 0 {
				made++
			}
		}
		for _, n := range l.charged {
			if n > 1 {
				twice++
			}
		}
		fmt.Printf("  %-48s %10s %12s %10s %13s\n", ru.what, commas(int(readsFailed.Load())), commas(int(chargesFailed.Load())), commas(made), commas(twice))
		last = plane
	}
	fmt.Println("  (in the last row each caller sends a key with its charge; payments has honoured keys all along)")

	fmt.Println()
	fmt.Println("  what the proxies reported during the last run, with no code for it in a caller or in payments:")
	fmt.Printf("    %-16s %10s %13s %8s %10s %10s\n", "route", "requests", "tried again", "failed", "median", "p99")
	names := []string{}
	for name := range last.seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rt := last.seen[name]
		sort.Slice(rt.took, func(a, b int) bool { return rt.took[a] < rt.took[b] })
		ms := func(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }
		fmt.Printf("    %-16s %10s %13s %8s %10s %10s\n", name, commas(rt.requests), commas(rt.again), commas(rt.failed),
			ms(rt.took[len(rt.took)/2]), ms(rt.took[len(rt.took)*99/100]))
	}
}
