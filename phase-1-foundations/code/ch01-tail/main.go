// Chapter 1.1 — why the mean hides the tail, and why fan-out makes the tail
// everybody's problem.
//
// Run:  go run ./phase-1-foundations/code/ch01-tail
//
// The backend modelled here is the one from "The Tail at Scale" (Dean and
// Barroso, 2013): it usually answers in about 10 ms, but 1 request in 100
// takes a full second.
package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

const (
	samples  = 200_000
	slowProb = 0.01 // 1 call in 100 is slow
	slow     = 1000 * time.Millisecond
)

// rng is seeded with constants so every run prints the same numbers.
var rng = rand.New(rand.NewPCG(1, 2))

// backendCall returns how long one call to one backend takes.
func backendCall() time.Duration {
	if rng.Float64() < slowProb {
		return slow
	}
	// The usual case: somewhere between 5 ms and 15 ms.
	return 5*time.Millisecond + time.Duration(rng.Int64N(int64(10*time.Millisecond)))
}

// percentile returns the value below which p percent of the sorted samples
// fall (nearest-rank method).
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func mean(xs []time.Duration) time.Duration {
	var sum time.Duration
	for _, x := range xs {
		sum += x
	}
	return sum / time.Duration(len(xs))
}

func main() {
	// Part 1: one backend. Compare the mean with the percentiles.
	lat := make([]time.Duration, samples)
	for i := range lat {
		lat[i] = backendCall()
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })

	slowCalls := 0
	for _, d := range lat {
		if d >= slow {
			slowCalls++
		}
	}

	fmt.Println("One backend, 200,000 calls")
	fmt.Printf("  mean   %8s   <- no real request took this long\n", mean(lat).Round(time.Millisecond))
	for _, p := range []float64{50, 90, 99, 99.9} {
		fmt.Printf("  p%-5v %8s\n", p, percentile(lat, p).Round(time.Millisecond))
	}
	fmt.Printf("  calls that took a full second: %.2f%%\n", float64(slowCalls)/samples*100)

	// Part 2: a user request that needs answers from n backends in parallel
	// is as slow as the slowest of them.
	fmt.Println()
	fmt.Println("A user request that waits for n backends in parallel")
	fmt.Println("  n     slower than 1s (simulated)   1 - 0.99^n (predicted)")
	for _, n := range []int{1, 10, 30, 100} {
		slowRequests := 0
		const requests = 50_000
		for range requests {
			var worst time.Duration
			for range n {
				if d := backendCall(); d > worst {
					worst = d
				}
			}
			if worst >= slow {
				slowRequests++
			}
		}
		simulated := float64(slowRequests) / requests * 100
		predicted := (1 - math.Pow(1-slowProb, float64(n))) * 100
		fmt.Printf("  %-5d %6.1f%%                       %6.1f%%\n", n, simulated, predicted)
	}
}
