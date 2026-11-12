// Chapter 3.2 — a load balancer, and four ways for it to let you down.
//
// Everything runs inside this one process, on local ports: instances of a
// small service, a load balancer in front of them, and a load generator.
//
//	part 1  one instance becomes ten times slower: round robin against least connections
//	part 2  one instance hangs: no health check, a TCP check, an HTTP check
//	part 3  a rolling restart under load: abrupt, and graceful
//	part 4  long-lived connections: balancing connections against balancing requests
//
// Run:
//
//	go run ./phase-3-scaling-out/code/ch02-loadbalancer            # all four parts, under a minute
//	go run ./phase-3-scaling-out/code/ch02-loadbalancer -part 2
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ an instance of the service

// A backend is one instance. It can work on eight requests at once; the
// rest wait their turn. Its behaviour can be changed while it runs.
type backend struct {
	id      int
	addr    string
	srv     *http.Server
	workers chan struct{}
	work    atomic.Int64 // how long one request takes, in nanoseconds
	hung    atomic.Bool  // accept requests and never answer them
	ready   atomic.Bool  // what the health endpoint reports
	served  atomic.Int64
}

func newBackend(id int, work time.Duration) *backend {
	b := &backend{id: id, workers: make(chan struct{}, 8)}
	b.work.Store(int64(work))
	b.ready.Store(true)
	b.start()
	return b
}

func (b *backend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if b.hung.Load() {
			<-r.Context().Done()
		} else if !b.ready.Load() {
			http.Error(w, "going away", http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if b.hung.Load() {
			<-r.Context().Done()
			return
		}
		select {
		case b.workers <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		time.Sleep(time.Duration(b.work.Load()))
		<-b.workers
		b.served.Add(1)
		fmt.Fprint(w, b.id)
	})
	return mux
}

// start begins listening: on a fresh port the first time, on the same
// port after a restart.
func (b *backend) start() {
	address := b.addr
	if address == "" {
		address = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	b.addr = ln.Addr().String()
	b.srv = &http.Server{Handler: b.handler()}
	go b.srv.Serve(ln)
}

// ------------------------------------------------------------ the load balancer (layer 7)

type target struct {
	b           *backend
	proxy       *httputil.ReverseProxy
	healthy     atomic.Bool
	outstanding atomic.Int64 // requests sent to it and not yet answered
	fails       int
	passes      int
}

// A balancer receives every request and forwards it to one target.
type balancer struct {
	mu      sync.RWMutex
	targets []*target
	policy  string // "round robin" or "least connections"
	check   string // "none", "tcp" or "http"
	next    atomic.Uint64
	addr    string
	stop    chan struct{}
}

func newBalancer(policy, check string, backends ...*backend) *balancer {
	lb := &balancer{policy: policy, check: check, stop: make(chan struct{})}
	for _, b := range backends {
		lb.add(b)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	lb.addr = "http://" + ln.Addr().String()
	go http.Serve(ln, lb)
	if check != "none" {
		go lb.checkHealth()
	}
	return lb
}

func (lb *balancer) add(b *backend) {
	u, _ := url.Parse("http://" + b.addr)
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext,
		ResponseHeaderTimeout: time.Second, // the balancer's own timeout for an instance
		MaxIdleConnsPerHost:   200,
	}
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusBadGateway)
	}
	t := &target{b: b, proxy: proxy}
	t.healthy.Store(true)
	lb.mu.Lock()
	lb.targets = append(lb.targets, t)
	lb.mu.Unlock()
}

