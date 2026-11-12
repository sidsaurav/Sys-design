// Chapter 7.6 — the arithmetic of being on call.
//
// Not a simulation: a calculator. It turns three rules from Google's SRE book
// into numbers for a team of a given size and a pager of a given loudness.
//
//	part 1  the rota: how much of the year each person is on call
//	part 2  the shift: how many incidents one person can take
//	part 3  two pagers: Chapter 7.3's rules before and after, in nights of sleep
//
//	go run ./phase-7-observability/code/ch06-oncall
//	go run ./phase-7-observability/code/ch06-oncall -people 5 -incidents 3
package main

import (
	"flag"
	"fmt"
	"math"
)

const (
	onCallAtOnce = 2  // a primary and a secondary, as the SRE book assumes
	maxOnCall    = 25 // the book's rule: no more than 25% of a person's time on call
	hoursEach    = 6  // the book's figure: what one incident costs, from the page to the last follow-up
	perShift     = 2  // …and so its limit: two incidents in a shift of twelve hours
	weeks        = 52
)

// poisson is the chance of exactly k events when the mean is m and events arrive independently.
func poisson(k int, m float64) float64 {
	p := math.Exp(-m)
	for i := 1; i <= k; i++ {
		p *= m / float64(i)
	}
	return p
}

// moreThan is the chance of more than k events.
func moreThan(k int, m float64) float64 {
	sum := 0.0
	for i := 0; i <= k; i++ {
		sum += poisson(i, m)
	}
	return 1 - sum
}

func part1(people int) {
	fmt.Println("PART 1 — the rota. Shifts of a week, with two people on call at a time: a primary and a secondary.")
	fmt.Println()
	fmt.Printf("  %-8s %18s %20s %22s\n", "people", "share of the year", "weeks on call", "within the 25% rule?")
	fmt.Printf("  %-8s %18s %20s\n", "", "on call", "a year, each")
	sizes := []int{3, 4, 5, 6, 8, 10, 12}
	listed := false
	for _, n := range sizes {
		listed = listed || n == people
	}
	if !listed {
		sizes = append(sizes, people)
	}
	for _, n := range sizes {
		share := 100 * float64(onCallAtOnce) / float64(n)
		verdict := "yes"
		if share > maxOnCall {
			verdict = "no"
		}
		mark := ""
		if n == people {
			mark = "   ← this team"
		}
		fmt.Printf("  %-8d %17.0f%% %20.1f %22s%s\n", n, share, float64(weeks*onCallAtOnce)/float64(n), verdict, mark)
	}
}

func part2(incidents float64) {
	fmt.Println("PART 2 — the shift. Incidents that need a person, arriving independently, at a steady rate.")
	fmt.Println()
	fmt.Printf("  %-12s %14s %18s %20s %24s\n", "incidents", "days with", "shifts of 12 h", "hours of work that", "…as a share of a")
	fmt.Printf("  %-12s %14s %18s %20s %24s\n", "a week", "none", "with more than 2", "they make, a week", "working week of 40 h")
	rates := []float64{0.5, 1, 2, 4, 7, 14}
	listed := false
	for _, r := range rates {
		listed = listed || r == incidents
	}
	if !listed {
		rates = append(rates, incidents)
	}
	for _, r := range rates {
		mark := ""
		if r == incidents {
			mark = "   ← this team"
		}
		work := r * hoursEach
		fmt.Printf("  %-12g %13.0f%% %17.1f%% %18.0f h %23.0f%%%s\n", r, 100*poisson(0, r/7), 100*moreThan(perShift, r/14), work, 100*work/40, mark)
	}
	fmt.Println()
	fmt.Printf("  The book asks that the median day has no incident. That holds while fewer than %.1f arrive in a week.\n", 7*math.Ln2)
}

// A pager is what one set of alert rules sent in the eight weeks of Chapter 7.3.
type pager struct {
	name          string
	pages, nights int // pages in the eight weeks, and nights (22:00 to 07:00) with at least one page
}

// The counts come from that chapter's example. Before: 168 pages, and one of them at 02:05 on
// every one of the 56 nights, when the batch job set off the CPU rule. After: 8 pages, of
// which the only ones at night were for the four minutes with every server down, at 03:00 on day 9.

func part3(people int) {
	fmt.Println("PART 3 — two pagers. The eight weeks of Chapter 7.3, for the person who is primary.")
	fmt.Println()
	pagers := []pager{
		{"before: a CPU rule and a threshold on errors", 168, 56},
		{"after: a burn-rate rule and a probe", 8, 1},
	}
	fmt.Printf("  %-48s %12s %16s %24s\n", "", "pages", "nights broken,", "nights broken a year,")
	fmt.Printf("  %-48s %12s %16s %24s\n", "", "a week", "of 7", fmt.Sprintf("each, in a team of %d", people))
	for _, p := range pagers {
		fmt.Printf("  %-48s %12.1f %16.1f %24.0f\n", p.name, float64(p.pages)/8, float64(p.nights)/8, float64(p.nights)/8*weeks/float64(people))
	}
}

func main() {
	people := flag.Int("people", 4, "how many people share the rota")
	incidents := flag.Float64("incidents", 2, "incidents that need a person, in a week")
	flag.Parse()
	if *people < onCallAtOnce || *incidents <= 0 {
		fmt.Println("the rota needs at least two people, and the rate of incidents must be more than 0")
		return
	}
	part1(*people)
	fmt.Println()
	part2(*incidents)
	fmt.Println()
	part3(*people)
}
