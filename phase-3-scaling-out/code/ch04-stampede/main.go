// Chapter 3.4 — four ways a cache lets you down, and what stops each.
//
//	part 1  a popular, expensive entry expires: the stampede
//	part 2  the whole cache is emptied while the system is busy
//	part 3  requests for things that do not exist
//	part 4  a cache that is too small
//
// As in Chapter 3.3, the "database" and the "cache" are in-memory
// stand-ins with fixed latencies, so that the output shows the behaviour
// and nothing else. The database can run only so many queries at once;
// the rest wait, and give up when their request's deadline passes.
//
// Run:
//
//	go run ./phase-3-scaling-out/code/ch04-stampede            # all four parts, a little over a minute
//	go run ./phase-3-scaling-out/code/ch04-stampede -part 1
package main

import (
	"container/list"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	products = 100_000 // products 0 to 99,999 exist; any other number does not
	notFound = -1
)

// ------------------------------------------------------------ the database

type database struct {
	pool    chan struct{}
	latency time.Duration
	queries atomic.Int64
}

func newDatabase(connections int, latency time.Duration) *database {
	return &database{pool: make(chan struct{}, connections), latency: latency}
}

var errTimeout = errors.New("timed out waiting for the database")

// lookup runs one query, waiting for a free connection until the deadline.
func (db *database) lookup(key int, deadline time.Time) (int, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case db.pool <- struct{}{}:
	case <-timer.C:
		return 0, errTimeout
	}
	time.Sleep(db.latency)
	<-db.pool
	db.queries.Add(1)
	if key < 0 || key >= products {
		return notFound, nil
	}
	return key * 7, nil // some value
}

// ------------------------------------------------------------ the cache

// An item is fresh until `fresh`. After that it is stale, and may still be
// served until `dead` by a caller that is willing.
type item struct {
	key, value  int
	fresh, dead time.Time
}

// cache is a key-value store with expiry and, if capacity is set, a limit
// on the number of entries: when it is full, the entry used least
// recently is evicted.
type cache struct {
	mu        sync.Mutex
	items     map[int]*list.Element
	recent    *list.List // most recently used first
	capacity  int
	hop       time.Duration
	evictions int
}

func newCache(capacity int, hop time.Duration) *cache {
	return &cache{items: map[int]*list.Element{}, recent: list.New(), capacity: capacity, hop: hop}
}

const (
	miss = iota
	fresh
	stale
)

func (c *cache) get(key int) (value int, state int) {
	time.Sleep(c.hop)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return 0, miss
	}
	it := el.Value.(*item)
	now := time.Now()
	if now.After(it.dead) {
		return 0, miss
	}
	c.recent.MoveToFront(el)
	if now.After(it.fresh) {
		return it.value, stale
	}
	return it.value, fresh
}

// set stores a value that is fresh for ttl and may be served stale for a
// further grace period.
func (c *cache) set(key, value int, ttl, grace time.Duration) {
	time.Sleep(c.hop)
	c.mu.Lock()
	defer c.mu.Unlock()
	it := &item{key, value, time.Now().Add(ttl), time.Now().Add(ttl + grace)}
	if el, ok := c.items[key]; ok {
		el.Value = it
		c.recent.MoveToFront(el)
		return
	}
	c.items[key] = c.recent.PushFront(it)
	if c.capacity > 0 && c.recent.Len() > c.capacity {
		oldest := c.recent.Back()
		c.recent.Remove(oldest)
		delete(c.items, oldest.Value.(*item).key)
		c.evictions++
	}
}

func (c *cache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items, c.recent = map[int]*list.Element{}, list.New()
}

func (c *cache) count(matching func(*item) bool) (n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.recent.Front(); el != nil; el = el.Next() {
		if matching(el.Value.(*item)) {
			n++
		}
	}
	return n
}

// ------------------------------------------------------------ the service

type service struct {
	db    *database
	cache *cache
	ttl   time.Duration

	coalesce    bool          // at most one database query per key at a time
	grace       time.Duration // serve a stale entry for this long while it is refreshed
	negativeTTL time.Duration // remember "not found" for this long (0: do not)

	mu      sync.Mutex
	flights map[int]*flight // queries in progress, by key

	hits, misses, realHits, realReads atomic.Int64
}

// A flight is one database query in progress, and everyone waiting for it.
type flight struct {
	done  chan struct{}
	value int
	err   error
}

