// Chapter 3.3 — what a cache buys, and what it costs.
//
// A catalogue of 100,000 products, a few of them very popular, is read
// 2,000 times a second. A database query takes 5 ms; a cache lookup takes
// 1 ms. Both the "database" and the "cache" are small in-memory stand-ins
// with those latencies, so that the numbers show the caching patterns and
// nothing else.
//
//	part 1  no cache, and cache-aside
//	part 2  prices change: three ways for the cache to learn of it, and how stale readers get
//	part 3  two races that leave a wrong entry in the cache, step by step
//
// Run:
//
//	go run ./phase-3-scaling-out/code/ch03-cache            # all three parts, about 45 seconds
//	go run ./phase-3-scaling-out/code/ch03-cache -part 3
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const products = 100_000

type product struct {
	id      int
	cents   int
	version int // goes up by one with every change of price
}

// ------------------------------------------------------------ the database

// database is the system of record. A query takes 5 ms, and at most 40
// run at once.
type database struct {
	mu      sync.Mutex
	rows    []product
	changes map[int][]time.Time // for each product, when each new version was committed
	pool    chan struct{}
	latency time.Duration
	queries atomic.Int64
}

func newDatabase(latency time.Duration) *database {
	db := &database{rows: make([]product, products), changes: map[int][]time.Time{}, pool: make(chan struct{}, 40), latency: latency}
	for i := range db.rows {
		db.rows[i] = product{id: i, cents: 1000, version: 1}
	}
	return db
}

func (db *database) query() {
	db.pool <- struct{}{}
	time.Sleep(db.latency)
	<-db.pool
	db.queries.Add(1)
}

func (db *database) read(id int) product {
	db.query()
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.rows[id]
}

func (db *database) write(id, cents int) product {
	db.query()
	db.mu.Lock()
	defer db.mu.Unlock()
	db.rows[id].cents = cents
	db.rows[id].version++
	db.changes[id] = append(db.changes[id], time.Now())
	return db.rows[id]
}

// staleFor reports how long the version a reader was given had already
// been out of date when the reader asked. Zero means it was current.
func (db *database) staleFor(got product, asked time.Time) time.Duration {
	db.mu.Lock()
	defer db.mu.Unlock()
	changes := db.changes[got.id] // changes[0] is when version 2 was committed, and so on
	if got.version-1 < len(changes) {
		if replaced := changes[got.version-1]; replaced.Before(asked) {
			return asked.Sub(replaced)
		}
	}
	return 0
}

// ------------------------------------------------------------ the cache

type entry struct {
	value   product
	expires time.Time
}

// cache is a key-value store with expiry, one network hop away.
type cache struct {
	mu     sync.Mutex
	items  map[int]entry
	leases map[int]uint64 // the lease currently valid for a key, if any
	next   uint64
	ttl    time.Duration
	hop    time.Duration
}

func newCache(ttl, hop time.Duration) *cache {
	return &cache{items: map[int]entry{}, leases: map[int]uint64{}, ttl: ttl, hop: hop}
}

func (c *cache) get(id int) (product, bool) {
	time.Sleep(c.hop)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[id]
	if !ok || time.Now().After(e.expires) {
		return product{}, false
	}
	return e.value, true
}

func (c *cache) set(id int, p product) {
	time.Sleep(c.hop)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[id] = entry{p, time.Now().Add(c.ttl)}
}

func (c *cache) del(id int) {
	time.Sleep(c.hop)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, id)
	delete(c.leases, id) // whoever was about to fill this entry may no longer do so
}

