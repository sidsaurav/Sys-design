// Chapter 6.7 — a game day in half a minute: break a dependency on purpose, and see whether a prediction holds.
//
// A shop, as real HTTP servers inside this process. It has 20 workers and two pages:
//
//	/cart      needs nothing outside the shop
//	/checkout  needs the payments service, and shows recommendations if it can get them
//
// 100 users a second ask for each page, and wait one second. Recommendations is
// protected as in Chapter 6.3: a wait of 50 ms and at most 2 calls at a time.
//
// Each experiment states a hypothesis, measures the steady state, injects a fault
// for two seconds, and measures again. A guard removes the fault early if users
// are being hurt badly. Then the shop is changed, and the experiments that
// disproved their hypotheses are run again.
//
//	go run ./phase-6-reliability/code/ch07-gameday
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ dependencies that can be broken

type fault int32

const (
	none    fault = iota
	slow          // takes 2 s to answer
	failing       // answers at once, with an error
)

// A dependency is a service that normally answers in 5 ms, and into which a fault can be injected.
type dependency struct {
	fault atomic.Int32
	*httptest.Server
}

func newDependency() *dependency {
	d := &dependency{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		takes := 5 * time.Millisecond
		switch fault(d.fault.Load()) {
		case failing:
			w.WriteHeader(http.StatusInternalServerError)
			return
		case slow:
			takes = 2 * time.Second
		}
		select {
		case <-time.After(takes):
		case <-r.Context().Done():
		}
	}))
	return d
}

// ------------------------------------------------------------ the shop

var client = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1000}}

