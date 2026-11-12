// Chapter 6.4 — more arrives than the service can do.
//
// Parts 1 to 4 are a model, in steps of 1 ms: a service with 20 workers, each
// of which spends 100 ms on a request, so that it can finish 200 a second. A
// client waits one second for an answer. The model uses no network and no
// clock. Requests arrive at slightly irregular moments, from a fixed seed, so
// it prints the same numbers on every run.
//
//	part 1  what to do with the excess: queue it, drop what has expired, or refuse it at the door
//	part 2  one client sends twenty times its share: a limit for each client
//	part 3  three kinds of request and room for two of them: which to refuse
//	part 4  the service slows to less than half its speed: a limit on the rate, and a limit on requests in progress
//	part 5  real HTTP: the limit on requests in progress, as a piece of Go middleware
//
//	go run ./phase-6-reliability/code/ch04-overload
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"time"
)

// ------------------------------------------------------------ two limiters

// A bucket holds up to burst tokens and gains rate tokens a second. A request takes one.
// With now from the clock it limits real traffic; here it is given the model's time.
type bucket struct {
	rate, burst  float64
	tokens, last float64
}

func (b *bucket) allow(now float64) bool {
	b.tokens = min(b.burst, b.tokens+(now-b.last)*b.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// limit lets at most n requests into a handler at once, and refuses the rest at once.
func limit(n int, next http.Handler) http.Handler {
	places := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case places <- struct{}{}:
			defer func() { <-places }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too busy", http.StatusServiceUnavailable)
		}
	})
}

// ------------------------------------------------------------ the model

const (
	workers  = 20
	patience = 1000 // ms that a client waits
)

type request struct {
	arrived int // ms
	stream  int
}

type stream struct {
	name      string
	perSecond float64
}

type model struct {
	streams []stream
	seconds int
	work    func(now int) int                       // ms that a request takes, if a worker starts it now
	admit   func(now int, r request, m *model) bool // the door. nil: everything comes in
	drop    int                                     // at the front of the queue, drop a request: 1 if its client has gone; 2 if there is no longer time to do it
	from    int                                     // measure requests that arrive from this second on…
	to      int                                     // …and before this one

	queue   []request
	sent    []int // by stream, within the measured seconds
	good    []int // answered within the client's patience
	refused []int // told no at the door
	waits   []int // ms from arrival to answer, for the good ones
}

func (m *model) run() *model {
	n := len(m.streams)
	m.sent, m.good, m.refused = make([]int, n), make([]int, n), make([]int, n)
	if m.work == nil {
		m.work = func(int) int { return 100 }
	}
	if m.to == 0 {
		m.from, m.to = 20, m.seconds-2
	}
	freeAt := make([]int, workers)
	rng := rand.New(rand.NewSource(6))
	var arriving []int
	next := make([]float64, n) // by stream: when its next request arrives
	for i := range next {
		next[i] = rng.Float64() * 1000 / m.streams[i].perSecond
	}
	for now := 0; now < m.seconds*1000; now++ {
		arriving = arriving[:0] // which streams send a request in this millisecond, in a random order
		for i, s := range m.streams {
			for ; next[i] <= float64(now); next[i] += (0.5 + rng.Float64()) * 1000 / s.perSecond {
				arriving = append(arriving, i)
			}
		}
		rng.Shuffle(len(arriving), func(i, j int) { arriving[i], arriving[j] = arriving[j], arriving[i] })
		for _, i := range arriving {
			r := request{arrived: now, stream: i}
			measured := now >= m.from*1000 && now < m.to*1000
			if measured {
				m.sent[i]++
			}
			if m.admit != nil && !m.admit(now, r, m) {
				if measured {
					m.refused[i]++
				}
				continue
			}
			m.queue = append(m.queue, r)
		}
		for w := range freeAt {
			for freeAt[w] <= now && len(m.queue) > 0 {
				r := m.queue[0]
				m.queue = m.queue[1:]
				if m.drop == 1 && now > r.arrived+patience {
					continue // its client has gone
				}
				if m.drop == 2 && now+m.work(now) > r.arrived+patience {
					continue // it could be started, but not finished in time
				}
				freeAt[w] = now + m.work(now)
				if freeAt[w] <= r.arrived+patience && r.arrived >= m.from*1000 && r.arrived < m.to*1000 {
					m.good[r.stream]++
					m.waits = append(m.waits, freeAt[w]-r.arrived)
				}
			}
		}
	}
	sort.Ints(m.waits)
	return m
}

func (m *model) perSecond(counts []int, stream int) float64 {
	return float64(counts[stream]) / float64(m.to-m.from)
}

