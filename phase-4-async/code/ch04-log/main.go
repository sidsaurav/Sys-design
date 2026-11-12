// Chapter 4.4 — a log: the smallest thing with the shape of Kafka.
//
// This is not Kafka. It is a model of it, in one process, small enough to
// read: a topic is a few partitions; a partition is a list that is only
// ever appended to; a reader's place in it is one number.
//
//	part 1  two services want every event, and a third arrives later: a queue and a log
//	part 2  partitions: order, and working in parallel
//	part 3  a reader's place: committing, crashing, and going back
//	part 4  what the log forgets: retention and compaction
//
//	go run ./phase-4-async/code/ch04-log
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// ------------------------------------------------------------ the log

type record struct {
	offset int64 // its position in the partition, for ever
	key    string
	value  string
}

// A partition is a list that is only appended to. Old records may be dropped
// from the front (retention) or thinned out (compaction); the offsets of
// those that remain never change.
type partition struct {
	mu      sync.Mutex
	next    int64 // the offset the next record will be given
	records []record
}

func (p *partition) append(key, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, record{p.next, key, value})
	p.next++
}

// read returns up to max records whose offsets are at or after the one
// given. If older records have been dropped, it starts at the oldest that remains.
func (p *partition) read(from int64, max int) []record {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := sort.Search(len(p.records), func(i int) bool { return p.records[i].offset >= from })
	return append([]record(nil), p.records[i:min(len(p.records), i+max)]...)
}

// keepNewest drops all but the newest n records: retention.
func (p *partition) keepNewest(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if drop := len(p.records) - n; drop > 0 {
		p.records = p.records[drop:]
	}
}

// compact keeps only the newest record for each key. Offsets are unchanged.
func (p *partition) compact() {
	p.mu.Lock()
	defer p.mu.Unlock()
	newest := map[string]int64{}
	for _, r := range p.records {
		newest[r.key] = r.offset
	}
	var kept []record
	for _, r := range p.records {
		if newest[r.key] == r.offset {
			kept = append(kept, r)
		}
	}
	p.records = kept
}

// A topic is several partitions, and — for each group of readers — the
// offset that group has reached in each partition.
type topic struct {
	parts []*partition
	mu    sync.Mutex
	place map[string][]int64
	turn  int
}

func newTopic(partitions int) *topic {
	t := &topic{place: map[string][]int64{}}
	for i := 0; i < partitions; i++ {
		t.parts = append(t.parts, &partition{})
	}
	return t
}

// produce appends to the partition chosen by the key. Records with the same
// key always go to the same partition. With no key, partitions take turns.
func (t *topic) produce(key, value string) {
	var n int
	if key == "" {
		t.mu.Lock()
		t.turn++
		n = t.turn % len(t.parts)
		t.mu.Unlock()
	} else {
		h := fnv.New32a()
		h.Write([]byte(key))
		n = int(h.Sum32()) % len(t.parts)
	}
	t.parts[n].append(key, value)
}

func (t *topic) committed(group string, part int) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.place[group] == nil {
		t.place[group] = make([]int64, len(t.parts))
	}
	return t.place[group][part]
}

func (t *topic) commit(group string, part int, offset int64) {
	t.committed(group, part) // make sure the group exists
	t.mu.Lock()
	t.place[group][part] = offset
	t.mu.Unlock()
}

