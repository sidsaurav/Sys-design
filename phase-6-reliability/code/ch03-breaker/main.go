// Chapter 6.3 — an optional dependency becomes slow. What happens to the page that uses it?
//
// A checkout page needs the payments service, and shows recommendations if it
// can get them. The checkout service has 20 workers. 200 users a second ask
// for the page, and each waits one second. From second 1 to second 4,
// recommendations takes 2 s to answer, where it normally takes 5 ms.
//
// The same five and a half seconds are run four times, with real HTTP
// servers inside this process:
//
//	(a) the page treats recommendations like payments: no answer, no page
//	(b) it waits 50 ms for recommendations, and then goes without
//	(c) as (b), and no more than 2 calls to recommendations at a time (a bulkhead)
//	(d) as (b), with a circuit breaker
//
//	go run ./phase-6-reliability/code/ch03-breaker
//	go run ./phase-6-reliability/code/ch03-breaker -users 400
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ a circuit breaker

// A breaker stops calls to something that keeps failing, and now and then lets one through to see.
type breaker struct {
	mu        sync.Mutex
	failures  int           // failures in a row
	threshold int           // this many open the breaker
	rest      time.Duration // how long it stays open before a trial call
	openUntil time.Time
	trial     bool // half-open: one trial call is out, and nothing else may pass
}

// allow says whether a call may be made now.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.failures < b.threshold: // closed: calls pass
		return true
	case time.Now().Before(b.openUntil) || b.trial: // open: fail at once, without calling
		return false
	default: // half-open: let one call through
		b.trial = true
		return true
	}
}

// record tells the breaker how a call that it allowed turned out.
func (b *breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trial = false
	if ok {
		b.failures = 0 // closed again
		return
	}
	if b.failures++; b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.rest)
	}
}

// ------------------------------------------------------------ the checkout service

type policy struct {
	name      string
	wait      time.Duration // how long to wait for recommendations (0: as long as the user does)
	essential bool          // no recommendations, no page
	bulkhead  chan struct{} // if set: a call to recommendations needs a place in here
	breaker   *breaker
}

const (
	workers  = 20
	patience = time.Second
	slowFrom = 1 * time.Second
	slowTo   = 4 * time.Second
	runFor   = 5500 * time.Millisecond
)

var (
	client               = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1000}}
	users                = flag.Int("users", 200, "how many users a second ask for the page")
	waiting, mostWaiting atomic.Int64 // workers that are waiting for recommendations: now, and the most at once
)

func get(ctx context.Context, url string) bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// recommend fetches recommendations under the policy, and says whether it got them.
func (p *policy) recommend(ctx context.Context, url string) bool {
	if p.breaker != nil && !p.breaker.allow() {
		return false // the breaker is open: do not even ask
	}
	if p.bulkhead != nil {
		select {
		case p.bulkhead <- struct{}{}:
			defer func() { <-p.bulkhead }()
		default:
			return false // every place is taken by a call that is waiting: go without
		}
	}
	if p.wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.wait)
		defer cancel()
	}
	now := waiting.Add(1)
	for most := mostWaiting.Load(); now > most && !mostWaiting.CompareAndSwap(most, now); most = mostWaiting.Load() {
	}
	ok := get(ctx, url)
	waiting.Add(-1)
	if p.breaker != nil {
		p.breaker.record(ok)
	}
	return ok
}

type result struct {
	start, took time.Duration
	ok, recs    bool
}

func run(p *policy) (results []result, reached []time.Duration) {
	begin := time.Now()
	var mu sync.Mutex
	sleep := func(ctx context.Context, d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-ctx.Done():
			return false
		}
	}
	payments := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sleep(r.Context(), 5*time.Millisecond)
	}))
	recommendations := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at, takes := time.Since(begin), 5*time.Millisecond
		mu.Lock()
		reached = append(reached, at)
		mu.Unlock()
		if at >= slowFrom && at < slowTo {
			takes = 2 * time.Second
		}
		if !sleep(r.Context(), takes) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	pool := make(chan struct{}, workers)
	checkout := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // wait for a worker
		case pool <- struct{}{}:
			defer func() { <-pool }()
		case <-r.Context().Done():
			return
		}
		if !get(r.Context(), payments.URL) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		recs := p.recommend(r.Context(), recommendations.URL)
		if !recs && p.essential {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if recs {
			w.Header().Set("X-Recommendations", "yes")
		}
	}))

	var wg sync.WaitGroup
	waiting.Store(0)
	mostWaiting.Store(0)
	for tick := time.NewTicker(time.Second / time.Duration(*users)); time.Since(begin) < runFor; <-tick.C {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), patience)
			defer cancel()
			res := result{start: time.Since(begin)}
			req, _ := http.NewRequestWithContext(ctx, "GET", checkout.URL, nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				res.ok = resp.StatusCode == http.StatusOK
				res.recs = resp.Header.Get("X-Recommendations") != ""
			}
			res.took = time.Since(begin) - res.start
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	checkout.Close()
	recommendations.Close()
	payments.Close()
	return results, reached
}

func main() {
	flag.Parse()
	fmt.Printf("A checkout page needs payments, and shows recommendations if it can. 20 workers; %d users a second;\n", *users)
	fmt.Println("a user waits 1 s. From second 1 to second 4, recommendations takes 2 s to answer, not 5 ms.")
	fmt.Println()
	fmt.Printf("  %-50s %-62s %s\n", "", "USERS WHO ARRIVE IN SECONDS 1 TO 3", "AFTER SECOND 4")
	fmt.Printf("  %-50s %9s %10s %12s %14s %14s %22s\n", "", "pages", "median", "99th", "most workers", "calls to", "recommendations are")
	fmt.Printf("  %-50s %9s %10s %12s %14s %14s %22s\n", "", "answered", "page", "percentile", "held at once", "recommend.", "back on the page in")
	for _, p := range []*policy{
		{name: "(a) no recommendations, no page", essential: true},
		{name: "(b) waits 50 ms for them, then goes without", wait: 50 * time.Millisecond},
		{name: "(c) as (b), and at most 2 such calls at a time", wait: 50 * time.Millisecond, bulkhead: make(chan struct{}, 2)},
		{name: "(d) as (b), with a circuit breaker", wait: 50 * time.Millisecond,
			breaker: &breaker{threshold: 5, rest: 500 * time.Millisecond}},
	} {
		results, reached := run(p)
		var took []time.Duration
		during, answered, calls := 0, 0, 0
		back := time.Duration(-1)
		for _, r := range results {
			if r.start >= slowFrom && r.start < slowTo-patience { // these users will have gone before the recovery
				during++
				if r.ok {
					answered++
					took = append(took, r.took)
				}
			}
			if done := r.start + r.took - slowTo; r.recs && done > 0 && (back < 0 || done < back) {
				back = done
			}
		}
		for _, at := range reached {
			if at >= slowFrom && at < slowTo-patience {
				calls++
			}
		}
		sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
		median, p99 := "—", "—"
		if len(took) > 0 {
			median = fmt.Sprintf("%d ms", took[len(took)/2].Milliseconds())
			p99 = fmt.Sprintf("%d ms", took[len(took)*99/100].Milliseconds())
		}
		fmt.Printf("  %-50s %8.0f%% %10s %12s %14d %14d %19d ms\n", p.name, 100*float64(answered)/float64(during), median, p99,
			mostWaiting.Load(), calls, back.Milliseconds())
	}
}