// pick chooses the target for the next request.
func (lb *balancer) pick() *target {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	var candidates []*target
	for _, t := range lb.targets {
		if t.healthy.Load() {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		candidates = lb.targets // nothing looks healthy: better to try than to refuse everyone
	}
	if lb.policy == "least connections" {
		// Keep only the targets with the fewest requests outstanding,
		// then take turns among those.
		fewest := candidates[0].outstanding.Load()
		for _, t := range candidates {
			fewest = min(fewest, t.outstanding.Load())
		}
		var least []*target
		for _, t := range candidates {
			if t.outstanding.Load() == fewest {
				least = append(least, t)
			}
		}
		candidates = least
	}
	return candidates[lb.next.Add(1)%uint64(len(candidates))]
}

func (lb *balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := lb.pick()
	t.outstanding.Add(1)
	defer t.outstanding.Add(-1)
	t.proxy.ServeHTTP(w, r)
}

// checkHealth probes every target ten times a second. Two failures in a
// row take a target out; two successes in a row bring it back.
func (lb *balancer) checkHealth() {
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for {
		select {
		case <-lb.stop:
			return
		case <-time.After(100 * time.Millisecond):
		}
		lb.mu.RLock()
		targets := append([]*target(nil), lb.targets...)
		lb.mu.RUnlock()
		for _, t := range targets {
			ok := false
			switch lb.check {
			case "tcp": // can a connection be opened?
				if conn, err := net.DialTimeout("tcp", t.b.addr, 100*time.Millisecond); err == nil {
					conn.Close()
					ok = true
				}
			case "http": // does the service answer a request?
				if resp, err := client.Get("http://" + t.b.addr + "/healthz"); err == nil {
					resp.Body.Close()
					ok = resp.StatusCode == http.StatusOK
				}
			}
			if ok {
				t.passes, t.fails = t.passes+1, 0
				if t.passes >= 2 {
					t.healthy.Store(true)
				}
			} else {
				t.fails, t.passes = t.fails+1, 0
				if t.fails >= 2 {
					t.healthy.Store(false)
				}
			}
		}
	}
}

func (lb *balancer) target(b *backend) *target {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	for _, t := range lb.targets {
		if t.b == b {
			return t
		}
	}
	return nil
}

// ------------------------------------------------------------ a layer-4 balancer

// tcpBalancer forwards connections, not requests. It chooses a backend
// when a connection is opened and then copies bytes in both directions
// without looking at them. Every request on that connection goes to the
// same backend.
type tcpBalancer struct {
	mu       sync.Mutex
	backends []*backend
	next     int
	addr     string
}

func newTCPBalancer(backends ...*backend) *tcpBalancer {
	lb := &tcpBalancer{backends: backends}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	lb.addr = "http://" + ln.Addr().String()
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			lb.mu.Lock()
			b := lb.backends[lb.next%len(lb.backends)]
			lb.next++
			lb.mu.Unlock()
			server, err := net.Dial("tcp", b.addr)
			if err != nil {
				client.Close()
				continue
			}
			go func() { io.Copy(server, client); server.Close() }()
			go func() { io.Copy(client, server); client.Close() }()
		}
	}()
	return lb
}

func (lb *tcpBalancer) add(b *backend) {
	lb.mu.Lock()
	lb.backends = append(lb.backends, b)
	lb.mu.Unlock()
}

// ------------------------------------------------------------ load

type outcome struct {
	ok, failed int
	latencies  []time.Duration
}

func (o *outcome) percentile(p float64) time.Duration {
	if len(o.latencies) == 0 {
		return 0
	}
	sort.Slice(o.latencies, func(i, j int) bool { return o.latencies[i] < o.latencies[j] })
	return o.latencies[int(p*float64(len(o.latencies)-1))]
}

// load sends requests at a fixed rate, whatever the answers do (an open
// loop: Chapter 1.7), and waits for them all.
func load(address string, rate int, d time.Duration) *outcome {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 500}}
	var out outcome
	var mu sync.Mutex
	var wg sync.WaitGroup
	begin := time.Now()
	for i := 0; i < int(d.Seconds()*float64(rate)); i++ {
		time.Sleep(time.Until(begin.Add(time.Duration(i) * time.Second / time.Duration(rate))))
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			resp, err := client.Get(address)
			good := err == nil && resp.StatusCode == http.StatusOK
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			mu.Lock()
			defer mu.Unlock()
			if good {
				out.ok++
				out.latencies = append(out.latencies, time.Since(start))
			} else {
				out.failed++
			}
		}()
	}
	wg.Wait()
	return &out
}

