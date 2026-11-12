// Chapter 5.5 — clocks that disagree, and a lock that two clients hold at once.
//
//	part 1  a lock with a time limit, a client that pauses, and a fencing token
//	part 2  measuring a duration across a moment when the clock is corrected
//	part 3  putting events from three machines in order: by their clocks, and by a counter
//
// Part 1 runs real goroutines with real (short) waits. Parts 2 and 3 use
// clocks that the program controls, since a program cannot — and should
// not — reset the clock of the machine it runs on.
//
//	go run ./phase-5-distributed-data/code/ch05-time
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// ------------------------------------------------------------ part 1: a lease, and a pause

// lockService grants a lock for a limited time: a lease. If the holder does
// not renew or release it, it expires, and someone else may take it.
type lockService struct {
	mu      sync.Mutex
	holder  string
	expires time.Time
	token   int64 // goes up by one every time the lock is granted
}

func (l *lockService) acquire(who string, lease time.Duration) (token int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" && time.Now().Before(l.expires) {
		return 0, false
	}
	l.holder, l.expires = who, time.Now().Add(lease)
	l.token++
	return l.token, true
}

func (l *lockService) release(who string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == who {
		l.holder = ""
	}
}

// storage is what the lock protects: one value, which only the lock's
// holder is supposed to write.
type storage struct {
	mu       sync.Mutex
	fenced   bool
	highest  int64 // the highest token it has seen
	value    string
	rejected int
}

func (s *storage) write(value string, token int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fenced && token < s.highest {
		s.rejected++ // somebody with a newer token has been here: this writer's lock is out of date
		return false
	}
	s.highest = max(s.highest, token)
	s.value = value
	return true
}

func part1() {
	fmt.Println("PART 1 — a lock is granted for 20 ms at a time. Client A takes it, checks that it holds it, and writes.")
	fmt.Println("         In 20 rounds of 100, A is paused for 60 ms between the check and the write — a garbage")
	fmt.Println("         collection, a frozen virtual machine. Client B is waiting for the lock.")
	fmt.Println()
	fmt.Printf("  %-44s %28s %30s\n", "the storage...", "times B's finished write", "writes refused as")
	fmt.Printf("  %-44s %28s %30s\n", "", "was overwritten by A's", "out of date")
	for _, fenced := range []bool{false, true} {
		lock, store := &lockService{}, &storage{fenced: fenced}
		overwritten := 0
		for round := 0; round < 100; round++ {
			pause := round%5 == 0
			token, _ := lock.acquire("A", 20*time.Millisecond)
			// A has the lock, and has checked. Now it does its work...
			bDone := make(chan struct{})
			go func() { // ...while B waits for the lock, takes it when it can, writes, and lets go
				defer close(bDone)
				for {
					if t, ok := lock.acquire("B", 20*time.Millisecond); ok {
						store.write(fmt.Sprint("B's result, round ", round), t)
						lock.release("B")
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
			if pause {
				time.Sleep(60 * time.Millisecond) // A is frozen. Its lease runs out. It does not know
				<-bDone
			}
			accepted := store.write(fmt.Sprint("A's result, round ", round), token)
			if pause && accepted {
				overwritten++ // B had the lock and finished; A's stale write replaced B's
			}
			lock.release("A")
			<-bDone
		}
		name := "accepts any write"
		if fenced {
			name = "refuses a token older than one it has seen"
		}
		fmt.Printf("  %-44s %28d %30d\n", name, overwritten, store.rejected)
	}
}

// ------------------------------------------------------------ part 2: a clock that is corrected

// machine has two clocks, as real ones do. The wall clock is what the time
// is believed to be, and can be corrected. The monotonic clock only counts.
type machine struct {
	monotonic time.Duration // time since the machine started
	offset    time.Duration // the wall clock's error, until someone corrects it
}

func (m *machine) wall() time.Duration  { return m.monotonic + m.offset }
func (m *machine) pass(d time.Duration) { m.monotonic += d }

func part2() {
	fmt.Println("PART 2 — a machine's clock is 800 ms fast. A request starts; 50 ms later, while it is still running,")
	fmt.Println("         the clock is corrected; 50 ms after that the request ends. How long did it take?")
	fmt.Println()
	m := &machine{offset: 800 * time.Millisecond}
	wallStart, monoStart := m.wall(), m.monotonic
	m.pass(50 * time.Millisecond)
	m.offset = 0 // the correction: the wall clock jumps back 800 ms
	m.pass(50 * time.Millisecond)
	fmt.Printf("  by the wall clock:        %6d ms\n", (m.wall() - wallStart).Milliseconds())
	fmt.Printf("  by the monotonic clock:   %6d ms\n", (m.monotonic - monoStart).Milliseconds())

	// The same mistake, in a lease.
	m = &machine{offset: 800 * time.Millisecond}
	granted := m.wall()
	lease := 500 * time.Millisecond
	m.offset = 0 // corrected just after the lease was granted
	held := time.Duration(0)
	for m.wall() < granted+lease { // "do I still hold the lock?", asked of the wall clock
		m.pass(10 * time.Millisecond)
		held += 10 * time.Millisecond
	}
	fmt.Printf("\n  A lock is granted for 500 ms, and the holder checks its wall clock to see whether it still has it.\n")
	fmt.Printf("  The clock is corrected just after. The holder goes on believing it has the lock for %d ms.\n", held.Milliseconds())
}

// ------------------------------------------------------------ part 3: order

type event struct {
	message int
	receive bool // false: the message was sent; true: it was received
	wall    time.Duration
	lamport int
	node    int
}

func part3() {
	fmt.Println("PART 3 — three machines exchange 2,000 messages. Their clocks are 40 ms slow, right, and 25 ms fast.")
	fmt.Println("         Every send and every receive is logged, and the logs are merged into one, in order.")
	fmt.Println()
	rng := rand.New(rand.NewSource(5))
	offsets := []time.Duration{-40 * time.Millisecond, 0, 25 * time.Millisecond}
	counters := make([]int, 3)
	var log []event
	now := time.Duration(0)
	for m := 0; m < 2000; m++ {
		now += time.Duration(1+rng.Intn(20)) * time.Millisecond
		from := rng.Intn(3)
		to := (from + 1 + rng.Intn(2)) % 3
		counters[from]++ // a logical clock: count, and send the count along
		log = append(log, event{m, false, now + offsets[from], counters[from], from})
		arrives := now + time.Duration(1+rng.Intn(5))*time.Millisecond
		counters[to] = max(counters[to], counters[from]) + 1 // on receiving: go past whatever the sender had reached
		log = append(log, event{m, true, arrives + offsets[to], counters[to], to})
	}
	backwards := func(less func(a, b event) bool) (n int) {
		sorted := append([]event(nil), log...)
		sort.SliceStable(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
		sent := map[int]bool{}
		for _, e := range sorted {
			if !e.receive {
				sent[e.message] = true
			} else if !sent[e.message] {
				n++ // received, according to the merged log, before it was sent
			}
		}
		return n
	}
	fmt.Printf("  %-44s %40s\n", "the merged log is ordered by...", "messages received before they were sent")
	fmt.Printf("  %-44s %40d\n", "each machine's wall clock", backwards(func(a, b event) bool { return a.wall < b.wall }))
	fmt.Printf("  %-44s %40d\n", "a counter carried on every message", backwards(func(a, b event) bool {
		if a.lamport != b.lamport {
			return a.lamport < b.lamport
		}
		return a.node < b.node
	}))
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 3)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
