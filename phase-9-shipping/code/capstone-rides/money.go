package main

import (
	"fmt"
	"sync"
	"time"
)

// An event says that a ride has finished, and what it cost.
type event struct{ ride, pence int }

// finish ends a ride. The ride and its event are written together, in the store. Without
// the outbox they are two writes, and a process can die between them.
func (c *city) finish(id int) {
	ev, first := c.store.finish(id)
	if !first {
		return
	}
	if c.sys.finished.Add(1)%12 == 0 {
		return // the process dies here: after the first write, and before anything else
	}
	if !on("outbox") {
		c.sys.events <- ev
	}
}

// relay publishes what is in the outbox, and only then removes it. One time in five it dies
// between the two, so that some events are published twice: delivery is at least once.
func (c *city) relay(stop <-chan struct{}) {
	for round := 1; ; round++ {
		select {
		case <-stop:
			return
		case <-time.After(second):
		}
		c.store.mu.Lock()
		batch := append([]event(nil), c.store.outbox...)
		c.store.mu.Unlock()
		for _, ev := range batch {
			c.sys.events <- ev
		}
		if round%5 == 0 {
			continue
		}
		c.store.mu.Lock()
		c.store.outbox = c.store.outbox[len(batch):]
		c.store.mu.Unlock()
	}
}

// payments charges for each finished ride, and tries again when the provider does not answer.
func (sys *system) payments() {
	for ev := range sys.events {
		key := ""
		if on("idempotency") {
			key = fmt.Sprint("ride-", ev.ride)
		}
		for try := 0; try < 8 && !sys.provider.charge(key, ev); try++ {
			time.Sleep(second)
		}
	}
}

// The provider takes the money. One answer in twelve is lost after the money has moved. A
// request that carries a key it has seen before is answered again, and not carried out again.
type provider struct {
	mu      sync.Mutex
	charges map[int]int // ride → the times that it has been charged
	seen    map[string]bool
	calls   int
}

func (p *provider) charge(key string, ev event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if key != "" && p.seen[key] {
		return true
	}
	p.seen[key] = true
	p.charges[ev.ride]++
	return p.calls%12 != 0
}

func (p *provider) total() (n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, times := range p.charges {
		n += times
	}
	return n
}
