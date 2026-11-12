// Chapter 7.4 — one request through nine services, and what a trace shows that nine dashboards do not.
//
// Nine real HTTP services in one process. Each records a span for every request
// it serves, and passes the identity of the trace to the services it calls, in a
// traceparent header. The tracer is written out here in about a hundred lines,
// so that nothing about it is hidden. In production you would use OpenTelemetry.
//
//	part 1  nine dashboards: each service's own view of its latency
//	part 2  one slow request, as a trace
//	part 3  every trace, added up: where the time went
//	part 4  one service forgets to pass the header on
//	part 5  sampling: what is kept, and what is lost
//
//	go run ./phase-7-observability/code/ch04-tracing
//	go run ./phase-7-observability/code/ch04-tracing -discounts parallel
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ the tracer

// A span is one piece of work, in one service, for one request.
type span struct {
	trace, id, parent string // the trace it belongs to, its own ID, and the span that caused it
	service, name     string
	start, end        time.Time
	failed            bool
}

func (s span) lasted() time.Duration { return s.end.Sub(s.start) }

// The tracer is where finished spans go. In production it is a collector, on another machine.
type tracer struct {
	mu    sync.Mutex
	spans []span
	next  atomic.Uint64
}

// current is what a request carries with it from span to span: the context.
type current struct{ trace, span string }

type key struct{}

func (t *tracer) newID(bytes int) string {
	sum := sha256.Sum256([]byte(fmt.Sprint("id-", t.next.Add(1))))
	return hex.EncodeToString(sum[:bytes])
}

// begin starts a span. If the context holds a span, the new one is its child, in the same
// trace. If it holds none, the new span is the root of a new trace.
func (t *tracer) begin(ctx context.Context, service, name, newTrace string) (context.Context, *span) {
	s := &span{id: t.newID(8), service: service, name: name, start: time.Now()}
	if c, ok := ctx.Value(key{}).(current); ok {
		s.trace, s.parent = c.trace, c.span
	} else if s.trace = newTrace; s.trace == "" {
		s.trace = t.newID(16)
	}
	return context.WithValue(ctx, key{}, current{trace: s.trace, span: s.id}), s
}

func (t *tracer) finish(s *span, failed bool) {
	s.end, s.failed = time.Now(), failed
	t.mu.Lock()
	t.spans = append(t.spans, *s)
	t.mu.Unlock()
}

// inject writes the context into the headers of an outgoing request, in the W3C format:
// version, trace ID, the caller's span ID, flags. The 01 says that this trace is being recorded.
func inject(ctx context.Context, h http.Header) {
	if c, ok := ctx.Value(key{}).(current); ok {
		h.Set("traceparent", "00-"+c.trace+"-"+c.span+"-01")
	}
}

// extract reads it back on the other side of the network.
func extract(ctx context.Context, h http.Header) context.Context {
	parts := strings.Split(h.Get("traceparent"), "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return ctx // no header, or not one that we understand: a new trace begins here
	}
	return context.WithValue(ctx, key{}, current{trace: parts[1], span: parts[2]})
}

// ------------------------------------------------------------ the services

type mesh struct {
	t       *tracer
	url     map[string]string
	client  *http.Client
	mode    string          // how pricing asks for discounts: serial, parallel or batch
	forgets map[string]bool // services that do not pass the header on
}

// serve wraps the work of a service. It continues the caller's trace, or begins one, and records a span.
func (m *mesh) serve(service string, work func(ctx context.Context, q url.Values) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := extract(r.Context(), r.Header)
		ctx, s := m.t.begin(ctx, service, r.URL.Path, r.URL.Query().Get("trace"))
		err := work(ctx, r.URL.Query())
		m.t.finish(s, err != nil)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
		}
	})
}

// call makes a request from one service to another, and takes the context with it.
func (m *mesh) call(ctx context.Context, from, to, path string, q url.Values) error {
	q = url.Values{"customer": q["customer"], "items": q["items"]}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url[to]+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if !m.forgets[from] {
		inject(ctx, req.Header)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%s answered %d", to, resp.StatusCode)
	}
	return nil
}