// call makes a request with a limit on its time and, if places is not nil, on how many are in progress.
func call(ctx context.Context, url string, wait time.Duration, places chan struct{}) bool {
	if places != nil {
		select {
		case places <- struct{}{}:
			defer func() { <-places }()
		default:
			return false
		}
	}
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type shop struct {
	payments, recommendations *dependency
	protectPayments           bool // the change made after the first round
	*httptest.Server
}

func newShop(protectPayments bool) *shop {
	s := &shop{payments: newDependency(), recommendations: newDependency(), protectPayments: protectPayments}
	workers := make(chan struct{}, 20)
	recPlaces, payPlaces := make(chan struct{}, 2), make(chan struct{}, 6)
	worker := func(r *http.Request) bool {
		select {
		case workers <- struct{}{}:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cart", func(w http.ResponseWriter, r *http.Request) {
		if !worker(r) {
			return
		}
		defer func() { <-workers }()
		time.Sleep(5 * time.Millisecond)
	})
	mux.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		if !worker(r) {
			return
		}
		defer func() { <-workers }()
		paid := false
		if s.protectPayments {
			paid = call(r.Context(), s.payments.URL, 100*time.Millisecond, payPlaces)
		} else {
			paid = call(r.Context(), s.payments.URL, 0, nil) // waits for as long as the user does
		}
		if !paid {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		call(r.Context(), s.recommendations.URL, 50*time.Millisecond, recPlaces) // wanted, not needed
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *shop) close() {
	s.Server.Close()
	s.payments.Close()
	s.recommendations.Close()
}

// ------------------------------------------------------------ an experiment

type result struct {
	page        string
	start, took time.Duration
	ok          bool // answered with 200
	answered    bool // answered at all, yes or no
}

// A measure is the steady state being watched: the share of some requests that meet some standard.
type measure struct {
	name string
	page string
	good func(r result) bool
}

var (
	pageInTime   = func(r result) bool { return r.ok && r.took <= 200*time.Millisecond }
	answerInTime = func(r result) bool { return r.answered && r.took <= 200*time.Millisecond }
)

type experiment struct {
	title      string
	hypothesis string
	inject     func(s *shop, f fault)
	fault      fault
	watch      measure
}

const (
	before  = 1 * time.Second // the fault is injected at this moment
	lasting = 2 * time.Second // and removed after this long, unless the guard removes it first
	total   = 4 * time.Second
)

func (m measure) share(results []result, from, to time.Duration) (float64, int) {
	good, n := 0, 0
	for _, r := range results {
		if r.page == m.page && r.start >= from && r.start < to {
			n++
			if m.good(r) {
				good++
			}
		}
	}
	if n == 0 {
		return 100, 0
	}
	return 100 * float64(good) / float64(n), n
}

// run carries out one experiment and reports whether the hypothesis survived.
func (e experiment) run(protectPayments bool) bool {
	s := newShop(protectPayments)
	defer s.close()
	var (
		mu      sync.Mutex
		results []result // one for every request, from the moment it is sent
		wg      sync.WaitGroup
		removed = before + lasting // when the fault was removed
		stopped bool
		over    = make(chan struct{})
	)
	begin := time.Now()
	snapshot := func() []result {
		mu.Lock()
		defer mu.Unlock()
		return append([]result(nil), results...)
	}
	go func() { // the experimenter
		defer close(over)
		time.Sleep(before)
		e.inject(s, e.fault)
		for time.Since(begin) < before+lasting {
			time.Sleep(250 * time.Millisecond)
			// The guard: look at the requests that have had their 200 ms. If fewer than half are good, stop.
			now := time.Since(begin)
			if share, n := e.watch.share(snapshot(), now-450*time.Millisecond, now-200*time.Millisecond); n > 0 && share < 50 && now-450*time.Millisecond >= before {
				stopped = true
				break
			}
		}
		e.inject(s, none)
		removed = time.Since(begin)
	}()
	pages := []string{"/cart", "/checkout"}
	for i, tick := 0, time.NewTicker(5*time.Millisecond); time.Since(begin) < total; i++ { // 100 users a second for each page
		<-tick.C
		wg.Add(1)
		go func(page string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			res := result{page: page, start: time.Since(begin)}
			mu.Lock()
			results = append(results, res) // not answered yet
			n := len(results) - 1
			mu.Unlock()
			req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+page, nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				res.answered, res.ok = true, resp.StatusCode == http.StatusOK
			}
			res.took = time.Since(begin) - res.start
			mu.Lock()
			results[n] = res
			mu.Unlock()
		}(pages[i%2])
	}
	wg.Wait()
	<-over

	all := snapshot()
	steady, _ := e.watch.share(all, 0, before)
	during, _ := e.watch.share(all, before, removed)
	after, _ := e.watch.share(all, removed+time.Second/2, total)
	held := during >= 99
	fmt.Printf("  %s\n", e.title)
	fmt.Printf("    hypothesis: %s\n", e.hypothesis)
	fmt.Printf("    %-22s %5.1f%%\n", "before the fault", steady)
	note := ""
	if stopped {
		note = fmt.Sprintf("   the guard removed the fault after %.1f s", (removed - before).Seconds())
	}
	fmt.Printf("    %-22s %5.1f%%%s\n", "during it", during, note)
	fmt.Printf("    %-22s %5.1f%%\n", "after it", after)
	if held {
		fmt.Printf("    the hypothesis held\n\n")
	} else {
		fmt.Printf("    DISPROVED\n\n")
	}
	return held
}

func main() {
	onRecommendations := func(s *shop, f fault) { s.recommendations.fault.Store(int32(f)) }
	onPayments := func(s *shop, f fault) { s.payments.fault.Store(int32(f)) }
	checkoutPages := measure{"checkout pages served within 200 ms", "/checkout", pageInTime}
	cartPages := measure{"cart pages served within 200 ms", "/cart", pageInTime}
	checkoutAnswers := measure{"checkout requests answered, yes or no, within 200 ms", "/checkout", answerInTime}
	experiments := []experiment{
		{"1. Recommendations takes 2 s to answer.", "checkout does not notice: 99% of checkout pages are served within 200 ms", onRecommendations, slow, checkoutPages},
		{"2. Recommendations answers every request with an error.", "checkout does not notice: 99% of checkout pages are served within 200 ms", onRecommendations, failing, checkoutPages},
		{"3. Payments takes 2 s to answer.", "the cart does not notice: 99% of cart pages are served within 200 ms", onPayments, slow, cartPages},
		{"4. Payments takes 2 s to answer.", "checkout fails quickly: 99% of checkout requests are answered, yes or no, within 200 ms", onPayments, slow, checkoutAnswers},
	}
	fmt.Println("A shop with 20 workers. 100 users a second ask for the cart, and 100 for the checkout. Each experiment:")
	fmt.Println("one second of normal running, a fault for two seconds, and a second and a half to recover.")
	fmt.Println()
	fmt.Println("ROUND 1 — the shop as it is")
	fmt.Println()
	var again []experiment
	for _, e := range experiments {
		if !e.run(false) {
			again = append(again, e)
		}
	}
	fmt.Println("ROUND 2 — after a change: a call to payments may take 100 ms and no longer, and at most 6 may be in progress")
	fmt.Println()
	for _, e := range again {
		e.run(true)
	}
}
