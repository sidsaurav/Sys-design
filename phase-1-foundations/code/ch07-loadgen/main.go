// Chapter 1.7 — the same server, measured two ways, giving two very
// different answers.
//
// Run:
//
//	go run ./phase-1-foundations/code/ch07-loadgen
//
// The server answers in about 1 ms, except that every 3 seconds it freezes
// for 300 ms (think: a garbage-collection pause, a lock, a noisy neighbour).
// So it is frozen 10% of the time.
//
// Test 1 is a "closed-loop" load test: 10 simulated users, each of which
// waits for its answer before sending its next request.
//
// Test 2 is an "open-loop" load test: requests are sent on a fixed schedule,
// 1,000 per second, whether or not earlier ones have been answered — the way
// independent real users behave.
//
// The program takes about 25 seconds.
package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"time"
)

const (
	testDuration = 12 * time.Second
	targetRate   = 1000 // requests per second, in both tests

	stallEvery = 3 * time.Second
	stallFor   = 300 * time.Millisecond
)

// newStallingServer returns a server that is fast except during its stalls.
func newStallingServer() *httptest.Server {
	var freeze sync.RWMutex

	// Every stallEvery, hold the lock for stallFor. While it is held,
	// every request waits.
	go func() {
		time.Sleep(stallEvery / 2)
		for {
			freeze.Lock()
			time.Sleep(stallFor)
			freeze.Unlock()
			time.Sleep(stallEvery - stallFor)
		}
	}()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		freeze.RLock() // blocks while the server is frozen
		freeze.RUnlock()
		time.Sleep(time.Millisecond) // the real work
		io.WriteString(w, "ok")
	}))
}

// newClient returns a client that keeps enough idle connections to reuse
// them under load (Chapter 1.2).
func newClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2000,
			MaxIdleConnsPerHost: 2000,
		},
	}
}

func get(client *http.Client, url string) {
	resp, err := client.Get(url)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// closedLoop simulates `users` users. Each sends a request, WAITS for the
// answer, and only then sends the next, pacing itself to hit the target
// rate. When the server freezes, every user is stuck on one request — and
// sends nothing else until it thaws.
func closedLoop(url string, users int) []time.Duration {
	client := newClient()
	perUserInterval := time.Second * time.Duration(users) / targetRate

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		latencies []time.Duration
	)
	deadline := time.Now().Add(testDuration)
	for range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := time.Now()
			for time.Now().Before(deadline) {
				time.Sleep(time.Until(next)) // pace this user to its share of the rate
				start := time.Now()
				get(client, url)
				took := time.Since(start)
				mu.Lock()
				latencies = append(latencies, took)
				mu.Unlock()

				next = next.Add(perUserInterval)
				if now := time.Now(); next.Before(now) {
					// The answer came late. This user does not send the
					// requests it "missed"; it just carries on from now.
					next = now
				}
			}
		}()
	}
	wg.Wait()
	return latencies
}

// openLoop sends requests on a fixed schedule: one every 1/targetRate
// seconds, no matter what happened to the earlier ones. Latency is measured
// from the moment each request was SCHEDULED to be sent.
func openLoop(url string) []time.Duration {
	client := newClient()
	interval := time.Second / targetRate

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		latencies []time.Duration
	)
	begin := time.Now()
	for i := 0; time.Duration(i)*interval < testDuration; i++ {
		scheduled := begin.Add(time.Duration(i) * interval)
		time.Sleep(time.Until(scheduled))
		wg.Add(1)
		go func() {
			defer wg.Done()
			get(client, url)
			took := time.Since(scheduled)
			mu.Lock()
			latencies = append(latencies, took)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return latencies
}

func report(name string, latencies []time.Duration) {
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var sum time.Duration
	slow := 0
	for _, l := range latencies {
		sum += l
		if l > 50*time.Millisecond {
			slow++
		}
	}
	fmt.Println(name)
	fmt.Printf("  requests sent: %d (the plan was %d)\n", len(latencies), targetRate*int(testDuration/time.Second))
	fmt.Printf("  mean %s   p50 %s   p90 %s   p99 %s   p99.9 %s   max %s\n",
		ms(sum/time.Duration(len(latencies))), ms(pct(latencies, 50)), ms(pct(latencies, 90)),
		ms(pct(latencies, 99)), ms(pct(latencies, 99.9)), ms(latencies[len(latencies)-1]))
	fmt.Printf("  requests slower than 50 ms: %d (%.1f%%)\n\n", slow, float64(slow)/float64(len(latencies))*100)
}

func main() {
	fmt.Printf("A server that answers in ~1 ms but freezes for %v every %v (%.0f%% of the time).\n",
		stallFor, stallEvery, float64(stallFor)/float64(stallEvery)*100)
	fmt.Printf("Both tests aim for %d requests per second for %v.\n\n", targetRate, testDuration)

	srv := newStallingServer()
	report("Test 1 — closed loop: 10 users, each waits for its answer before sending again",
		closedLoop(srv.URL, 10))
	srv.Close()

	srv = newStallingServer()
	report("Test 2 — open loop: requests sent on schedule, whether or not earlier ones came back",
		openLoop(srv.URL))
	srv.Close()
}

func pct(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%dms", d.Round(time.Millisecond).Milliseconds())
}
