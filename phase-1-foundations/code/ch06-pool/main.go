// Chapter 1.6 — where requests wait: pools, queues, and utilisation.
//
// Run:
//
//	go run ./phase-1-foundations/code/ch06-pool
//	go run ./phase-1-foundations/code/ch06-pool -part b -rate 40
//	go run ./phase-1-foundations/code/ch06-pool -part b -acquire-timeout 500ms
//
// Part A is a simulation of a server with a pool of 10 connections. It shows
// what happens to latency as the arrival rate approaches the pool's capacity,
// what happens beyond it, and what a bounded queue changes. It runs in under
// a second and prints the same numbers every time.
//
// Part B runs real goroutines against a real pool to show the most common
// way to shrink a pool's capacity by accident: holding a connection while
// waiting for something else. It takes about fifteen seconds.
package main

import (
	"container/heap"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

func main() {
	part := flag.String("part", "all", "which part to run: a, b or all")
	rate := flag.Int("rate", 100, "part B: requests per second")
	acquireTimeout := flag.Duration("acquire-timeout", 0, "part B: give up waiting for a connection after this long (0 = wait for ever)")
	flag.Parse()

	if *part == "a" || *part == "all" {
		partA()
	}
	if *part == "b" || *part == "all" {
		partB(*rate, *acquireTimeout)
	}
}

// ------------------------------------------------------------------ part A

const (
	poolSize    = 10
	meanService = 20 * time.Millisecond // how long a request holds a connection
)

// capacity is the most requests per second the pool can serve: each
// connection finishes 1/0.020 = 50 requests per second, and there are 10.
var capacity = float64(poolSize) / meanService.Seconds()

type result struct {
	latencies []time.Duration // of the requests that were served
	rejected  int
	inFlight  float64 // average number of requests in the server
	duration  time.Duration
	backlog   int // requests still waiting when the run ended
}

// simulate plays `seconds` of traffic arriving at `rate` requests per second
// through the pool. Requests arrive at random (Poisson arrivals) and hold a
// connection for a random time averaging meanService. If maxQueue >= 0, a
// request that finds maxQueue others already waiting is rejected at once.
func simulate(rate float64, seconds float64, maxQueue int, seed uint64) result {
	rng := rand.New(rand.NewPCG(seed, 99))
	exp := func(mean float64) float64 { return -mean * math.Log(1-rng.Float64()) }

	freeAt := make([]float64, poolSize) // when each connection is next free
	var inSystem finishTimes            // finish times of requests not yet done
	var waitingStarts []float64         // start times of requests still queued
	var res result
	var sumInFlight float64
	var arrivals int

	for now := exp(1 / rate); now < seconds; now += exp(1 / rate) {
		arrivals++

		// How many earlier requests are still in the server right now?
		for len(inSystem) > 0 && inSystem[0] <= now {
			heap.Pop(&inSystem)
		}
		sumInFlight += float64(len(inSystem))

		// How many are still waiting for a connection?
		for len(waitingStarts) > 0 && waitingStarts[0] <= now {
			waitingStarts = waitingStarts[1:]
		}
		if maxQueue >= 0 && len(waitingStarts) >= maxQueue {
			res.rejected++
			continue
		}

		// Take the connection that becomes free soonest.
		k := 0
		for i := range freeAt {
			if freeAt[i] < freeAt[k] {
				k = i
			}
		}
		start := math.Max(now, freeAt[k])
		finish := start + exp(meanService.Seconds())
		freeAt[k] = finish
		if start > now {
			waitingStarts = append(waitingStarts, start)
		}
		heap.Push(&inSystem, finish)
		res.latencies = append(res.latencies, time.Duration((finish-now)*float64(time.Second)))
	}

	for len(waitingStarts) > 0 && waitingStarts[0] <= seconds {
		waitingStarts = waitingStarts[1:]
	}
	res.backlog = len(waitingStarts)
	res.inFlight = sumInFlight / float64(arrivals)
	res.duration = time.Duration(seconds * float64(time.Second))
	sort.Slice(res.latencies, func(i, j int) bool { return res.latencies[i] < res.latencies[j] })
	return res
}

func partA() {
	fmt.Printf("PART A — a pool of %d connections; a request holds one for %v on average.\n", poolSize, meanService)
	fmt.Printf("Capacity = %d / %.3f s = %.0f requests per second.\n\n", poolSize, meanService.Seconds(), capacity)

	fmt.Println("1. Latency as the arrival rate approaches capacity")
	fmt.Println("   arrivals  utilisation   in flight   rate x mean latency     mean      p50      p99")
	for _, rate := range []float64{100, 250, 400, 450, 475, 490} {
		r := simulate(rate, 2000, -1, 1)
		mean := meanOf(r.latencies)
		fmt.Printf("   %4.0f/s     %3.0f%%        %6.1f       %6.1f              %6s   %6s   %6s\n",
			rate, rate/capacity*100, r.inFlight, rate*mean.Seconds(),
			ms(mean), ms(pct(r.latencies, 50)), ms(pct(r.latencies, 99)))
	}

	fmt.Println()
	fmt.Println("2. Beyond capacity: 550 requests per second for 60 seconds")
	r := simulate(550, 60, -1, 2)
	fmt.Printf("   unbounded queue:        served %5d   rejected %5d   p50 %6s   p99 %6s   still waiting at the end: %d\n",
		len(r.latencies), r.rejected, ms(pct(r.latencies, 50)), ms(pct(r.latencies, 99)), r.backlog)
	r = simulate(550, 60, 20, 2)
	fmt.Printf("   queue limited to 20:    served %5d   rejected %5d   p50 %6s   p99 %6s   still waiting at the end: %d\n",
		len(r.latencies), r.rejected, ms(pct(r.latencies, 50)), ms(pct(r.latencies, 99)), r.backlog)
	fmt.Println()
}

// finishTimes is a min-heap of times.
type finishTimes []float64

func (h finishTimes) Len() int           { return len(h) }
func (h finishTimes) Less(i, j int) bool { return h[i] < h[j] }
func (h finishTimes) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *finishTimes) Push(x any)        { *h = append(*h, x.(float64)) }
func (h *finishTimes) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// ------------------------------------------------------------------ part B

