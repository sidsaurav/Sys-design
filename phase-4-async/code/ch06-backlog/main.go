// Chapter 4.6 — a queue falling behind, and what can be done about it.
//
// A queue's behaviour is arithmetic: how fast work arrives, how fast it is
// done, and for how long. So this program does not sleep. It steps a clock
// forward a tenth of a second at a time and counts, which lets it use
// realistic numbers — minutes and tens of thousands of messages — and
// finish at once.
//
//	part 1  a ten-minute burst, and what each dashboard shows
//	part 2  after an outage: four ways to work through the backlog
//	part 3  a queue with no limit, and a queue with one
//	part 4  one record that cannot be handled, in a partition of a log
//	part 5  a few urgent jobs behind sixty thousand that are not
//
//	go run ./phase-4-async/code/ch06-backlog
package main

import (
	"flag"
	"fmt"
)

const tick = 0.1 // seconds

// waiting is a group of messages that arrived at the same moment.
type waiting struct {
	since float64 // when they arrived (seconds)
	count float64
}

type queue struct {
	groups  []waiting
	limit   float64 // most messages it will hold; 0 means no limit
	refused float64
	dropped float64
	longest float64 // the longest any handled message waited
}

func (q *queue) depth() (n float64) {
	for _, g := range q.groups {
		n += g.count
	}
	return n
}

func (q *queue) add(now, count float64) {
	if q.limit > 0 {
		room := q.limit - q.depth()
		if count > room {
			q.refused += count - max(room, 0)
			count = max(room, 0)
		}
	}
	if count > 0 {
		q.groups = append(q.groups, waiting{now, count})
	}
}

// oldest reports how long the oldest waiting message has waited.
func (q *queue) oldest(now float64) float64 {
	if len(q.groups) == 0 {
		return 0
	}
	return now - q.groups[0].since
}

// take handles up to n messages: the oldest first, or the newest first.
// It returns how long the last one it handled had waited.
func (q *queue) take(now, n float64, newestFirst bool) (waited float64) {
	for n > 1e-9 && len(q.groups) > 0 {
		i := 0
		if newestFirst {
			i = len(q.groups) - 1
		}
		g := &q.groups[i]
		got := min(n, g.count)
		g.count -= got
		n -= got
		waited = now - g.since
		q.longest = max(q.longest, waited)
		if g.count <= 1e-9 {
			if newestFirst {
				q.groups = q.groups[:i]
			} else {
				q.groups = q.groups[1:]
			}
		}
	}
	return waited
}

// expire drops everything that has waited longer than ttl.
func (q *queue) expire(now, ttl float64) {
	for len(q.groups) > 0 && now-q.groups[0].since > ttl {
		q.dropped += q.groups[0].count
		q.groups = q.groups[1:]
	}
}

