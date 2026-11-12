// Chapter 6.2 — timeouts and retries when there are many layers and many clients.
//
//	part 1  three services in a chain, real HTTP: who retries, and what the last one sees
//	part 2  the same chain, and a queue: a timeout at each hop, a context passed on, a deadline passed on
//	part 3  a model: Chapter 6.1's 30-second stall again, with backoff, a retry budget, and deadlines
//	part 4  a model: a page that needs 100 servers, with and without a second call to the slow ones
//
// Parts 1 and 2 start HTTP servers inside this process, on this machine.
// Parts 3 and 4 use no network and no clock, and print the same numbers every run.
//
//	go run ./phase-6-reliability/code/ch02-deadlines
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ a retry budget

// A budget allows retries only while they are a small share of all requests.
// Every request adds a fraction of a token. Every retry spends a whole one.
type budget struct {
	mu     sync.Mutex
	tokens float64
	ratio  float64 // 0.1: one retry for every ten requests
	max    float64 // how many retries may be saved up
}

func (b *budget) request() {
	b.mu.Lock()
	b.tokens = min(b.max, b.tokens+b.ratio)
	b.mu.Unlock()
}

func (b *budget) retry() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ------------------------------------------------------------ part 1: who retries

const noRetry = "X-Do-Not-Retry" // on a failure: "I have tried, or it is pointless. Do not send this again."

// A caller makes a request to the service below it, up to three times.
type caller struct {
	below  string  // the URL of the service below
	signal bool    // sends and obeys the do-not-retry header
	budget *budget // if set, a retry needs a token
}

// call returns whether the request succeeded and, if it did not, whether the caller above may try again.
func (c *caller) call() (ok, retryable bool) {
	if c.budget != nil {
		c.budget.request()
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 && c.budget != nil && !c.budget.retry() {
			break
		}
		resp, err := http.Get(c.below)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true, false
		}
		if c.signal && resp.Header.Get(noRetry) != "" {
			return false, false
		}
	}
	return false, !c.signal // this layer has done the retrying: with the signal, it tells the layer above not to
}

func (c *caller) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	ok, retryable := c.call()
	if ok {
		return
	}
	if !retryable {
		w.Header().Set(noRetry, "1")
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func part1() {
	fmt.Println("PART 1 — a user's request passes through three services: edge, orders, stock. Stock is failing every")
	fmt.Println("         request. 1,000 users make a request. Every caller is willing to try three times.")
	fmt.Println()
	fmt.Printf("  %-66s %14s %14s\n", "", "requests that", "for each")
	fmt.Printf("  %-66s %14s %14s\n", "", "reached stock", "user request")
	for _, v := range []struct {
		name   string
		signal bool
		budget bool
	}{
		{"every layer retries", false, false},
		{"a layer that has retried tells the layers above not to", true, false},
		{"…and may retry only one request in ten", true, true},
	} {
		var reached atomic.Int64
		stock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		ordersCaller := &caller{below: stock.URL, signal: v.signal}
		if v.budget {
			ordersCaller.budget = &budget{tokens: 10, ratio: 0.1, max: 10}
		}
		orders := httptest.NewServer(ordersCaller)
		edge := httptest.NewServer(&caller{below: orders.URL, signal: v.signal})
		user := &caller{below: edge.URL, signal: v.signal}
		var wg sync.WaitGroup
		for u := 0; u < 8; u++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 125; i++ {
					user.call()
				}
			}()
		}
		wg.Wait()
		edge.Close()
		orders.Close()
		stock.Close()
		fmt.Printf("  %-66s %14d %14.1f\n", v.name, reached.Load(), float64(reached.Load())/1000)
	}
}

// ------------------------------------------------------------ part 2: a deadline that travels

// The header that carries the deadline: how much longer the caller will wait, in milliseconds.
// It is a duration and not a clock reading, because two machines' clocks need not agree (Chapter 5.5).
const timeLeft = "X-Time-Left-Ms"

const (
	job      = 100 * time.Millisecond // stock does one job at a time, and a job takes this long
	patience = 350 * time.Millisecond // how long a user waits
)

type mode int

const (
	ownTimeouts  mode = iota // each hop makes a new context, with a timeout of its own
	passContext              // each hop passes on the context of the request it is serving
	passDeadline             // …and the deadline, in a header
)

// withDeadline gives a handler a context that ends when the request's deadline does.
func withDeadline(r *http.Request) (context.Context, context.CancelFunc, time.Time) {
	if ms, err := strconv.ParseInt(r.Header.Get(timeLeft), 10, 64); err == nil {
		at := time.Now().Add(time.Duration(ms) * time.Millisecond)
		ctx, cancel := context.WithDeadline(r.Context(), at)
		return ctx, cancel, at
	}
	return r.Context(), func() {}, time.Time{}
}

