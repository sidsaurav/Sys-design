// Chapter 6.6 — a region is lost. What it costs in writes, in time, and on the way back.
//
// A model, with no network and no clock: every run prints the same numbers.
// The distances, rates and delays are assumptions, stated in the output.
//
//	part 1  replicating to a second region: what a failover loses, and what losing nothing costs
//	part 2  moving the users: how long, by DNS and by a balancer, and what is waiting for them
//	part 3  failing back: the writes that only the old region has
//
//	go run ./phase-6-reliability/code/ch06-regions
package main

import (
	"flag"
	"fmt"
	"math/rand"
)

// ------------------------------------------------------------ part 1

func part1() {
	const (
		perSecond = 500 // writes a second
		local     = 5   // ms: a write, in its own region
		roundTrip = 80  // ms: to the other region and back
	)
	fmt.Println("PART 1 — a database in region A takes 500 writes a second, each in 5 ms, and replicates to region B,")
	fmt.Println("         80 ms away there and back. Region A is lost.")
	fmt.Println()
	fmt.Printf("  %-64s %16s %24s\n", "", "a write takes", "confirmed writes that")
	fmt.Printf("  %-64s %16s %24s\n", "", "", "region B does not have")
	for _, c := range []struct {
		name  string
		lagMs int // how far B is behind at the moment of the failure; -1: A waits for B
	}{
		{"asynchronous; B is 100 ms behind", 100},
		{"asynchronous; B is 1 s behind", 1000},
		{"asynchronous; B had fallen 30 s behind, and nobody was looking", 30000},
		{"synchronous: A confirms a write when B has it", -1},
	} {
		// Writes arrive every 2 ms for a minute; then A is gone. B has what was sent to it more than lagMs ago.
		lost, takes := 0, local
		for at := 0; at < 60000; at += 1000 / perSecond {
			if c.lagMs >= 0 && at >= 60000-c.lagMs {
				lost++
			}
		}
		if c.lagMs < 0 {
			takes = local + roundTrip
		}
		fmt.Printf("  %-64s %13d ms %24d\n", c.name, takes, lost)
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	const (
		users   = 100000
		each    = 0.01 // requests a second from one user: 1,000 a second in all
		noticed = 30   // seconds until health checks have marked region A as down
	)
	fmt.Println("PART 2 — 100,000 users, 1,000 requests a second, all served by region A. It is lost at second 0, and")
	fmt.Println("         health checks take 30 seconds to say so. Then users are sent to region B.")
	fmt.Println()
	fmt.Printf("  %-58s %20s %20s %20s\n", "how users are sent to a region", "half have moved", "all have moved", "requests that")
	fmt.Printf("  %-58s %20s %20s %20s\n", "", "after", "after", "went nowhere")
	rng := rand.New(rand.NewSource(6))
	for _, c := range []struct {
		name string
		ttl  int // seconds for which a user's resolver keeps the answer; 0: no DNS change is needed
	}{
		{"by DNS; the answer may be kept for 300 seconds", 300},
		{"by DNS; the answer may be kept for 60 seconds", 60},
		{"through a balancer in front of both regions", 0},
	} {
		moved := make([]int, 0, users) // for each user: the second at which their requests reach region B
		failed := 0.0
		for u := 0; u < users; u++ {
			at := noticed
			if c.ttl > 0 {
				// The user's resolver last asked at some moment in the past TTL, and asks again when that answer expires.
				// Only an answer given after the health checks have noticed points to region B.
				for at = rng.Intn(c.ttl); at < noticed; at += c.ttl {
				}
			}
			moved = append(moved, at)
			failed += float64(at) * each
		}
		half, all := 0, 0
		counts := make([]int, 2*300+noticed+1)
		for _, at := range moved {
			counts[at]++
			all = max(all, at)
		}
		for n, s := 0, 0; n*2 < users; s++ {
			n += counts[s]
			half = s
		}
		fmt.Printf("  %-58s %18d s %18d s %20.0f\n", c.name, half, all, failed)
	}

	fmt.Println()
	fmt.Println("  What is waiting for them in region B, once they have all arrived: 1,000 requests a second.")
	fmt.Println()
	fmt.Printf("  %-58s %20s %20s\n", "region B, before the failure", "requests refused", "full service after")
	for _, c := range []struct {
		name    string
		can     float64 // requests a second that region B can do when the users arrive
		minutes int     // minutes until it can do 1,000
	}{
		{"was serving half the users, and can serve them all", 1000, 0},
		{"a small copy that can do 200; it takes 10 minutes to grow", 200, 10},
		{"data only; servers must be started: 30 minutes", 0, 30},
	} {
		refused := (1000 - c.can) * 60 * float64(c.minutes)
		after := "at once"
		if c.minutes > 0 {
			after = fmt.Sprintf("%d minutes", c.minutes)
		}
		fmt.Printf("  %-58s %20.0f %20s\n", c.name, refused, after)
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	const (
		accounts  = 100000
		perSecond = 500
		lagMs     = 1000
	)
	fmt.Println("PART 3 — 100,000 accounts; 500 writes a second, each to an account chosen at random. Region A is cut off")
	fmt.Println("         with B one second behind, so A holds 500 writes that B never received. B takes over. Later A")
	fmt.Println("         comes back, with those 500 writes.")
	fmt.Println()
	fmt.Printf("  %-34s %24s %32s %26s\n", "region B has been taking", "writes B has taken", "of A's 500: the account has", "of A's 500: can simply")
	fmt.Printf("  %-34s %24s %32s %26s\n", "writes for", "since", "been written again in B", "be applied in B")
	for _, minutes := range []int{1, 10, 60, 24 * 60} {
		rng := rand.New(rand.NewSource(6))
		stranded := make([]int, perSecond*lagMs/1000) // the accounts that A's unreplicated writes touched
		for i := range stranded {
			stranded[i] = rng.Intn(accounts)
		}
		since := perSecond * 60 * minutes
		touched := make([]bool, accounts)
		for i := 0; i < since; i++ {
			touched[rng.Intn(accounts)] = true
		}
		conflicts := 0
		for _, a := range stranded {
			if touched[a] {
				conflicts++
			}
		}
		name := fmt.Sprintf("%d minutes", minutes)
		switch {
		case minutes == 1:
			name = "1 minute"
		case minutes == 60:
			name = "1 hour"
		case minutes > 60:
			name = fmt.Sprintf("%d hours", minutes/60)
		}
		fmt.Printf("  %-34s %24d %32d %26d\n", name, since, conflicts, len(stranded)-conflicts)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 3)")
	flag.Parse()
	for i, f := range []func(){part1, part2, part3} {
		if *part == 0 || *part == i+1 {
			f()
			fmt.Println()
		}
	}
}