// getWithLease is get, except that on a miss it also hands out a lease:
// a token that gives the caller the right to fill this entry.
func (c *cache) getWithLease(id int) (p product, ok bool, lease uint64) {
	if p, ok = c.get(id); ok {
		return p, true, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	c.leases[id] = c.next
	return product{}, false, c.next
}

// setWithLease stores the value only if the lease is still the valid one
// for the key. A delete in the meantime will have cancelled it.
func (c *cache) setWithLease(id int, p product, lease uint64) bool {
	c.mu.Lock()
	valid := c.leases[id] == lease
	c.mu.Unlock()
	if valid {
		c.set(id, p)
	}
	return valid
}

// ------------------------------------------------------------ the service

// service is the application code. onWrite says what a price change does
// to the cache: "nothing", "delete" or "update".
type service struct {
	db      *database
	cache   *cache // nil: no cache
	onWrite string
	hits    atomic.Int64
	misses  atomic.Int64
}

// price is cache-aside: look in the cache; on a miss, ask the database
// and put the answer in the cache for the next reader.
func (s *service) price(id int) product {
	if s.cache == nil {
		return s.db.read(id)
	}
	if p, ok := s.cache.get(id); ok {
		s.hits.Add(1)
		return p
	}
	s.misses.Add(1)
	p := s.db.read(id)
	s.cache.set(id, p)
	return p
}

// setPrice changes the database first, and then deals with the cache.
func (s *service) setPrice(id, cents int) {
	p := s.db.write(id, cents)
	switch s.onWrite {
	case "delete":
		s.cache.del(id)
	case "update":
		s.cache.set(id, p)
	}
}

// ------------------------------------------------------------ load

type report struct {
	reads       int
	latencies   []time.Duration
	stale       int
	worstStale  time.Duration
	queriesPerS float64
	hitRatio    float64
}

// run reads prices at 2,000 a second — popular products far more often
// than the rest — for two seconds to warm up and then five seconds that
// are measured. If writes is not zero, it also changes the price of one of
// the fifty most popular products that many times a second.
func run(s *service, writes int) report {
	const rate, warmUp, measured = 2000, 2, 5
	ids := make([]int, rate*(warmUp+measured))
	for i := range ids {
		ids[i] = int(popularity.Uint64())
	}

	stop := make(chan struct{})
	var writers sync.WaitGroup
	if writes > 0 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			rng := rand.New(rand.NewSource(2))
			tick := time.NewTicker(time.Second / time.Duration(writes))
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					go s.setPrice(rng.Intn(50), 1000+rng.Intn(9000))
				}
			}
		}()
	}

	var rep report
	var mu sync.Mutex
	var wg sync.WaitGroup
	var queriesBefore int64
	var measuringSince time.Time
	begin := time.Now()
	for i, id := range ids {
		time.Sleep(time.Until(begin.Add(time.Duration(i) * time.Second / rate)))
		warm := i >= rate*warmUp
		if i == rate*warmUp { // the warm-up is over: start counting
			queriesBefore, measuringSince = s.db.queries.Load(), time.Now()
			s.hits.Store(0)
			s.misses.Store(0)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			asked := time.Now()
			p := s.price(id)
			took := time.Since(asked)
			if !warm {
				return
			}
			stale := s.db.staleFor(p, asked)
			mu.Lock()
			rep.latencies = append(rep.latencies, took)
			if stale > 0 {
				rep.stale++
				rep.worstStale = max(rep.worstStale, stale)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(measuringSince).Seconds()
	close(stop)
	writers.Wait()

	rep.reads = len(rep.latencies)
	rep.queriesPerS = float64(s.db.queries.Load()-queriesBefore) / elapsed
	if total := s.hits.Load() + s.misses.Load(); total > 0 {
		rep.hitRatio = float64(s.hits.Load()) / float64(total)
	}
	sort.Slice(rep.latencies, func(i, j int) bool { return rep.latencies[i] < rep.latencies[j] })
	return rep
}

// popularity picks a product: number 0 most often, and so on down. The
// same sequence is used for every run.
var popularity *rand.Zipf

func resetPopularity() { popularity = rand.NewZipf(rand.New(rand.NewSource(1)), 1.4, 2, products-1) }

func (r report) pct(p float64) string {
	return ms(r.latencies[int(p*float64(len(r.latencies)-1))])
}

func ms(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.2f s", d.Seconds())
	}
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

const (
	dbLatency = 5 * time.Millisecond
	cacheHop  = time.Millisecond
)

// ------------------------------------------------------------ part 1

func part1() {
	resetPopularity()
	top := 0
	for i := 0; i < 100_000; i++ {
		if popularity.Uint64() < 1000 {
			top++
		}
	}
	fmt.Println("PART 1 — 2,000 reads a second; a database query takes 5 ms, a cache lookup 1 ms")
	fmt.Printf("         (the 1,000 most popular of 100,000 products receive %d%% of the reads)\n", top/1000)
	fmt.Println()
	fmt.Printf("  %-34s %14s %10s %9s %9s %9s\n", "", "db queries/s", "hit ratio", "p50", "p90", "p99")
	resetPopularity()
	none := run(&service{db: newDatabase(dbLatency)}, 0)
	fmt.Printf("  %-34s %14.0f %10s %9s %9s %9s\n", "no cache", none.queriesPerS, "-", none.pct(0.50), none.pct(0.90), none.pct(0.99))
	for _, ttl := range []time.Duration{time.Second, 30 * time.Second} {
		resetPopularity()
		r := run(&service{db: newDatabase(dbLatency), cache: newCache(ttl, cacheHop)}, 0)
		fmt.Printf("  %-34s %14.0f %9.0f%% %9s %9s %9s\n", fmt.Sprintf("cache-aside, entries live %v", ttl),
			r.queriesPerS, r.hitRatio*100, r.pct(0.50), r.pct(0.90), r.pct(0.99))
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — the same, while prices of popular products change 20 times a second. Cached entries live 2 seconds.")
	fmt.Println()
	fmt.Printf("  %-44s %12s %24s %14s\n", "when a price changes, the writer...", "stale reads", "oldest price served was", "db queries/s")
	for _, c := range []struct{ name, onWrite string }{
		{"does nothing: the entry expires in time", "nothing"},
		{"deletes the cached entry", "delete"},
		{"updates the cached entry", "update"},
	} {
		s := &service{db: newDatabase(dbLatency), cache: newCache(2*time.Second, cacheHop), onWrite: c.onWrite}
		resetPopularity()
		r := run(s, 20)
		worst := "-"
		if r.stale > 0 {
			worst = ms(r.worstStale) + " out of date"
		}
		fmt.Printf("  %-44s %6d (%2.0f%%) %24s %14.0f\n", c.name, r.stale, float64(r.stale)*100/float64(r.reads), worst, r.queriesPerS)
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 3

func dollars(p product) string {
	return fmt.Sprintf("$%d.%02d (version %d)", p.cents/100, p.cents%100, p.version)
}

func part3() {
	const id = 7
	step := func(who, what string) { fmt.Printf("     %-9s %s\n", who, what) }
	verdict := func(db *database, c *cache) {
		truth := db.read(id)
		if cached, ok := c.get(id); !ok {
			fmt.Printf("     => the database says %s; the cache has no entry: the next reader will fetch the right price\n\n", dollars(truth))
		} else if cached.version == truth.version {
			fmt.Printf("     => the database says %s; so does the cache\n\n", dollars(truth))
		} else {
			fmt.Printf("     => the database says %s; the cache says %s, and will until the entry expires\n\n", dollars(truth), dollars(cached))
		}
	}
	fmt.Println("PART 3 — two races, one step at a time. Product 7 costs $10.00.")
	fmt.Println()

	fmt.Println("  A. Two writers, each updating the cache after the database")
	db, c := newDatabase(0), newCache(time.Minute, 0)
	first := db.write(id, 1100)
	step("writer 1", "database: price = "+dollars(first))
	second := db.write(id, 1200)
	step("writer 2", "database: price = "+dollars(second))
	c.set(id, second)
	step("writer 2", "cache:    set "+dollars(second))
	c.set(id, first)
	step("writer 1", "cache:    set "+dollars(first)+"      <- arrives late")
	verdict(db, c)

	fmt.Println("  B. The same two writers, each deleting the cached entry instead")
	db, c = newDatabase(0), newCache(time.Minute, 0)
	first = db.write(id, 1100)
	step("writer 1", "database: price = "+dollars(first))
	second = db.write(id, 1200)
	step("writer 2", "database: price = "+dollars(second))
	c.del(id)
	step("writer 2", "cache:    delete")
	c.del(id)
	step("writer 1", "cache:    delete                       <- arrives late, and does no harm")
	verdict(db, c)

	fmt.Println("  C. A reader and a writer: the stale set")
	db, c = newDatabase(0), newCache(time.Minute, 0)
	_, found := c.get(id)
	step("reader", fmt.Sprintf("cache:    get -> found: %v", found))
	old := db.read(id)
	step("reader", "database: reads "+dollars(old))
	changed := db.write(id, 1100)
	step("writer", "database: price = "+dollars(changed))
	c.del(id)
	step("writer", "cache:    delete                       (there is nothing to delete yet)")
	c.set(id, old)
	step("reader", "cache:    set "+dollars(old)+"      <- the old price, stored after the delete")
	verdict(db, c)

	fmt.Println("  D. The same, with a lease")
	db, c = newDatabase(0), newCache(time.Minute, 0)
	_, found, lease := c.getWithLease(id)
	step("reader", fmt.Sprintf("cache:    get -> found: %v, and lease %d: the right to fill this entry", found, lease))
	old = db.read(id)
	step("reader", "database: reads "+dollars(old))
	changed = db.write(id, 1100)
	step("writer", "database: price = "+dollars(changed))
	c.del(id)
	step("writer", "cache:    delete                       (which cancels the lease)")
	stored := c.setWithLease(id, old, lease)
	step("reader", fmt.Sprintf("cache:    set %s with lease %d -> stored: %v", dollars(old), lease, stored))
	verdict(db, c)
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 3)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3} {
		if *part == 0 || *part == i+1 {
			run()
		}
	}
}