// forward is a middle service: it calls the service below and passes the answer up.
func forward(below string, m mode, held *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() { held.Add(int64(time.Since(start))) }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // "two seconds should be plenty"
		switch m {
		case passContext:
			ctx, cancel = r.Context(), func() {} // ends when the caller goes away
		case passDeadline:
			ctx, cancel, _ = withDeadline(r) // …or at the deadline it sent, whichever is first
		}
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", below, nil)
		if at, ok := ctx.Deadline(); ok && m == passDeadline {
			req.Header.Set(timeLeft, strconv.FormatInt(time.Until(at).Milliseconds(), 10))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
	})
}

func part2() {
	fmt.Println("PART 2 — the same three services. Stock does one job at a time, and a job takes 100 ms. Ten users ask at")
	fmt.Println("         the same moment, and each waits 350 ms.")
	fmt.Println()
	fmt.Printf("  %-42s %9s %16s %14s %14s %18s\n", "", "users", "the others", "stock worked", "of which", "edge and orders")
	fmt.Printf("  %-42s %9s %16s %14s %14s %18s\n", "", "answered", "were told after", "for", "for nobody", "were held for")
	for _, v := range []struct {
		name string
		m    mode
	}{
		{"each hop has a timeout of its own", ownTimeouts},
		{"each hop passes its context on", passContext},
		{"each hop passes the deadline on", passDeadline},
	} {
		var (
			queue                   = make(chan struct{}, 1) // one job at a time
			ahead                   atomic.Int64             // jobs waiting or being done
			worked, wasted, held    atomic.Int64
			gaveUp                  time.Time // when the users stop waiting
			stockDone, othersWaited sync.WaitGroup
		)
		stock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stockDone.Add(1)
			defer stockDone.Done()
			ctx, cancel, deadline := withDeadline(r)
			defer cancel()
			// With a deadline, stock can see that a job would finish too late, and say so at once.
			if !deadline.IsZero() && time.Until(deadline) < time.Duration(ahead.Load()+1)*job {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			ahead.Add(1)
			defer ahead.Add(-1)
			select {
			case queue <- struct{}{}:
				defer func() { <-queue }()
			case <-ctx.Done(): // whoever was waiting for this has gone: leave the queue
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			for step := 0; step < 10; step++ { // the job, in ten steps
				select {
				case <-time.After(job / 10):
					worked.Add(int64(job / 10))
					if time.Now().After(gaveUp) {
						wasted.Add(int64(job / 10))
					}
				case <-ctx.Done():
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
		}))
		orders := httptest.NewServer(forward(stock.URL, v.m, &held))
		edge := httptest.NewServer(forward(orders.URL, v.m, &held))

		start := time.Now()
		gaveUp = start.Add(patience)
		var answered atomic.Int64
		var told []time.Duration
		var mu sync.Mutex
		for u := 0; u < 10; u++ {
			othersWaited.Add(1)
			go func() {
				defer othersWaited.Done()
				ctx, cancel := context.WithDeadline(context.Background(), gaveUp)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "GET", edge.URL, nil)
				req.Header.Set(timeLeft, strconv.FormatInt(time.Until(gaveUp).Milliseconds(), 10))
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						answered.Add(1)
						return
					}
				}
				mu.Lock()
				told = append(told, time.Since(start))
				mu.Unlock()
			}()
			time.Sleep(time.Millisecond) // so that they arrive in order
		}
		othersWaited.Wait()
		time.Sleep(50 * time.Millisecond)
		stockDone.Wait() // let stock finish whatever it is still doing
		edge.Close()
		orders.Close()
		stock.Close()
		sort.Slice(told, func(i, j int) bool { return told[i] < told[j] })
		fmt.Printf("  %-42s %9d %13d ms %11d ms %11d ms %15d ms\n", v.name, answered.Load(),
			told[len(told)/2].Milliseconds(), time.Duration(worked.Load()).Milliseconds(),
			time.Duration(wasted.Load()).Milliseconds(), time.Duration(held.Load()).Milliseconds())
	}
}

// ------------------------------------------------------------ part 3: the stall, again

const hz = 100 // steps in a second

type attempt struct {
	user, n  int
	deadline int
	answered bool
}

type policy struct {
	name     string
	backoff  bool    // wait before trying again: 100 to 200 ms, then 200 to 400 ms
	budget   float64 // retries allowed for each request made (0: as many as there are failures)
	deadline bool    // the service knows each request's deadline, and does not work on one that has passed
}