func (s *service) get(key int, deadline time.Time) (int, error) {
	real := key >= 0 && key < products
	if real {
		s.realReads.Add(1)
	}
	value, state := s.cache.get(key)
	switch state {
	case fresh:
		s.hits.Add(1)
		if real {
			s.realHits.Add(1)
		}
		return value, nil
	case stale:
		// Good enough for this caller. Refresh it in the background — once,
		// however many callers notice at the same moment.
		s.hits.Add(1)
		go s.loadOnce(key, time.Now().Add(5*time.Second))
		return value, nil
	}
	s.misses.Add(1)
	if s.coalesce {
		return s.loadOnce(key, deadline)
	}
	return s.load(key, deadline)
}

// load asks the database and stores the answer in the cache.
func (s *service) load(key int, deadline time.Time) (int, error) {
	value, err := s.db.lookup(key, deadline)
	if err != nil {
		return 0, err
	}
	if value != notFound {
		s.cache.set(key, value, s.ttl, s.grace)
	} else if s.negativeTTL > 0 {
		s.cache.set(key, notFound, s.negativeTTL, 0)
	}
	return value, nil
}

// loadOnce is load, except that callers asking for the same key while a
// query for it is already running wait for that query's answer instead of
// starting their own. (In production Go code: golang.org/x/sync/singleflight.)
func (s *service) loadOnce(key int, deadline time.Time) (int, error) {
	s.mu.Lock()
	if s.flights == nil {
		s.flights = map[int]*flight{}
	}
	if f, ok := s.flights[key]; ok {
		s.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-f.done:
			return f.value, f.err
		case <-timer.C:
			return 0, errTimeout
		}
	}
	f := &flight{done: make(chan struct{})}
	s.flights[key] = f
	s.mu.Unlock()

	f.value, f.err = s.load(key, deadline)

	s.mu.Lock()
	delete(s.flights, key)
	s.mu.Unlock()
	close(f.done)
	return f.value, f.err
}

// ------------------------------------------------------------ load

type second struct{ ok, failed int }

type report struct {
	ok, failed int
	latencies  []time.Duration
	perSecond  []second
}

func (r *report) pct(p float64) string {
	if len(r.latencies) == 0 {
		return "-"
	}
	sort.Slice(r.latencies, func(i, j int) bool { return r.latencies[i] < r.latencies[j] })
	return ms(r.latencies[int(p*float64(len(r.latencies)-1))])
}