func ms(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

func shares(backends []*backend) string {
	var total int64
	for _, b := range backends {
		total += b.served.Load()
	}
	parts := make([]string, len(backends))
	for i, b := range backends {
		parts[i] = fmt.Sprintf("%d%%", (b.served.Load()*100+total/2)/max(total, 1))
	}
	return strings.Join(parts, " / ")
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — three instances; the third has become ten times slower (100 ms a request instead of 10)")
	fmt.Println("         450 requests a second for 5 seconds; the balancer gives an instance 1 second to answer")
	fmt.Println()
	fmt.Printf("  %-20s %8s %8s %9s %9s   %s\n", "policy", "ok", "failed", "p50", "p99", "share answered by each instance")
	for _, policy := range []string{"round robin", "least connections"} {
		backends := []*backend{newBackend(1, 10*time.Millisecond), newBackend(2, 10*time.Millisecond), newBackend(3, 100*time.Millisecond)}
		lb := newBalancer(policy, "none", backends...)
		out := load(lb.addr, 450, 5*time.Second)
		fmt.Printf("  %-20s %8d %8d %9s %9s   %s\n", policy, out.ok, out.failed, ms(out.percentile(0.50)), ms(out.percentile(0.99)), shares(backends))
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — three healthy instances; one second in, the third hangs: it accepts requests and never answers")
	fmt.Println("         300 requests a second for 5 seconds; the balancer gives an instance 1 second to answer")
	fmt.Println()
	fmt.Printf("  %-34s %8s %8s   %s\n", "health check", "ok", "failed", "the hung instance stopped receiving requests after")
	names := map[string]string{
		"none": "none",
		"tcp":  "can a connection be opened?",
		"http": "does GET /healthz answer?",
	}
	for _, check := range []string{"none", "tcp", "http"} {
		backends := []*backend{newBackend(1, 10*time.Millisecond), newBackend(2, 10*time.Millisecond), newBackend(3, 10*time.Millisecond)}
		lb := newBalancer("round robin", check, backends...)
		noticed := "never"
		go func() {
			time.Sleep(time.Second)
			backends[2].hung.Store(true)
			hungAt := time.Now()
			for time.Since(hungAt) < 4*time.Second {
				if !lb.target(backends[2]).healthy.Load() {
					noticed = ms(time.Since(hungAt))
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		out := load(lb.addr, 300, 5*time.Second)
		fmt.Printf("  %-34s %8d %8d   %s\n", names[check], out.ok, out.failed, noticed)
		close(lb.stop)
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — a deploy: each of three instances is restarted in turn, under 200 requests a second")
	fmt.Println("         (a request takes 50 ms; the balancer checks health ten times a second)")
	fmt.Println()
	fmt.Printf("  %-58s %8s %8s\n", "how an instance is stopped", "ok", "failed")
	for _, graceful := range []bool{false, true} {
		backends := []*backend{newBackend(1, 50*time.Millisecond), newBackend(2, 50*time.Millisecond), newBackend(3, 50*time.Millisecond)}
		lb := newBalancer("round robin", "http", backends...)
		go func() {
			time.Sleep(time.Second)
			for _, b := range backends {
				if graceful {
					// 1. Say "do not send me more", and keep serving.
					b.ready.Store(false)
					// 2. Wait until the balancer has noticed.
					for lb.target(b).healthy.Load() {
						time.Sleep(10 * time.Millisecond)
					}
					// 3. Finish the requests already in progress, then stop.
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					b.srv.Shutdown(ctx)
					cancel()
				} else {
					b.srv.Close() // stop now: open connections are cut
				}
				time.Sleep(300 * time.Millisecond) // the new version starts up
				b.start()
				b.ready.Store(true)
				for !lb.target(b).healthy.Load() { // wait until it is back in rotation
					time.Sleep(10 * time.Millisecond)
				}
				time.Sleep(300 * time.Millisecond)
			}
		}()
		out := load(lb.addr, 200, 6*time.Second)
		name := "killed at once"
		if graceful {
			name = "fails its health check, waits, finishes its requests, stops"
		}
		fmt.Printf("  %-58s %8d %8d\n", name, out.ok, out.failed)
		close(lb.stop)
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 4

// keepAliveClients starts nine clients, each holding one connection open
// and sending requests down it one after another. Three of them are busy;
// six pause between requests. They run until stop is closed.
func keepAliveClients(address string, stop chan struct{}) *sync.WaitGroup {
	var wg sync.WaitGroup
	for c := 0; c < 9; c++ {
		wg.Add(1)
		pause := 40 * time.Millisecond
		if c%3 == 0 {
			pause = 0 // a busy client
		}
		go func() {
			defer wg.Done()
			client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
			for {
				select {
				case <-stop:
					client.CloseIdleConnections()
					return
				default:
				}
				if resp, err := client.Get(address); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				time.Sleep(pause)
			}
		}()
		time.Sleep(5 * time.Millisecond) // connect in order
	}
	return &wg
}

func part4() {
	fmt.Println("PART 4 — nine clients, each with one long-lived connection; three of them are busy")
	fmt.Println("         after two seconds a fourth instance is added")
	fmt.Println()
	fmt.Printf("  %-44s %-34s %s\n", "the balancer forwards", "requests per instance, first 2 s", "next 2 s, with a fourth instance")
	for _, layer := range []string{"connections (layer 4)", "requests (layer 7)"} {
		backends := []*backend{newBackend(1, 5*time.Millisecond), newBackend(2, 5*time.Millisecond), newBackend(3, 5*time.Millisecond)}
		var address string
		var add func(*backend)
		if strings.HasPrefix(layer, "connections") {
			lb := newTCPBalancer(backends...)
			address, add = lb.addr, lb.add
		} else {
			lb := newBalancer("round robin", "none", backends...)
			address, add = lb.addr, lb.add
		}
		stop := make(chan struct{})
		clients := keepAliveClients(address, stop)
		time.Sleep(2 * time.Second)
		first := counts(backends)

		fourth := newBackend(4, 5*time.Millisecond)
		backends = append(backends, fourth)
		add(fourth)
		for _, b := range backends {
			b.served.Store(0)
		}
		time.Sleep(2 * time.Second)
		second := counts(backends)
		close(stop)
		clients.Wait()
		fmt.Printf("  %-44s %-34s %s\n", layer, first, second)
	}
}

func counts(backends []*backend) string {
	parts := make([]string, len(backends))
	for i, b := range backends {
		parts[i] = fmt.Sprint(b.served.Load())
	}
	return strings.Join(parts, " / ")
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3, part4} {
		if *part == 0 || *part == i+1 {
			run()
		}
	}
}