// stall runs Chapter 6.1's part 3: a service that can finish 1,000 requests a second is sent 800, and
// finishes nothing from second 30 to second 60. Clients wait one second and try up to three times.
func stall(p policy) (recovered int, attempts, late int) {
	const seconds, load, each = 360, 800, 1000
	rng := rand.New(rand.NewSource(1))
	steps := (seconds + 5) * hz
	expiring := make([][]*attempt, steps+2*hz)
	later := make([][]*attempt, steps+2*hz) // retries that are waiting out a backoff
	made, good := make([]int, seconds+5), make([]int, seconds+5)
	var (
		done   []bool
		when   []int
		queue  []*attempt
		tokens = 100.0
	)
	send := func(now int, a *attempt) {
		a.deadline = now + hz
		expiring[a.deadline] = append(expiring[a.deadline], a)
		queue = append(queue, a)
		attempts++
	}
	for now := 0; now < steps; now++ {
		if now < seconds*hz {
			for i := 0; i < load/hz; i++ {
				done, when = append(done, false), append(when, now/hz)
				made[now/hz]++
				tokens = min(100, tokens+p.budget)
				send(now, &attempt{user: len(done) - 1, n: 1})
			}
		}
		for _, a := range later[now] {
			if !done[a.user] {
				send(now, a)
			}
		}
		if now < 30*hz || now >= 60*hz {
			for finished := 0; finished < each/hz && len(queue) > 0; {
				a := queue[0]
				queue = queue[1:]
				if now > a.deadline {
					if p.deadline {
						continue // its client has gone: throw it away, unstarted
					}
					late++
				} else if a.answered = true; !done[a.user] {
					done[a.user] = true
					good[when[a.user]]++
				}
				finished++
			}
		}
		for _, a := range expiring[now] {
			if a.answered || done[a.user] || a.n == 3 {
				continue
			}
			if p.budget > 0 {
				if tokens < 1 {
					continue // no budget for a retry: the request fails
				}
				tokens--
			}
			next, wait := &attempt{user: a.user, n: a.n + 1}, 0
			if p.backoff {
				wait = (10 + rng.Intn(11)) << (a.n - 1) // 100–200 ms, then 200–400 ms
			}
			later[now+wait] = append(later[now+wait], next)
			if wait == 0 {
				send(now, next)
				later[now] = later[now][:len(later[now])-1]
			}
		}
		expiring[now], later[now] = nil, nil
	}
	recovered = -1
	for s := seconds - 1; s >= 60 && good[s]*100 >= made[s]*99; s-- {
		recovered = s - 60
	}
	return recovered, attempts, late
}

func part3() {
	fmt.Println("PART 3 — Chapter 6.1's stall, as a model: a service that can finish 1,000 requests a second is sent 800,")
	fmt.Println("         and finishes nothing from second 30 to second 60. Clients wait 1 s and try up to three times.")
	fmt.Println()
	fmt.Printf("  %-58s %22s %16s %20s\n", "", "once the stall ends,", "attempts sent", "finished too late")
	fmt.Printf("  %-58s %22s %16s %20s\n", "", "answers 99% again", "", "to matter")
	for _, p := range []policy{
		{name: "retries at once"},
		{name: "retries after a backoff, with jitter", backoff: true},
		{name: "retries limited to one for every ten requests", budget: 0.1},
		{name: "retries at once; the service drops what has expired", deadline: true},
		{name: "a retry budget, and a service that drops what has expired", budget: 0.1, deadline: true},
	} {
		recovered, attempts, late := stall(p)
		after := "never"
		if recovered >= 0 {
			after = fmt.Sprintf("after %d s", recovered)
			if recovered == 0 {
				after = "at once"
			}
		}
		fmt.Printf("  %-58s %22s %16d %20d\n", p.name, after, attempts, late)
	}
}

// ------------------------------------------------------------ part 4: a second call to the slow ones

func part4() {
	fmt.Println("PART 4 — a model: a page needs answers from 100 servers. 99 calls in 100 take 8 to 12 ms. One in 100")
	fmt.Println("         takes a second. 10,000 pages.")
	fmt.Println()
	fmt.Printf("  %-52s %14s %12s %18s %14s\n", "", "pages slower", "median", "99th percentile", "extra calls")
	fmt.Printf("  %-52s %14s %12s %18s %14s\n", "", "than 100 ms", "page", "", "")
	for _, hedgeAfter := range []int{0, 20} {
		rng := rand.New(rand.NewSource(4))
		call := func() int {
			if rng.Intn(100) == 0 {
				return 1000
			}
			return 8 + rng.Intn(5)
		}
		var pages []int
		slow, calls, extra := 0, 0, 0
		for p := 0; p < 10000; p++ {
			page := 0
			for s := 0; s < 100; s++ {
				t := call()
				calls++
				if hedgeAfter > 0 && t > hedgeAfter { // no answer yet: ask another server, and take whichever answers first
					t = min(t, hedgeAfter+call())
					extra++
				}
				page = max(page, t)
			}
			if page > 100 {
				slow++
			}
			pages = append(pages, page)
		}
		sort.Ints(pages)
		name, more := "wait for every server", "—"
		if hedgeAfter > 0 {
			name, more = fmt.Sprintf("after %d ms with no answer, ask a second server", hedgeAfter), fmt.Sprintf("%.1f%%", 100*float64(extra)/float64(calls))
		}
		fmt.Printf("  %-52s %13.1f%% %9d ms %15d ms %14s\n", name, float64(slow)/100, pages[len(pages)/2], pages[len(pages)*99/100], more)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	flag.Parse()
	for i, f := range []func(){part1, part2, part3, part4} {
		if *part == 0 || *part == i+1 {
			f()
			fmt.Println()
		}
	}
}