// pool is a fixed set of connections. acquire blocks until one is free.
// It keeps the same two counters that Go's database/sql exposes in DBStats:
// how many acquisitions had to wait, and for how long in total.
type pool struct {
	conns   chan struct{}
	timeout time.Duration // how long acquire may wait; 0 means for ever

	mu           sync.Mutex
	waitCount    int
	waitDuration time.Duration
}

var errPoolTimeout = errors.New("timed out waiting for a connection")

func newPool(size int, timeout time.Duration) *pool {
	p := &pool{conns: make(chan struct{}, size), timeout: timeout}
	for range size {
		p.conns <- struct{}{}
	}
	return p
}

func (p *pool) acquire() error {
	select {
	case <-p.conns: // a connection was free
		return nil
	default:
	}

	// None free: this goroutine now waits in line.
	var giveUp <-chan time.Time
	if p.timeout > 0 {
		timer := time.NewTimer(p.timeout)
		defer timer.Stop()
		giveUp = timer.C
	}
	start := time.Now()
	var err error
	select {
	case <-p.conns:
	case <-giveUp:
		err = errPoolTimeout
	}
	p.mu.Lock()
	p.waitCount++
	p.waitDuration += time.Since(start)
	p.mu.Unlock()
	return err
}

func (p *pool) release() { p.conns <- struct{}{} }

const (
	queryTime   = 5 * time.Millisecond   // one database query
	paymentTime = 200 * time.Millisecond // a call to an outside payment provider
)

// checkoutHoldingConnection keeps its database connection for the whole
// request, including the 200 ms spent waiting for another company's API.
func checkoutHoldingConnection(db *pool) error {
	if err := db.acquire(); err != nil {
		return err
	}
	defer db.release()
	time.Sleep(queryTime)   // read the cart
	time.Sleep(paymentTime) // call the payment provider — connection idle but held
	time.Sleep(queryTime)   // save the order
	return nil
}

// checkoutReleasingConnection gives the connection back before the slow call
// and takes one again afterwards.
func checkoutReleasingConnection(db *pool) error {
	if err := db.acquire(); err != nil {
		return err
	}
	time.Sleep(queryTime) // read the cart
	db.release()

	time.Sleep(paymentTime) // call the payment provider — holding nothing

	if err := db.acquire(); err != nil {
		return err
	}
	time.Sleep(queryTime) // save the order
	db.release()
	return nil
}

// drive sends requests at a fixed rate for a fixed time, whether or not
// earlier ones have finished. It returns how long each successful request
// took, and how many failed.
func drive(handler func(*pool) error, db *pool, rate int, d time.Duration) (latencies []time.Duration, failed int) {
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	interval := time.Second / time.Duration(rate)
	begin := time.Now()
	for i := 0; time.Duration(i)*interval < d; i++ {
		time.Sleep(time.Until(begin.Add(time.Duration(i) * interval)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			err := handler(db)
			took := time.Since(start)
			mu.Lock()
			if err != nil {
				failed++
			} else {
				latencies = append(latencies, took)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencies, failed
}

func partB(rate int, acquireTimeout time.Duration) {
	const duration = 5 * time.Second
	fmt.Printf("PART B — a checkout handler: query (%v), call a payment API (%v), query (%v).\n", queryTime, paymentTime, queryTime)
	fmt.Printf("Pool of %d connections, %d requests per second for %v. The work itself takes %v.\n",
		poolSize, rate, duration, 2*queryTime+paymentTime)
	if acquireTimeout > 0 {
		fmt.Printf("A request gives up if it cannot get a connection within %v.\n", acquireTimeout)
	}
	fmt.Println()

	for _, v := range []struct {
		name    string
		handler func(*pool) error
		held    time.Duration // how long one request keeps a connection
	}{
		{"Holding the connection during the payment call", checkoutHoldingConnection, 2*queryTime + paymentTime},
		{"Releasing the connection during the payment call", checkoutReleasingConnection, 2 * queryTime},
	} {
		db := newPool(poolSize, acquireTimeout)
		lat, failed := drive(v.handler, db, rate, duration)
		fmt.Println(v.name)
		fmt.Printf("  each request holds a connection for %v -> the pool can serve %.0f requests per second\n",
			v.held, float64(poolSize)/v.held.Seconds())
		fmt.Printf("  succeeded %d, failed %d   p50 %6s   p99 %6s   slowest %6s\n",
			len(lat), failed, ms(pct(lat, 50)), ms(pct(lat, 99)), ms(pct(lat, 100)))
		averageWait := time.Duration(0)
		if db.waitCount > 0 {
			averageWait = db.waitDuration / time.Duration(db.waitCount)
		}
		fmt.Printf("  pool: %d acquisitions had to wait, %s on average\n\n", db.waitCount, ms(averageWait))
	}
}

// ------------------------------------------------------------------ helpers

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

func meanOf(xs []time.Duration) time.Duration {
	var sum time.Duration
	for _, x := range xs {
		sum += x
	}
	return sum / time.Duration(len(xs))
}

func ms(d time.Duration) string {
	if d >= 10*time.Second {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d >= time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dms", d.Round(time.Millisecond).Milliseconds())
}
