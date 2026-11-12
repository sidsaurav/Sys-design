package main

import (
	"sort"
	"sync"
	"time"
)

// The index holds each driver's last reported position. It is kept in memory and is not
// durable: if it is lost, the phones fill it again within one beat.
type index struct {
	mu    sync.Mutex
	cells [grid][grid]map[int]bool
	at    map[int]position
}

type position struct {
	x, y int
	seen time.Time
}

func newIndex() *index {
	ix := &index{at: map[int]position{}}
	for x := range ix.cells {
		for y := range ix.cells[x] {
			ix.cells[x][y] = map[int]bool{}
		}
	}
	return ix
}

func (ix *index) report(driver, x, y int) {
	ix.mu.Lock()
	if old, ok := ix.at[driver]; ok {
		delete(ix.cells[old.x][old.y], driver)
	}
	ix.cells[x][y][driver] = true
	ix.at[driver] = position{x, y, time.Now()}
	ix.mu.Unlock()
}

// fresh reports whether a driver has been heard from lately.
func (ix *index) fresh(driver int) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return !on("fresh") || time.Since(ix.at[driver].seen) <= freshFor
}

func (ix *index) silentFor(driver int) time.Duration {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return time.Since(ix.at[driver].seen)
}

// near returns the drivers within three cells of a place, the nearest first.
func (ix *index) near(x, y int) []int {
	type candidate struct{ id, far int }
	var found []candidate
	ix.mu.Lock()
	for dx := -3; dx <= 3; dx++ {
		for dy := -3; dy <= 3; dy++ {
			if cx, cy := x+dx, y+dy; cx >= 0 && cx < grid && cy >= 0 && cy < grid {
				for id := range ix.cells[cx][cy] {
					found = append(found, candidate{id, abs(dx) + abs(dy)})
				}
			}
		}
	}
	ix.mu.Unlock()
	sort.Slice(found, func(a, b int) bool {
		if found[a].far != found[b].far {
			return found[a].far < found[b].far
		}
		return found[a].id < found[b].id
	})
	ids := make([]int, len(found))
	for i, f := range found {
		ids[i] = f.id
	}
	return ids
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// The store stands for a city's database: its drivers, its rides, and the outbox.
type store struct {
	sys     *system
	mu      sync.Mutex
	claimed [drivers]int  // for each driver, the ride that holds them, or 0
	active  [drivers]int  // for each driver, the rides that they are carrying out
	rides   map[int]*ride // by number
	outbox  []event       // rides that have finished, and have not yet been published
}

type ride struct {
	rider   int
	driver  int
	offered int    // the driver to whom it is on offer, or -1
	state   string // asked, matched, finished
}

func newStore(sys *system) *store { return &store{sys: sys, rides: map[int]*ride{}} }

func (s *store) add(id, rider int) {
	s.mu.Lock()
	s.rides[id] = &ride{rider: rider, driver: -1, offered: -1, state: "asked"}
	s.mu.Unlock()
}

// claim holds a driver for a ride, if nobody else holds them. It is one conditional write:
// UPDATE drivers SET ride = $1 WHERE id = $2 AND ride IS NULL.
func (s *store) claim(driver, id int) bool {
	s.mu.Lock()
	if s.claimed[driver] != 0 {
		s.mu.Unlock()
		return false
	}
	if !on("claim") { // look, and then write, as two statements
		s.mu.Unlock()
		time.Sleep(time.Millisecond)
		s.mu.Lock()
	}
	s.claimed[driver] = id
	s.rides[id].offered = driver
	s.mu.Unlock()
	return true
}

// match is a driver's acceptance. It succeeds only if the ride is still on offer to that
// driver: UPDATE rides SET state = 'matched' … WHERE id = $1 AND state = 'asked' AND offered = $2.
func (s *store) match(id, driver int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rides[id]
	if on("claim") && (r.state != "asked" || r.offered != driver) {
		return false // the offer was withdrawn, or the ride has gone to somebody else
	}
	if r.state == "finished" {
		return false
	}
	if r.state == "matched" && r.driver != driver {
		s.sys.audit.twoDrivers.Add(1)
	}
	r.state, r.driver = "matched", driver
	if s.active[driver]++; s.active[driver] > 1 {
		s.sys.audit.twoRides.Add(1)
	}
	return true
}

// withdraw takes an offer back. It reports whether the driver accepted first.
func (s *store) withdraw(id, driver int) (accepted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rides[id]
	if r.state != "asked" && r.driver == driver {
		return true
	}
	if r.offered == driver {
		r.offered = -1
	}
	if s.claimed[driver] == id {
		s.claimed[driver] = 0
	}
	return false
}

// finish ends a ride, frees its driver, and writes the event for it in the same transaction.
func (s *store) finish(id int) (ev event, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rides[id]
	if r.state == "finished" {
		return ev, false
	}
	r.state = "finished"
	s.active[r.driver]--
	if s.claimed[r.driver] == id {
		s.claimed[r.driver] = 0
	}
	ev = event{ride: id, pence: 1250}
	if on("outbox") {
		s.outbox = append(s.outbox, ev)
	}
	return ev, true
}

// read answers a request to see a ride. Only its own rider may.
func (s *store) read(id, rider int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rides[id]
	return r != nil && (!on("owner-check") || r.rider == rider)
}

// A queue is the door of a city's dispatch: a fixed number of slots, and a short line.
type queue struct {
	line chan *request
	room int
}

func newQueue(cities int) *queue {
	q := &queue{line: make(chan *request, 1<<16), room: waiting * cities}
	for i := 0; i < slots*cities; i++ {
		go func() {
			for req := range q.line {
				req.city.dispatch(req)
			}
		}()
	}
	return q
}

type request struct {
	city   *city
	ride   int
	x, y   int
	at     time.Time
	result chan reply // what drivers answer
}

type reply struct {
	driver int
	yes    bool
}

// ask is a rider's request for a ride.
func (c *city) ask(rider, x, y int) {
	req := &request{city: c, ride: int(c.sys.rides.Add(1)), x: x, y: y, at: time.Now(), result: make(chan reply, 8)}
	c.store.add(req.ride, rider)
	c.mu.Lock()
	c.asked++
	c.mu.Unlock()
	if len(c.queue.line) >= c.queue.room {
		c.answer(req, away) // dispatch is full: say so now, and not in half a minute
		return
	}
	c.asking.Store(req.ride, req)
	c.queue.line <- req
}

// dispatch finds a driver for one request, or gives up in time to say so.
func (c *city) dispatch(req *request) {
	defer c.asking.Delete(req.ride)
	for time.Since(req.at) < giveUp {
		tried := false
		for _, d := range c.index.near(req.x, req.y) {
			left := giveUp - time.Since(req.at)
			if left < 2*second {
				break // too little time for anybody to answer
			}
			if !c.index.fresh(d) || !c.store.claim(d, req.ride) {
				continue
			}
			tried = true
			if c.index.silentFor(d) > freshFor+second {
				c.sys.audit.stale.Add(1)
			}
			select {
			case c.drivers[d].offers <- req.ride:
			default:
			}
			if c.await(req, d, min(offerFor, left)) {
				c.answer(req, yes)
				return
			}
		}
		if !tried {
			time.Sleep(min(2*second, max(giveUp-time.Since(req.at), 0))) // nobody is free nearby: look again shortly
		}
	}
	c.answer(req, no)
}

// await waits for one driver's answer to one offer. It waits no longer than the offer
// lasts, and no longer than the request has left (Chapter 6.2).
func (c *city) await(req *request, d int, wait time.Duration) bool {
	var expired <-chan time.Time
	if on("offer-timeout") {
		t := time.NewTimer(wait)
		defer t.Stop()
		expired = t.C
	}
	for {
		select {
		case a := <-req.result:
			if a.driver != d {
				continue // an old answer, to an offer that was withdrawn
			}
			if a.yes {
				return true
			}
			return c.store.withdraw(req.ride, d)
		case <-expired:
			return c.store.withdraw(req.ride, d)
		}
	}
}
