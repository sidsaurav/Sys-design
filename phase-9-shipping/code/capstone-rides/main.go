// Final capstone — a ride-hailing backend in miniature: two cities, each a cell of its own.
//
// What is real: the claim on a driver, the offer and its timeout, the states of a ride, the
// outbox and its relay, the payment and its key, and the check on who may read a ride. They
// run concurrently, as copies of a service would, and they race as copies of a service do.
// What is a model: the drivers, the riders and the payment provider. Time is compressed: one
// second in the city is 10 ms here, so five minutes of two cities take about four seconds.
//
//	go run ./phase-9-shipping/code/capstone-rides
//	go run ./phase-9-shipping/code/capstone-rides -without claim
//
// -without takes any of: claim, fresh, offer-timeout, cells, outbox, idempotency,
// owner-check. The program exits with status 1 if anything that must be true is broken.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	second   = 10 * time.Millisecond // one second in the city
	beat     = 4 * second            // a driver's phone reports its position
	freshFor = 15 * second           // a position older than this is not used
	offerFor = 8 * second            // a driver has this long to accept
	giveUp   = 25 * second           // dispatch stops looking, so that…
	answerBy = 30 * second           // …every rider is answered within this
	runFor   = 300                   // seconds of the city's time
	grid     = 10                    // a city is 10 × 10 cells
	drivers  = 150                   // in each city
	slots    = 24                    // requests that a city's dispatch works on at once
	waiting  = 12                    // requests that may wait for a slot
)

var without = map[string]bool{}

// on reports whether a safeguard is in place.
func on(safeguard string) bool { return !without[safeguard] }

// The things that must be true, and how often each was broken.
type audit struct {
	twoRides, twoDrivers, late, stale atomic.Int64
}

// A system is everything that the cities share: the payment pipeline, and the auditor.
type system struct {
	audit    audit
	rides    atomic.Int64 // the last ride's number
	finished atomic.Int64
	events   chan event
	provider *provider
}

const (
	yes  = iota // given a driver
	no          // told that there are no cars, after dispatch has looked
	away        // turned away at the door: dispatch was full
)

type city struct {
	name    string
	sys     *system
	index   *index
	store   *store
	drivers []*driver
	queue   *queue
	asking  sync.Map // ride → the request that dispatch is working on

	mu    sync.Mutex
	asked int
	count [3]int
	took  []time.Duration
}

func newCity(name string, sys *system, q *queue) *city {
	c := &city{name: name, sys: sys, queue: q, index: newIndex(), store: newStore(sys)}
	for i := 0; i < drivers; i++ {
		c.drivers = append(c.drivers, &driver{id: i, offers: make(chan int, 4)})
	}
	return c
}

// answer tells a rider yes or no, and notes how long it took.
func (c *city) answer(req *request, how int) {
	took := time.Since(req.at)
	if took > answerBy+second {
		c.sys.audit.late.Add(1)
	}
	c.mu.Lock()
	c.count[how]++
	c.took = append(c.took, took)
	c.mu.Unlock()
}

func main() {
	list := flag.String("without", "", "safeguards to remove, separated by commas")
	flag.Parse()
	for _, s := range strings.Split(*list, ",") {
		if s != "" {
			without[s] = true
		}
	}

	sys := &system{events: make(chan event, 1<<16), provider: &provider{charges: map[int]int{}, seen: map[string]bool{}}}
	north, south := newQueue(1), newQueue(1)
	if !on("cells") {
		north = newQueue(2) // one queue and one set of slots, for both cities
		south = north
	}
	cities := []*city{newCity("north", sys, north), newCity("south", sys, south)}

	stop, begin := make(chan struct{}), time.Now()
	for i, c := range cities {
		for _, d := range c.drivers {
			go d.run(c, int64(i), begin, stop)
		}
		go c.relay(stop)
	}
	for i := 0; i < 4; i++ {
		go sys.payments()
	}
	time.Sleep(2 * beat) // every driver has reported once
	var wg sync.WaitGroup
	for i, c := range cities {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.riders(int64(i), c.name == "south")
		}()
	}
	wg.Wait()
	time.Sleep((giveUp + 75*second) + 25*second) // the last rides finish, and the last payments are made
	close(stop)

	report(cities, sys)
}