// consume reads everything a group has not yet read. Each partition is read
// by one goroutine: within a partition, in order; across partitions, at once.
func (t *topic) consume(group string, handle func(part int, r record)) {
	var wg sync.WaitGroup
	for n, p := range t.parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				batch := p.read(t.committed(group, n), 100)
				if len(batch) == 0 {
					return
				}
				for _, r := range batch {
					handle(n, r)
				}
				t.commit(group, n, batch[len(batch)-1].offset+1)
			}
		}()
	}
	wg.Wait()
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — 1,000 \"order placed\" events. The email service and the analytics service each need all of")
	fmt.Println("         them. A week later a fraud-checking service is added, and wants them too.")
	fmt.Println()
	fmt.Printf("  %-52s %10s %12s %22s\n", "", "email saw", "analytics saw", "fraud, a week later, saw")

	// A queue: one list; whoever takes a message removes it.
	var mu sync.Mutex
	var queue []int
	for i := 0; i < 1000; i++ {
		queue = append(queue, i)
	}
	take := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(queue) == 0 {
			return false
		}
		queue = queue[1:]
		return true
	}
	saw := make([]int, 2)
	var wg sync.WaitGroup
	for s := range saw {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for take() {
				saw[s]++
				time.Sleep(10 * time.Microsecond)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("  %-52s %10d %12d %22d\n", "a queue: a message is removed once it is processed", saw[0], saw[1], len(queue))

	// A log: nothing is removed; each group of readers has its own place.
	t := newTopic(4)
	for i := 0; i < 1000; i++ {
		t.produce(fmt.Sprint("order-", i), "placed")
	}
	count := func(group string) int {
		var n int
		var mu sync.Mutex
		t.consume(group, func(int, record) { mu.Lock(); n++; mu.Unlock() })
		return n
	}
	fmt.Printf("  %-52s %10d %12d %22d\n", "a log: each reader keeps its own place", count("email"), count("analytics"), count("fraud"))
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — 2,000 events: 200 accounts, 10 changes each, produced in order. Handling an event takes 1 ms.")
	fmt.Println()
	fmt.Printf("  %-52s %12s %32s\n", "", "events a second", "accounts seen out of order")

	for _, v := range []struct {
		name       string
		partitions int
		keyed      bool
	}{
		{"1 partition", 1, true},
		{"4 partitions, the account as the key", 4, true},
		{"4 partitions, no key", 4, false},
	} {
		t := newTopic(v.partitions)
		for account := 1; account <= 200; account++ { // an account's ten changes are produced one after another
			for change := 1; change <= 10; change++ {
				key := ""
				if v.keyed {
					key = fmt.Sprint("account-", account)
				}
				t.produce(key, fmt.Sprint(account, " ", change))
			}
		}
		var mu sync.Mutex
		last := map[int]int{}
		disorder := map[int]bool{}
		begin := time.Now()
		t.consume("ledger", func(_ int, r record) {
			var account, change int
			fmt.Sscan(r.value, &account, &change)
			time.Sleep(time.Millisecond)
			mu.Lock()
			if change < last[account] {
				disorder[account] = true
			}
			last[account] = change
			mu.Unlock()
		})
		fmt.Printf("  %-52s %12.0f %25d of 200\n", v.name, 2000/time.Since(begin).Seconds(), len(disorder))
	}
	fmt.Println()
	fmt.Println("  Each partition is read by one member of a group. The partitions each member gets, of 4:")
	for _, members := range []int{2, 4, 5} {
		got := make([][]int, members)
		for part := 0; part < 4; part++ {
			got[part%members] = append(got[part%members], part)
		}
		fmt.Printf("    a group of %d readers: %v\n", members, got)
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — one reader, 1,000 records. It commits its place every 10 records, and dies at record 995.")
	fmt.Println("         A replacement starts from the place that was committed.")
	fmt.Println()
	fmt.Printf("  %-52s %16s %16s\n", "the reader commits its place...", "handled twice", "never handled")

	for _, before := range []bool{false, true} {
		t := newTopic(1)
		for i := 0; i < 1000; i++ {
			t.produce("k", fmt.Sprint(i))
		}
		handled := map[int64]int{}
		run := func(dieAt int64) {
			p := t.parts[0]
			for {
				batch := p.read(t.committed("billing", 0), 10)
				if len(batch) == 0 {
					return
				}
				if before {
					t.commit("billing", 0, batch[len(batch)-1].offset+1)
				}
				for _, r := range batch {
					if r.offset == dieAt {
						return // dies: this batch is half done, and — if committing after — not committed
					}
					handled[r.offset]++
				}
				if !before {
					t.commit("billing", 0, batch[len(batch)-1].offset+1)
				}
			}
		}
		run(995)
		run(-1) // the replacement
		var twice, never int
		for i := int64(0); i < 1000; i++ {
			switch handled[i] {
			case 0:
				never++
			case 2:
				twice++
			}
		}
		name := "after handling a batch"
		if before {
			name = "before handling a batch"
		}
		fmt.Printf("  %-52s %16d %16d\n", name, twice, never)
	}

	// Going back: a place is only a number, and it can be set.
	t := newTopic(1)
	for i := 0; i < 1000; i++ {
		t.produce("k", fmt.Sprint(i))
	}
	n := 0
	t.consume("billing", func(int, record) { n++ })
	t.commit("billing", 0, 0)
	t.consume("billing", func(int, record) { n++ })
	fmt.Printf("\n  The group's place is set back to 0, to run a corrected version of the reader: %d records handled in all.\n", n)
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — what the log forgets.")
	fmt.Println()

	// Retention: the oldest records are dropped, whether or not anyone has read them.
	t := newTopic(1)
	p := t.parts[0]
	for i := 0; i < 1000; i++ {
		t.produce("k", fmt.Sprint(i))
	}
	n := 0
	for _, r := range p.read(0, 500) { // a reader handles 500 records, commits, and then is down for a while
		n++
		t.commit("reports", 0, r.offset+1)
	}
	for i := 1000; i < 6000; i++ {
		t.produce("k", fmt.Sprint(i))
		p.keepNewest(1000) // the log keeps its newest 1,000 records
	}
	place := t.committed("reports", 0)
	oldest := p.read(place, 1)[0].offset
	fmt.Printf("  Retention. A reader stops at offset %d. While it is away, 5,000 more records are written, and the\n", place)
	fmt.Printf("  log keeps only its newest 1,000. When the reader returns, the oldest record left is offset %d:\n", oldest)
	fmt.Printf("  %d records were dropped before it read them.\n\n", oldest-place)

	// Compaction: keep the newest record for each key.
	t = newTopic(1)
	p = t.parts[0]
	rng := rand.New(rand.NewSource(1))
	truth := map[string]string{}
	for i := 0; i < 100000; i++ {
		key, value := fmt.Sprint("user-", rng.Intn(1000)), fmt.Sprint("address ", i)
		t.produce(key, value)
		truth[key] = value
	}
	before := len(p.records)
	p.compact()
	rebuilt := map[string]string{}
	t.consume("new-service", func(_ int, r record) { rebuilt[r.key] = r.value })
	same := len(rebuilt) == len(truth)
	for k, v := range truth {
		same = same && rebuilt[k] == v
	}
	fmt.Printf("  Compaction. %d address changes for 1,000 users. After compaction the log holds %d records:\n", before, len(p.records))
	fmt.Printf("  the latest for each user. A new service reads them from the start and has every user's\n")
	fmt.Printf("  current address: %v.\n", same)
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3, part4} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