func clock(seconds float64) string {
	return fmt.Sprintf("%d:%02d", int(seconds)/60, int(seconds)%60)
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — workers can handle 100 messages a second. 80 a second arrive — except from minute 5 to")
	fmt.Println("         minute 15, when 150 a second arrive.")
	fmt.Println()
	fmt.Printf("  %6s %12s %12s %12s %10s %12s %22s\n", "time", "arriving /s", "handled /s", "workers busy", "errors", "waiting", "oldest has waited")
	q := &queue{}
	var handled float64
	for step := 0; step <= 50*600; step++ {
		now := float64(step) * tick
		rate := 80.0
		if now >= 300 && now < 900 {
			rate = 150
		}
		q.add(now, rate*tick)
		before := q.depth()
		q.take(now, 100*tick, false)
		handled = (before - q.depth()) / tick
		if step%(5*600) == 0 {
			fmt.Printf("  %6s %12.0f %12.0f %11.0f%% %10d %12.0f %20.0f s\n", clock(now), rate, handled, handled, 0, q.depth(), q.oldest(now))
		}
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — the workers are down for 10 minutes while 80 messages a second arrive: 48,000 are waiting")
	fmt.Println("         when they come back. Arrivals continue at 80 a second; the workers handle 100.")
	fmt.Println()
	fmt.Printf("  %-44s %26s %22s %12s\n", "the backlog is worked through...", "new messages are prompt", "longest any message", "dropped")
	fmt.Printf("  %-44s %26s %22s %12s\n", "", "again after", "waited", "")
	for _, v := range []struct {
		name        string
		newestFirst bool
		ttl         float64
		capacity    float64
	}{
		{"oldest first", false, 0, 100},
		{"newest first", true, 0, 100},
		{"oldest first, dropping what is over 60 s old", false, 60, 100},
		{"oldest first, with three times the workers", false, 0, 300},
	} {
		q := &queue{}
		for t := 0.0; t < 600; t += tick { // the outage
			q.add(t, 80*tick)
		}
		prompt := -1.0
		for step := 0; step < 80*600 && (prompt < 0 || q.depth() > 1); step++ {
			now := 600 + float64(step)*tick
			q.add(now, 80*tick)
			if v.ttl > 0 {
				q.expire(now, v.ttl)
			}
			waited := q.take(now, v.capacity*tick, v.newestFirst)
			// Is a message that arrives now handled within a second?
			fresh := waited < 1
			if !v.newestFirst {
				fresh = q.oldest(now) < 1
			}
			if fresh && prompt < 0 {
				prompt = now - 600
			}
		}
		fmt.Printf("  %-44s %22.0f min %18.0f min %12.0f\n", v.name, prompt/60, q.longest/60, q.dropped)
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — 150 messages a second arrive for 10 minutes. The workers handle 100.")
	fmt.Println()
	fmt.Printf("  %-44s %12s %22s %24s\n", "the queue...", "waiting", "oldest has waited", "refused at the door")
	for _, limit := range []float64{0, 1000} {
		q := &queue{limit: limit}
		var now float64
		for step := 0; step < 10*600; step++ {
			now = float64(step) * tick
			q.add(now, 150*tick)
			q.take(now, 100*tick, false)
		}
		name := "holds whatever it is given"
		if limit > 0 {
			name = "holds at most 1,000, and refuses the rest"
		}
		fmt.Printf("  %-44s %12.0f %20.0f s %17.0f (%.0f%%)\n", name, q.depth(), q.oldest(now), q.refused, q.refused*100/(150*600))
	}
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — one partition of a log holds 1,000 records. Record 100 cannot be handled.")
	fmt.Println()
	fmt.Printf("  %-44s %12s %18s %20s\n", "when a record fails, the reader...", "handled", "never reached", "set aside")
	for _, giveUpAfter := range []int{0, 3} {
		handled, aside, attempts := 0, 0, 0
		offset := 0
		for offset < 1000 && attempts < 10000 { // 10,000 attempts stands for "for ever"
			if offset == 99 {
				attempts++
				if giveUpAfter == 0 || attempts < giveUpAfter {
					continue // try the same record again: the offset cannot move past it
				}
				aside++ // copy it to a dead-letter topic, and move on
			} else {
				handled++
			}
			offset++
		}
		name := "tries it again, for ever"
		if giveUpAfter > 0 {
			name = "tries 3 times, then copies it elsewhere"
		}
		fmt.Printf("  %-44s %12d %18d %20d\n", name, handled, 1000-offset-0, aside)
	}
}

// ------------------------------------------------------------ part 5

func part5() {
	fmt.Println("PART 5 — 60,000 newsletter emails are queued at once. Password-reset emails arrive at one a second.")
	fmt.Println("         The workers send 100 emails a second.")
	fmt.Println()
	fmt.Printf("  %-44s %26s %26s\n", "", "a password reset waits", "the newsletter is finished")
	for _, separate := range []bool{false, true} {
		bulk, urgent := &queue{}, &queue{}
		bulk.add(0, 60000)
		var worst, finished float64
		for step := 0; finished == 0; step++ {
			now := float64(step) * tick
			target := bulk
			if separate {
				target = urgent
			}
			target.add(now, 1*tick)
			if separate {
				// Ten of the hundred workers take only urgent mail.
				urgent.take(now, 10*tick, false)
				bulk.take(now, 90*tick, false)
				worst = max(worst, urgent.longest)
				if bulk.depth() < 1 {
					finished = now
				}
			} else {
				before := bulk.depth()
				bulk.take(now, 100*tick, false)
				worst = max(worst, bulk.oldest(now))
				if before > 0 && bulk.groups[0].since > 0 { // everything queued at time 0 has gone
					finished = now
				}
			}
		}
		name := "one queue for everything"
		if separate {
			name = "a queue of its own, and 10 workers kept for it"
		}
		fmt.Printf("  %-44s %24.1f s %22.0f min\n", name, worst, finished/60)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 5)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3, part4, part5} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