func report(cities []*city, sys *system) {
	removed := []string{}
	for name := range without {
		removed = append(removed, name)
	}
	sort.Strings(removed)
	if len(removed) == 0 {
		removed = []string{"nothing"}
	}
	fmt.Printf("Two cities of %d drivers, for five minutes. At 2:00 a stadium empties in the south.\n", drivers)
	fmt.Printf("Removed: %s.\n\n", strings.Join(removed, ", "))
	fmt.Printf("  %-8s %8s %16s %10s %14s %10s %12s\n", "city", "asked", "given a driver", "told no", "turned away", "median", "slowest 1%")
	fmt.Printf("  %-8s %8s %16s %10s %14s %10s %12s\n", "", "", "", "", "at the door", "answer", "of answers")
	unanswered, quiet := 0, 0
	for _, c := range cities {
		c.mu.Lock()
		sort.Slice(c.took, func(a, b int) bool { return c.took[a] < c.took[b] })
		answered := len(c.took)
		at := func(q float64) string {
			if answered == 0 {
				return "-"
			}
			return fmt.Sprintf("%.1f s", float64(c.took[int(q*float64(answered-1))])/float64(second))
		}
		fmt.Printf("  %-8s %8d %9d (%3.0f%%) %10d %14d %10s %12s\n", c.name, c.asked, c.count[yes], 100*float64(c.count[yes])/float64(c.asked),
			c.count[no], c.count[away], at(0.5), at(0.99))
		unanswered += c.asked - answered
		if c.name == "north" {
			quiet = c.count[away]
		}
		c.mu.Unlock()
	}

	stuck, uncharged, twice, done := 0, 0, 0, []int{}
	for _, c := range cities {
		c.store.mu.Lock()
		for _, rideID := range c.store.claimed {
			if rideID != 0 && c.store.rides[rideID].state == "asked" {
				stuck++
			}
		}
		for id, r := range c.store.rides {
			if r.state == "finished" {
				done = append(done, id)
			}
		}
		c.store.mu.Unlock()
	}
	sys.provider.mu.Lock()
	for _, id := range done {
		switch n := sys.provider.charges[id]; {
		case n == 0:
			uncharged++
		case n > 1:
			twice++
		}
	}
	sys.provider.mu.Unlock()
	strangers, r := 0, rand.New(rand.NewSource(9))
	for i := 0; i < 200 && len(done) > 0; i++ { // somebody asks for a ride that is not theirs
		id := done[r.Intn(len(done))]
		for _, c := range cities {
			if c.store.read(id, -1) {
				strangers++
			}
		}
	}

	fmt.Printf("\n  %-66s %8s\n", "what must be true", "broken")
	broken := 0
	for _, check := range []struct {
		what string
		n    int
	}{
		{"a driver has one ride at a time", int(sys.audit.twoRides.Load())},
		{"a ride has one driver", int(sys.audit.twoDrivers.Load())},
		{"every rider is answered, within 30 seconds", int(sys.audit.late.Load()) + unanswered},
		{"no offer goes to a driver who has been silent for 15 seconds", int(sys.audit.stale.Load())},
		{"no driver is left held for a ride that never came", stuck},
		{"a city that is not busy turns nobody away", quiet},
		{"every finished ride is charged", uncharged},
		{"no ride is charged twice", twice},
		{"nobody reads a ride that is not theirs", strangers},
	} {
		fmt.Printf("  %-66s %8d\n", check.what, check.n)
		broken += check.n
	}
	fmt.Printf("\n  rides finished: %d   charges made: %d\n", len(done), sys.provider.total())
	if broken > 0 {
		os.Exit(1)
	}
}