func pause(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func number(q url.Values, name string) int {
	n, _ := strconv.Atoi(q.Get(name))
	return n
}

// items is how many different products a customer's cart holds. One customer in a hundred
// is a wholesaler, with forty.
func items(customer int) int {
	if customer%100 == 7 {
		return 40
	}
	return 1 + customer%3
}

func newMesh(mode string, forgets ...string) (*mesh, func()) {
	m := &mesh{t: &tracer{}, url: map[string]string{}, mode: mode, forgets: map[string]bool{},
		client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200}}}
	for _, s := range forgets {
		m.forgets[s] = true
	}
	work := map[string]func(ctx context.Context, q url.Values) error{
		"gateway": func(ctx context.Context, q url.Values) error {
			pause(2) // check the session
			return m.call(ctx, "gateway", "checkout", "/checkout", q)
		},
		"checkout": func(ctx context.Context, q url.Values) error {
			pause(3)
			if err := m.call(ctx, "checkout", "cart", "/cart", q); err != nil {
				return err
			}
			// The price and the stock do not depend on each other, so ask for both at once.
			errs := make(chan error, 2)
			go func() { errs <- m.call(ctx, "checkout", "pricing", "/price", q) }()
			go func() { errs <- m.call(ctx, "checkout", "inventory", "/reserve", q) }()
			for i := 0; i < 2; i++ {
				if err := <-errs; err != nil {
					return err
				}
			}
			return m.call(ctx, "checkout", "payments", "/charge", q)
		},
		"cart": func(ctx context.Context, q url.Values) error { pause(8); return nil },
		"pricing": func(ctx context.Context, q url.Values) error {
			pause(4)
			if err := m.call(ctx, "pricing", "catalog", "/products", q); err != nil {
				return err
			}
			n := items(number(q, "customer"))
			switch m.mode {
			case "parallel": // every product's discount at once
				errs := make(chan error, n)
				for i := 0; i < n; i++ {
					go func() { errs <- m.call(ctx, "pricing", "discounts", "/discount", q) }()
				}
				for i := 0; i < n; i++ {
					if err := <-errs; err != nil {
						return err
					}
				}
			case "batch": // one request, for all the products
				q = url.Values{"customer": q["customer"], "items": {strconv.Itoa(n)}}
				return m.call(ctx, "pricing", "discounts", "/discounts", q)
			default: // one product after another: written when a cart had two things in it
				for i := 0; i < n; i++ {
					if err := m.call(ctx, "pricing", "discounts", "/discount", q); err != nil {
						return err
					}
				}
			}
			return nil
		},
		"catalog": func(ctx context.Context, q url.Values) error { pause(10); return nil },
		"discounts": func(ctx context.Context, q url.Values) error {
			pause(12 + number(q, "items")/4) // a batch costs a little more for each product
			return nil
		},
		"inventory": func(ctx context.Context, q url.Values) error {
			if number(q, "customer")%100 == 31 { // these customers' stock rows are locked by a slow job
				pause(300)
			}
			pause(15)
			return nil
		},
		"payments": func(ctx context.Context, q url.Values) error {
			pause(2)
			if err := m.call(ctx, "payments", "fraud", "/score", q); err != nil {
				return err
			}
			// Not every span is a call to another service. This one is work inside payments.
			_, s := m.t.begin(ctx, "payments", "ask the bank", "")
			pause(30)
			m.t.finish(s, false)
			return nil
		},
		"fraud": func(ctx context.Context, q url.Values) error {
			pause(20)
			if number(q, "customer")%100 == 55 { // the model has no score for these customers
				return fmt.Errorf("no score")
			}
			return nil
		},
	}
	var servers []*httptest.Server
	for name, w := range work {
		s := httptest.NewServer(m.serve(name, w))
		m.url[name] = s.URL
		servers = append(servers, s)
	}
	return m, func() {
		for _, s := range servers {
			s.Close()
		}
	}
}