// room is the door that most parts use: come in if fewer than n requests are waiting.
func room(n int) func(int, request, *model) bool {
	return func(_ int, _ request, m *model) bool { return len(m.queue) < n }
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — a service that can finish 200 requests a second, and is sent more. A client waits 1 s.")
	fmt.Println()
	loads := []float64{100, 180, 220, 300, 600}
	policies := []struct {
		name string
		make func() *model
	}{
		{"every request waits its turn", func() *model { return &model{} }},
		{"…and one whose client has gone is dropped unstarted", func() *model { return &model{drop: 1} }},
		{"…and so is one that can no longer be finished in time", func() *model { return &model{drop: 2} }},
		{"at most 20 may wait; the rest are refused at once", func() *model { return &model{admit: room(20)} }},
	}
	for _, table := range []string{"answered in time, a second", "the median answer took"} {
		fmt.Printf("  %-54s", table)
		for _, l := range loads {
			fmt.Printf(" %9s", fmt.Sprintf("%.0f /s", l))
		}
		fmt.Println(" sent")
		for _, p := range policies {
			fmt.Printf("  %-54s", p.name)
			for _, l := range loads {
				m := p.make()
				m.streams, m.seconds = []stream{{"all", l}}, 60
				m.run()
				switch {
				case table[0] == 'a':
					fmt.Printf(" %9.0f", m.perSecond(m.good, 0))
				case m.perSecond(m.good, 0) < 1:
					fmt.Printf(" %9s", "—")
				default:
					fmt.Printf(" %6d ms", m.waits[len(m.waits)/2])
				}
			}
			fmt.Println()
		}
		fmt.Println()
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — three clients. Two send 50 requests a second each. The third sends 1,000.")
	fmt.Println()
	fmt.Printf("  %-58s %22s %22s %22s\n", "answered in time, a second (and as a share of what was sent)", "client A, 50 /s", "client B, 50 /s", "client C, 1,000 /s")
	streams := []stream{{"A", 50}, {"B", 50}, {"C", 1000}}
	for _, perClient := range []float64{0, 80} {
		buckets := []*bucket{{rate: perClient, burst: 20}, {rate: perClient, burst: 20}, {rate: perClient, burst: 20}}
		m := &model{streams: streams, seconds: 60, admit: func(now int, r request, m *model) bool {
			if perClient > 0 && !buckets[r.stream].allow(float64(now)/1000) {
				return false
			}
			return len(m.queue) < 20
		}}
		m.run()
		name := "one limit for everybody: at most 20 waiting"
		if perClient > 0 {
			name = "…and each client may send 80 a second, and no more"
		}
		fmt.Printf("  %-58s", name)
		for i := range streams {
			fmt.Printf(" %22s", fmt.Sprintf("%.0f  (%.0f%%)", m.perSecond(m.good, i), 100*float64(m.good[i])/float64(m.sent[i])))
		}
		fmt.Println()
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — 300 requests a second for a service that can do 200: 50 payments, 110 page views, and 140 from a")
	fmt.Println("         crawler.")
	fmt.Println()
	fmt.Printf("  %-58s %22s %22s %22s\n", "answered in time, a second (and as a share of what was sent)", "payments, 50 /s", "page views, 110 /s", "crawler, 140 /s")
	streams := []stream{{"payments", 50}, {"page views", 110}, {"crawler", 140}}
	for _, byKind := range []bool{false, true} {
		limits := []int{20, 20, 20} // how many may be waiting when a request of each kind is let in
		name := "whoever arrives when 20 are waiting is refused"
		if byKind {
			limits = []int{20, 10, 1}
			name = "the crawler is refused first, then page views, then payments"
		}
		m := &model{streams: streams, seconds: 60, admit: func(_ int, r request, m *model) bool {
			return len(m.queue) < limits[r.stream]
		}}
		m.run()
		fmt.Printf("  %-58s", name)
		for i := range streams {
			fmt.Printf(" %22s", fmt.Sprintf("%.0f  (%.0f%%)", m.perSecond(m.good, i), 100*float64(m.good[i])/float64(m.sent[i])))
		}
		fmt.Println()
	}
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — the service is sent 180 requests a second. At second 30, something it depends on slows down, and")
	fmt.Println("         each request takes a worker 250 ms where it took 100: the service can now finish 80 a second.")
	fmt.Println()
	fmt.Printf("  %-58s %22s %22s %22s\n", "", "answered in time,", "answered in time,", "refused at once,")
	fmt.Printf("  %-58s %22s %22s %22s\n", "", "a second, before", "a second, after", "a second, after")
	slow := func(now int) int {
		if now >= 30000 {
			return 250
		}
		return 100
	}
	for _, inProgress := range []bool{false, true} {
		b := &bucket{rate: 200, burst: 20}
		name := "a limit on the rate: 200 a second may come in"
		admit := func(now int, _ request, _ *model) bool { return b.allow(float64(now) / 1000) }
		if inProgress {
			name = "a limit on requests in progress: 20 working, 20 waiting"
			admit = room(20)
		}
		before := (&model{streams: []stream{{"all", 180}}, seconds: 90, work: slow, admit: admit, from: 5, to: 28}).run()
		b = &bucket{rate: 200, burst: 20}
		after := (&model{streams: []stream{{"all", 180}}, seconds: 90, work: slow, admit: admit, from: 50, to: 88}).run()
		fmt.Printf("  %-58s %22.0f %22.0f %22.0f\n", name, before.perSecond(before.good, 0), after.perSecond(after.good, 0), after.perSecond(after.refused, 0))
	}
}

// ------------------------------------------------------------ part 5

func part5() {
	fmt.Println("PART 5 — real HTTP. A handler that takes 100 ms, behind limit(10, …). 100 requests arrive together.")
	fmt.Println()
	server := httptest.NewServer(limit(10, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	})))
	defer server.Close()
	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		served, refused int
		retryAfter      string
	)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(server.URL)
			if err != nil {
				return
			}
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if resp.StatusCode == http.StatusOK {
				served++
				return
			}
			refused++
			retryAfter = resp.Header.Get("Retry-After")
		}()
	}
	wg.Wait()
	fmt.Printf("  served: %d\n", served)
	fmt.Printf("  refused: %d, with status 503 and the header Retry-After: %s\n", refused, retryAfter)
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 5)")
	flag.Parse()
	for i, f := range []func(){part1, part2, part3, part4, part5} {
		if *part == 0 || *part == i+1 {
			f()
			fmt.Println()
		}
	}
}