func ms(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	if d >= 10*time.Millisecond {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

// drive sends reads at a steady rate for a number of seconds. key(i)
// chooses what the i-th read asks for; each read is given up after
// `patience`. at, if set, is called once when the given second begins.
func drive(s *service, rate, seconds int, patience time.Duration, key func(i int) int, at int, then func()) *report {
	rep := &report{perSecond: make([]second, seconds)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	begin := time.Now()
	for i := 0; i < rate*seconds; i++ {
		time.Sleep(time.Until(begin.Add(time.Duration(i) * time.Second / time.Duration(rate))))
		if then != nil && i == at*rate {
			then()
		}
		k, bucket := key(i), i/rate
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			_, err := s.get(k, start.Add(patience))
			took := time.Since(start)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rep.failed++
				rep.perSecond[bucket].failed++
				return
			}
			rep.ok++
			rep.perSecond[bucket].ok++
			rep.latencies = append(rep.latencies, took)
		}()
	}
	wg.Wait()
	return rep
}

// popular returns a function that picks products the way customers do:
// product 0 most often, and so on down.
func popular() func(int) int {
	z := rand.NewZipf(rand.New(rand.NewSource(1)), 1.4, 2, products-1)
	return func(int) int { return int(z.Uint64()) }
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — one popular entry: 500 reads a second. Computing it takes 300 ms; it is cached for 1 second.")
	fmt.Println("         The database runs 12 queries at once. A reader waits at most 2 seconds. Six seconds of load.")
	fmt.Println()
	fmt.Printf("  %-52s %10s %8s %8s %9s %9s\n", "when the entry expires...", "db queries", "ok", "failed", "p50", "p99")
	for _, v := range []struct {
		name     string
		coalesce bool
		grace    time.Duration
	}{
		{"every reader that misses asks the database", false, 0},
		{"one reader asks; the others wait for its answer", true, 0},
		{"one reader asks; the others get the old value", true, 10 * time.Second},
	} {
		s := &service{db: newDatabase(12, 300*time.Millisecond), cache: newCache(0, time.Millisecond),
			ttl: time.Second, coalesce: v.coalesce, grace: v.grace}
		s.cache.set(0, 0, s.ttl, s.grace) // the entry is in the cache when the load begins
		r := drive(s, 500, 6, 2*time.Second, func(int) int { return 0 }, 0, nil)
		fmt.Printf("  %-52s %10d %8d %8d %9s %9s\n", v.name, s.db.queries.Load(), r.ok, r.failed, r.pct(0.50), r.pct(0.99))
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — 2,000 reads a second over the whole catalogue. A query takes 50 ms; the database runs 12 at once")
	fmt.Println("         (240 a second). With a warm cache it sees about 100 a second. Two seconds in, the cache is emptied.")
	fmt.Println("         A reader waits at most 1 second.")
	fmt.Println()
	fmt.Printf("  %-30s %-62s %8s %12s\n", "", "failed reads in each second", "in all", "db queries")
	fmt.Printf("  %-30s %s\n", "", strings.Repeat("-", 84))
	for _, v := range []struct {
		name     string
		coalesce bool
	}{
		{"every miss asks the database", false},
		{"one query per key at a time", true},
	} {
		s := &service{db: newDatabase(12, 50*time.Millisecond), cache: newCache(0, time.Millisecond),
			ttl: time.Minute, coalesce: v.coalesce}
		for k := 0; k < 2000; k++ { // start warm
			s.cache.set(k, k*7, s.ttl, 0)
		}
		var before int64
		r := drive(s, 2000, 12, time.Second, popular(), 2, func() {
			before = s.db.queries.Load()
			s.cache.flush()
		})
		fmt.Printf("  %-30s", v.name)
		for _, sec := range r.perSecond {
			fmt.Printf("%5d", sec.failed)
		}
		fmt.Printf("   %8d %12d\n", r.failed, s.db.queries.Load()-before)
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — 2,000 reads a second, a quarter of them for products that do not exist. A query takes 5 ms.")
	fmt.Println("         The cache holds 5,000 entries. Five seconds of load.")
	fmt.Println()
	fmt.Printf("  %-36s %-26s %12s %22s %20s\n", "the missing products are...", "\"not found\" is...", "db queries/s", "hit ratio, real products", "\"not found\" entries")
	for _, v := range []struct {
		missing  string
		repeat   bool
		negative time.Duration
	}{
		{"the same 50, again and again", true, 0},
		{"the same 50, again and again", true, 30 * time.Second},
		{"different every time", false, 0},
		{"different every time", false, 30 * time.Second},
	} {
		s := &service{db: newDatabase(40, 5*time.Millisecond), cache: newCache(5000, time.Millisecond),
			ttl: time.Minute, negativeTTL: v.negative}
		for k := 1999; k >= 0; k-- { // start warm
			s.cache.set(k, k*7, s.ttl, 0)
		}
		pick, rng, unseen := popular(), rand.New(rand.NewSource(3)), products
		key := func(i int) int {
			switch {
			case i%4 != 0:
				return pick(i)
			case v.repeat:
				return products + rng.Intn(50)
			default:
				unseen++
				return unseen
			}
		}
		drive(s, 2000, 5, time.Second, key, 0, nil)
		remembered := "not remembered"
		if v.negative > 0 {
			remembered = "remembered for 30 s"
		}
		fmt.Printf("  %-36s %-26s %12d %21.0f%% %20d\n", v.missing, remembered, int(s.db.queries.Load())/5,
			float64(s.realHits.Load())*100/float64(s.realReads.Load()), s.cache.count(func(it *item) bool { return it.value == notFound }))
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — how big must the cache be? 200,000 reads of a 100,000-product catalogue, through caches of four sizes.")
	fmt.Println("         When a cache is full it evicts the entry used least recently.")
	fmt.Println()
	fmt.Printf("  %18s %22s %12s %12s\n", "cache holds", "share of the catalogue", "hit ratio", "evictions")
	for _, capacity := range []int{100, 1000, 10000, 100000} {
		s := &service{db: newDatabase(1, 0), cache: newCache(capacity, 0), ttl: time.Hour}
		pick := popular()
		for i := 0; i < 200_000; i++ {
			s.get(pick(i), time.Now().Add(time.Second))
		}
		fmt.Printf("  %10d entries %21.1f%% %11.0f%% %12d\n", capacity, float64(capacity)*100/products,
			float64(s.hits.Load())*100/200_000, s.cache.evictions)
	}
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