// traceOf gives request n its trace ID. In production the ID is random. Here it comes from
// the request's number, so that every run samples the same requests in part 5.
func traceOf(n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprint("trace-", n)))
	return hex.EncodeToString(sum[:16])
}

// load sends this many checkouts through the gateway, forty at a time.
func (m *mesh) load(requests int) {
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < 40; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				q := url.Values{"customer": {strconv.Itoa(n % 500)}, "trace": {traceOf(n)}}
				if resp, err := m.client.Get(m.url["gateway"] + "/checkout?" + q.Encode()); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}
	for n := 0; n < requests; n++ {
		jobs <- n
	}
	close(jobs)
	wg.Wait()
}

// ------------------------------------------------------------ reading the spans

var order = []string{"gateway", "checkout", "cart", "pricing", "catalog", "discounts", "inventory", "payments", "fraud"}

func ms(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }

// A trace is the spans that share a trace ID, with the one that has no parent among them as the root.
type trace struct {
	spans []span
	root  span
}

func traces(spans []span) map[string]*trace {
	byID := map[string]*trace{}
	for _, s := range spans {
		if byID[s.trace] == nil {
			byID[s.trace] = &trace{}
		}
		byID[s.trace].spans = append(byID[s.trace].spans, s)
	}
	for _, t := range byID {
		ids := map[string]bool{}
		for _, s := range t.spans {
			ids[s.id] = true
		}
		sort.Slice(t.spans, func(i, j int) bool { return t.spans[i].start.Before(t.spans[j].start) })
		for _, s := range t.spans {
			if !ids[s.parent] {
				t.root = s
				break
			}
		}
	}
	return byID
}

// own is the time that a span spent on its own work: its length, less the time during which
// at least one of its children was running.
func (t *trace) own(s span) time.Duration {
	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge
	for _, c := range t.spans {
		if c.parent == s.id {
			edges = append(edges, edge{c.start, 1}, edge{c.end, -1})
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].at.Before(edges[j].at) })
	covered, running := time.Duration(0), 0
	var since time.Time
	for _, e := range edges {
		if running == 0 {
			since = e.at
		}
		if running += e.delta; running == 0 {
			covered += e.at.Sub(since)
		}
	}
	return s.lasted() - covered
}

