package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	beat = 50 * time.Millisecond  // how often a copy tells the registry that it is alive
	ttl  = 300 * time.Millisecond // how long the registry believes a copy that has gone quiet
)

// The registry knows which addresses answer to a name. Here there is one name, "orders".
type registry struct {
	mu   sync.Mutex
	seen map[string]time.Time // address → when it last said that it was alive
}

func (r *registry) alive(addr string) {
	r.mu.Lock()
	r.seen[addr] = time.Now()
	r.mu.Unlock()
}

func (r *registry) remove(addr string) {
	r.mu.Lock()
	delete(r.seen, addr)
	r.mu.Unlock()
}

// lookup returns the addresses that have been heard from lately.
func (r *registry) lookup() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for addr, at := range r.seen {
		if time.Since(at) < ttl {
			out = append(out, addr)
		}
	}
	sort.Strings(out)
	return out
}

// A replica is one running copy of the service.
type replica struct {
	srv  *httptest.Server
	quit chan struct{}
	done chan struct{}
}

func start(reg *registry) *replica {
	c := &replica{quit: make(chan struct{}), done: make(chan struct{})}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	reg.alive(c.srv.URL)
	go func() { // the heartbeat
		defer close(c.done)
		t := time.NewTicker(beat)
		defer t.Stop()
		for {
			select {
			case <-c.quit:
				return
			case <-t.C:
				reg.alive(c.srv.URL)
			}
		}
	}()
	return c
}

// leave is a planned stop: say so, give callers time to hear of it, and then stop (Chapter 9.1).
func (c *replica) leave(reg *registry, drain time.Duration) {
	close(c.quit)
	<-c.done
	reg.remove(c.srv.URL)
	time.Sleep(drain)
	c.srv.Close()
}

// die is an unplanned stop. Nothing is said: the registry finds out when the heartbeats end.
func (c *replica) die() {
	close(c.quit)
	c.srv.CloseClientConnections()
	c.srv.Close()
}

// A caller is one way of finding the service.
type caller struct {
	what    string
	addrs   []string      // the addresses that it knows
	refresh time.Duration // how often it asks the registry again; 0 means never
	retry   bool          // whether it tries another copy when one does not answer
	asked   time.Time
	next    int
	failed  [2]atomic.Int64 // while the copies were replaced; after one died
}

// order returns the copies to try for the next request: one first, and then the others.
func (c *caller) order(reg *registry) []string {
	if c.refresh > 0 && time.Since(c.asked) >= c.refresh {
		c.addrs, c.asked = reg.lookup(), time.Now()
	}
	c.next++
	out := make([]string, 0, len(c.addrs))
	for i := range c.addrs {
		out = append(out, c.addrs[(c.next+i)%len(c.addrs)])
	}
	if !c.retry && len(out) > 1 {
		out = out[:1]
	}
	return out
}

const (
	requests = 3000 // one each millisecond
	replaced = 1800 // by this request every copy has been replaced
	dies     = 2000 // at this request one copy dies
)

func part1(drain time.Duration) {
	reg := &registry{seen: map[string]time.Time{}}
	copies := []*replica{start(reg), start(reg), start(reg)}
	callers := []*caller{
		{what: "an address written in its configuration", addrs: []string{copies[0].srv.URL}},
		{what: "the name, looked up once when it started", addrs: reg.lookup()},
		{what: "the name, looked up again every 100 ms", refresh: 100 * time.Millisecond},
		{what: "…and another copy tried when one does not answer", refresh: 100 * time.Millisecond, retry: true},
	}

	var wg sync.WaitGroup
	begin := time.Now()
	for i := 0; i < requests; i++ {
		time.Sleep(time.Until(begin.Add(time.Duration(i) * time.Millisecond)))
		switch i {
		case 320, 720, 1120: // a deploy: a new copy starts at a new address, and an old one leaves
			old := copies[i/400]
			copies[i/400] = start(reg)
			wg.Add(1)
			go func() {
				defer wg.Done()
				old.leave(reg, drain)
			}()
		case dies:
			copies[0].die()
		}
		window := 0
		if i >= replaced {
			window = 1
		}
		for _, c := range callers {
			order := c.order(reg)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, addr := range order {
					if get(addr + "/orders/7") {
						return
					}
				}
				c.failed[window].Add(1)
			}()
		}
	}
	wg.Wait()
	for _, c := range copies[1:] {
		c.leave(reg, 0)
	}

	fmt.Printf("PART 1 — \"orders\" runs as three copies. A caller sends it %s requests, one each millisecond.\n", commas(requests))
	fmt.Println("Meanwhile each copy is replaced by one at a new address, and then one copy dies without a word.")
	fmt.Println()
	fmt.Printf("  %-50s %24s %20s\n", "how the caller finds a copy", "failed while the copies", "failed after one")
	fmt.Printf("  %-50s %24s %20s\n", "", fmt.Sprintf("were replaced, of %s", commas(replaced)), fmt.Sprintf("copy died, of %s", commas(requests-replaced)))
	for _, c := range callers {
		fmt.Printf("  %-50s %24s %20s\n", c.what, commas(int(c.failed[0].Load())), commas(int(c.failed[1].Load())))
	}
}
