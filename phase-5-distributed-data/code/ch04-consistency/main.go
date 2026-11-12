// Chapter 5.4 — what a reader may see, under three consistency models, and during a partition.
//
// A model in one process. Time is a counter of ticks; a leader and two
// followers hold one value each per key; replication takes a few ticks.
//
//	part 1  no faults: three ways to read, and what each lets a client observe
//	part 2  the network splits for a while: refuse requests, or answer them?
//
//	go run ./phase-5-distributed-data/code/ch04-consistency
package main

import (
	"flag"
	"fmt"
	"math/rand"
)

// ------------------------------------------------------------ part 1

type pending struct {
	version int
	due     int // the tick at which a follower has applied it
}

type follower struct {
	version int
	queue   []pending
}

func (f *follower) advance(now int) {
	for len(f.queue) > 0 && f.queue[0].due <= now {
		f.version = f.queue[0].version
		f.queue = f.queue[1:]
	}
}

type reader struct {
	lastWrite int // the newest version this client wrote
	lastRead  int // the version this client read last time
	newest    int // the newest version this client has seen or written
}

func part1() {
	fmt.Println("PART 1 — one value, a leader and two followers that are 1 to 6 ticks behind. Four clients write and")
	fmt.Println("         read for 50,000 ticks. Nothing fails. What can a client observe?")
	fmt.Println()
	fmt.Printf("  %-46s %14s %14s %14s %16s\n", "a read is answered by...", "missed a", "went back", "missed the", "needed a")
	fmt.Printf("  %-46s %14s %14s %14s %16s\n", "", "finished write", "in time", "reader's own", "second server")
	fmt.Printf("  %-46s %14s %14s %14s %16s\n", "", "", "", "write", "")
	for _, mode := range []string{"any replica", "any replica that has what this client has seen", "the leader"} {
		rng := rand.New(rand.NewSource(1))
		leader := 0
		followers := []*follower{{}, {}}
		clients := make([]reader, 4)
		var reads, stale, backwards, ownMissing, second int
		for tick := 0; tick < 50000; tick++ {
			for _, f := range followers {
				f.advance(tick)
			}
			finished := leader // every write up to here has been confirmed to its client
			for c := range clients {
				cl := &clients[c]
				if rng.Intn(5) == 0 { // a write: to the leader, confirmed at once
					leader++
					cl.lastWrite, cl.newest = leader, leader
					for _, f := range followers {
						due := tick + 1 + rng.Intn(6)
						if n := len(f.queue); n > 0 && f.queue[n-1].due > due {
							due = f.queue[n-1].due // changes are applied in order
						}
						f.queue = append(f.queue, pending{leader, due})
					}
					continue
				}
				reads++
				var got int
				switch pick := rng.Intn(3); {
				case mode == "the leader" || pick == 0:
					got = leader
				default:
					got = followers[pick-1].version
					if mode != "any replica" && got < cl.newest {
						second++ // this replica is behind what the client has seen: ask the leader instead
						got = leader
					}
				}
				if got < finished {
					stale++
				}
				if got < cl.lastRead {
					backwards++
				}
				if got < cl.lastWrite {
					ownMissing++
				}
				cl.lastRead = got
				cl.newest = max(cl.newest, got)
			}
		}
		pct := func(n int) string { return fmt.Sprintf("%.1f%%", float64(n)*100/float64(reads)) }
		fmt.Printf("  %-46s %14s %14s %14s %16s\n", mode, pct(stale), pct(backwards), pct(ownMissing), pct(second))
	}
}

// ------------------------------------------------------------ part 2

type write struct {
	tick int
	side int
}

func part2() {
	fmt.Println("PART 2 — three replicas, each with a client beside it, and 50 keys. For 400 of 1,000 ticks the network")
	fmt.Println("         separates replica 1 from replicas 2 and 3.")
	fmt.Println()
	fmt.Printf("  %-52s %14s %24s %24s\n", "during the split...", "requests", "confirmed writes that", "reads that missed a")
	fmt.Printf("  %-52s %14s %24s %24s\n", "", "refused", "were later discarded", "confirmed write")
	for _, answerAnyway := range []bool{false, true} {
		rng := rand.New(rand.NewSource(2))
		const keys = 50
		// Each side's view of each key: the tick of the newest write it has.
		var view [2][keys]int
		var history [keys][]write // confirmed writes to each key during the split
		var requests, refused, staleReads int
		for tick := 1; tick <= 1000; tick++ {
			split := tick > 300 && tick <= 700
			if !split { // connected: both sides have everything
				for k := 0; k < keys; k++ {
					newest := max(view[0][k], view[1][k])
					view[0][k], view[1][k] = newest, newest
				}
			}
			for client := 0; client < 3; client++ {
				side := 0 // client 0 is beside replica 1; clients 1 and 2 are beside replicas 2 and 3
				if client > 0 {
					side = 1
				}
				requests++
				k := rng.Intn(keys)
				isWrite := rng.Intn(10) < 3
				if split && side == 0 && !answerAnyway {
					refused++ // replica 1 cannot reach a majority, and says so
					continue
				}
				if isWrite {
					view[side][k] = tick
					if split {
						history[k] = append(history[k], write{tick, side})
					} else {
						view[1-side][k] = tick // connected: the other side has it at once
					}
				} else if view[side][k] < max(view[0][k], view[1][k]) {
					staleReads++ // the other side has a newer confirmed write
				}
			}
		}
		// When the split ends, the later write to each key wins. Writes on the
		// other side that the winner never saw are gone.
		discarded := 0
		for k := 0; k < keys; k++ {
			if len(history[k]) == 0 {
				continue
			}
			winner := history[k][len(history[k])-1].side
			for _, w := range history[k] {
				if w.side != winner {
					discarded++
				}
			}
		}
		name := "the side without a majority refuses requests"
		if answerAnyway {
			name = "every replica goes on answering"
		}
		fmt.Printf("  %-52s %13.0f%% %24d %24d\n", name, float64(refused)*100/float64(requests), discarded, staleReads)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 or 2)")
	flag.Parse()
	for i, run := range []func(){part1, part2} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