func quantile(sorted []time.Duration, q float64) time.Duration {
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

// ------------------------------------------------------------ part 1

func part1(spans []span) {
	fmt.Println("PART 1 — nine dashboards. Each service times the requests that it serves, and knows nothing else.")
	fmt.Println()
	fmt.Printf("  %-12s %10s %10s %18s\n", "service", "requests", "median", "99th percentile")
	by := map[string][]time.Duration{}
	for _, s := range spans {
		if strings.HasPrefix(s.name, "/") {
			by[s.service] = append(by[s.service], s.lasted())
		}
	}
	for _, name := range order {
		d := by[name]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		fmt.Printf("  %-12s %10d %10s %18s\n", name, len(d), ms(quantile(d, 0.5)), ms(quantile(d, 0.99)))
	}
}

// ------------------------------------------------------------ part 2

func waterfall(t *trace) {
	// Put the spans in the order of the tree: each span, then its children in order of starting.
	children := map[string][]span{}
	for _, s := range t.spans {
		if s.id != t.root.id {
			children[s.parent] = append(children[s.parent], s)
		}
	}
	type row struct {
		span
		depth int
	}
	var rows []row
	var walk func(s span, depth int)
	walk = func(s span, depth int) {
		rows = append(rows, row{s, depth})
		for _, c := range children[s.id] {
			walk(c, depth+1)
		}
	}
	walk(t.root, 0)

	const width = 44
	scale := func(d time.Duration) int { return int(float64(d) / float64(t.root.lasted()) * width) }
	show := func(r row) {
		mark := ""
		if r.failed {
			mark = "  FAILED"
		}
		fmt.Printf("  %-22s %-14s %8s %8s  %s%s%s\n", strings.Repeat("  ", r.depth)+r.service, r.name, ms(r.start.Sub(t.root.start)), ms(r.lasted()),
			strings.Repeat(" ", scale(r.start.Sub(t.root.start))), strings.Repeat("█", max(1, scale(r.lasted()))), mark)
	}
	fmt.Printf("  %-22s %-14s %8s %8s\n", "service", "work", "starts", "lasts")
	for i := 0; i < len(rows); i++ {
		// Many calls of the same kind in a row are shown as the first two and a count.
		last := i
		for last+1 < len(rows) && rows[last+1].parent == rows[i].parent && rows[last+1].name == rows[i].name && rows[last+1].service == rows[i].service {
			last++
		}
		show(rows[i])
		if last-i >= 3 {
			show(rows[i+1])
			how := "each beginning as the one before it ends"
			if rows[i+2].start.Before(rows[i+1].end) {
				how = "all at the same time"
			}
			fmt.Printf("  %-22s … and %d more of the same, %s\n", strings.Repeat("  ", rows[i].depth), last-i-1, how)
			i = last
		}
	}
}

func part2(all map[string]*trace) {
	t := all[traceOf(1007)] // request 1007 is customer 7's third checkout
	fmt.Printf("PART 2 — one slow request, as a trace. Customer 7's checkout took %s and made %d spans.\n", ms(t.root.lasted()), len(t.spans))
	fmt.Println()
	for _, s := range t.spans {
		if s.service == "pricing" && s.parent != "" {
			fmt.Printf("  the header on the request from checkout to pricing:\n  traceparent: 00-%s-%s-01\n\n", s.trace, s.parent)
			break
		}
	}
	waterfall(t)
	for _, s := range t.spans {
		if s.service == "pricing" {
			fmt.Printf("\n  Of pricing's %s, %s was work of its own.\n", ms(s.lasted()), ms(t.own(s)))
		}
	}
}

// ------------------------------------------------------------ part 3

func part3(all map[string]*trace) {
	var whole []*trace
	for _, t := range all {
		if t.root.service == "gateway" {
			whole = append(whole, t)
		}
	}
	sort.Slice(whole, func(i, j int) bool { return whole[i].root.lasted() > whole[j].root.lasted() })
	slow := whole[:len(whole)/50]
	fmt.Printf("PART 3 — every trace, added up. Whose own work was the time? All %d requests, and the slowest %d.\n", len(whole), len(slow))
	fmt.Println()
	type sums struct {
		own   map[string]time.Duration
		calls map[string]int
		total time.Duration
	}
	add := func(ts []*trace) sums {
		s := sums{own: map[string]time.Duration{}, calls: map[string]int{}}
		for _, t := range ts {
			for _, sp := range t.spans {
				own := t.own(sp)
				s.own[sp.service] += own
				s.total += own
				if strings.HasPrefix(sp.name, "/") {
					s.calls[sp.service]++
				}
			}
		}
		return s
	}
	a, b := add(whole), add(slow)
	fmt.Printf("  %-12s %16s %14s %8s %16s %14s\n", "", "all: calls in", "share of the", "", "slowest: calls", "share of the")
	fmt.Printf("  %-12s %16s %14s %8s %16s %14s\n", "service", "each request", "time", "", "in each request", "time")
	for _, name := range order {
		fmt.Printf("  %-12s %16.1f %13.0f%% %8s %16.1f %13.0f%%\n", name, float64(a.calls[name])/float64(len(whole)), 100*float64(a.own[name])/float64(a.total),
			"", float64(b.calls[name])/float64(len(slow)), 100*float64(b.own[name])/float64(b.total))
	}
	// For each slow request: which service's own work was the largest part of it?
	blame, calls := map[string]int{}, map[string]int{}
	for _, t := range slow {
		own := map[string]time.Duration{}
		n := map[string]int{}
		for _, sp := range t.spans {
			own[sp.service] += t.own(sp)
			if strings.HasPrefix(sp.name, "/") {
				n[sp.service]++
			}
		}
		worst := order[0]
		for _, name := range order {
			if own[name] > own[worst] {
				worst = name
			}
		}
		blame[worst]++
		calls[worst] += n[worst]
	}
	fmt.Println()
	fmt.Printf("  The slowest %d, one at a time: where most of each one's time went.\n", len(slow))
	for _, name := range order {
		if blame[name] > 0 {
			plural := map[bool]string{true: "", false: "s"}
			each := calls[name] / blame[name]
			fmt.Printf("    %2d request%-1s  %-10s called %d time%s in each\n", blame[name], plural[blame[name] == 1], name, each, plural[each == 1])
		}
	}
}

// ------------------------------------------------------------ part 4

func part4(mode string) {
	m, stop := newMesh(mode, "pricing")
	m.load(100)
	stop()
	all := traces(m.t.spans)
	whole, orphans, orphanSpans := 0, 0, 0
	for _, t := range all {
		if t.root.service == "gateway" {
			whole++
		} else {
			orphans++
			orphanSpans += len(t.spans)
		}
	}
	t := all[traceOf(7)]
	fmt.Println("PART 4 — the same services, but pricing does not pass the header on to the services that it calls.")
	fmt.Println()
	fmt.Printf("  100 requests made %d traces: %d that begin at the gateway, and %d others, of %d spans, that begin nowhere.\n", len(all), whole, orphans, orphanSpans)
	fmt.Printf("  Customer 7's checkout again: %s, and now %d spans.\n\n", ms(t.root.lasted()), len(t.spans))
	waterfall(t)
	for _, s := range t.spans {
		if s.service == "pricing" {
			fmt.Printf("\n  By this trace, pricing spent %s on work of its own.\n", ms(t.own(s)))
		}
	}
}

// ------------------------------------------------------------ part 5

func part5(all map[string]*trace) {
	fmt.Println("PART 5 — sampling. The traces of part 1, kept three ways.")
	fmt.Println()
	// Head: decided when the trace begins, from its ID alone, so every service decides alike.
	head := func(id string) bool {
		n, _ := strconv.ParseUint(id[:8], 16, 32)
		return n%50 == 0
	}
	slowAt := 300 * time.Millisecond
	policies := []struct {
		name string
		keep func(t *trace) bool
	}{
		{"keep everything", func(t *trace) bool { return true }},
		{"head: 1 trace in 50, chosen at the start", func(t *trace) bool { return head(t.root.trace) }},
		{"tail: every slow or failed trace, and 1 in 50 of the rest", func(t *trace) bool {
			return t.root.failed || t.root.lasted() >= slowAt || head(t.root.trace)
		}},
	}
	fmt.Printf("  %-60s %8s %8s %14s %14s\n", "", "traces", "spans", "of the slow", "of the failed")
	fmt.Printf("  %-60s %8s %8s %14s %14s\n", "", "kept", "stored", "requests", "requests")
	for _, p := range policies {
		kept, spans, slow, slowKept, failed, failedKept := 0, 0, 0, 0, 0, 0
		for _, t := range all {
			isSlow, isFailed, keep := t.root.lasted() >= slowAt, t.root.failed, p.keep(t)
			if isSlow {
				slow++
			}
			if isFailed {
				failed++
			}
			if keep {
				kept++
				spans += len(t.spans)
				if isSlow {
					slowKept++
				}
				if isFailed {
					failedKept++
				}
			}
		}
		fmt.Printf("  %-60s %8d %8d %14s %14s\n", p.name, kept, spans, fmt.Sprintf("%d of %d", slowKept, slow), fmt.Sprintf("%d of %d", failedKept, failed))
	}
}

func main() {
	mode := flag.String("discounts", "serial", "how pricing asks for discounts: serial, parallel or batch")
	flag.Parse()
	m, stop := newMesh(*mode)
	m.load(1500)
	stop()
	all := traces(m.t.spans)
	part1(m.t.spans)
	fmt.Println()
	part2(all)
	fmt.Println()
	part3(all)
	fmt.Println()
	part4(*mode)
	fmt.Println()
	part5(all)
}
