// Chapter 9.7 — a day of load, and several ways of having enough copies to carry it.
//
//	part 1  the morning peak, met by a fixed fleet, by an autoscaler, and by a schedule
//	part 2  what an autoscaler cannot see: a signal that misleads, and a limit somewhere else
//
//	go run ./phase-9-shipping/code/ch07-capacity
//
// This is a model, and not a measurement. It steps through a day one second at a time. The
// autoscaler follows the rule that Kubernetes documents for its HorizontalPodAutoscaler: it
// looks every 15 seconds, wants ceil(copies × signal ÷ target), does nothing while the signal
// is within a tenth of the target, and removes copies only after five minutes of wanting fewer.
// Its signal is how busy the last minute's load makes the copies that are ready. Requests
// beyond what the ready copies can carry are counted as failed, and nothing worse happens to
// the rest, which is kinder than a real overload (Chapter 6.4).
package main

import (
	"fmt"
	"math"
	"slices"
)

const (
	perCopy = 100.0 // requests a second that one copy carries within its SLO: found by a load test
	fewest  = 2     // the autoscaler never goes below this
	look    = 15    // seconds between the autoscaler's looks
	day     = 24 * 3600
)

// The load through the day, in requests a second, as straight lines between these points.
var curve = []struct {
	at  string
	rps float64
}{
	{"00:00", 200}, {"07:00", 200}, {"08:55", 400}, {"09:00", 1200}, {"12:00", 1200},
	{"13:00", 900}, {"14:00", 1200}, {"17:00", 1100}, {"19:00", 300}, {"24:00", 200},
}

func clock(s string) int {
	var h, m int
	fmt.Sscanf(s, "%d:%d", &h, &m)
	return h*3600 + m*60
}

func demand(t int) float64 {
	for i := 1; i < len(curve); i++ {
		a, b := clock(curve[i-1].at), clock(curve[i].at)
		if t <= b {
			return curve[i-1].rps + (curve[i].rps-curve[i-1].rps)*float64(t-a)/float64(b-a)
		}
	}
	return curve[len(curve)-1].rps
}

// A design is one way of providing copies.
type design struct {
	what    string
	fixed   int     // a fleet of this many copies, and no autoscaler
	target  float64 // how busy the autoscaler wants the copies to be
	startup int     // seconds from ordering a copy to its carrying load
	floor   int     // a schedule: at least this many copies…
	from    string  // …from this time of day
	fullAt  float64 // what the signal reads when a copy is full; 1 for a signal that tells the truth
	dbLimit int     // the most copies that the database has connections for; 0 means no limit
}

type result struct {
	before, most int     // copies at 08:55, and the most in the day
	failed       float64 // requests that found no capacity
	total        float64 // requests in the day
	over         int     // seconds in which some requests failed
	copyHours    float64
	trace        []string
}

func run(d design) result {
	var r result
	if d.fullAt == 0 {
		d.fullAt = 1
	}
	ready := d.fixed
	if ready == 0 {
		ready = max(fewest, int(math.Ceil(demand(0)/perCopy/d.target)))
	}
	var starting []int         // when each copy that has been ordered will be ready
	var served [60]float64     // the load that was carried in each second of the last minute
	var wanted [300 / look]int // what the autoscaler wanted at each look in the last five minutes
	failedThisMinute, line := 0.0, ""

	for t := 0; t < day; t++ {
		for len(starting) > 0 && starting[0] <= t {
			ready, starting = ready+1, starting[1:]
		}
		load := demand(t)
		serving := ready
		if d.dbLimit > 0 && serving > d.dbLimit {
			serving = d.dbLimit // the rest have started, and cannot reach the database
		}
		capacity := float64(serving) * perCopy
		if load > capacity {
			r.failed += load - capacity
			failedThisMinute += load - capacity
			r.over++
		}
		r.total += load
		r.copyHours += float64(ready+len(starting)) / 3600
		r.most = max(r.most, ready)
		if t == clock("08:55") {
			r.before = ready
		}
		if t%60 == 0 {
			line = fmt.Sprintf("    %02d:%02d %8.0f %14d %10d", t/3600, t%3600/60, load, ready, len(starting))
		}
		if t%60 == 59 {
			if t >= clock("08:55") && t < clock("09:11") {
				r.trace = append(r.trace, fmt.Sprintf("%s %22s", line, commas(failedThisMinute)))
			}
			failedThisMinute = 0
		}

		served[t%60] = math.Min(load, capacity)
		if d.fixed > 0 || t%look != 0 {
			continue
		}
		// The signal: how busy the last minute's load makes the copies that are ready now, as
		// the autoscaler's metric reads it. A copy cannot be more than full.
		signal := 0.0
		for _, s := range served {
			signal += s / 60 / (float64(ready) * perCopy) * d.fullAt
		}
		all := ready + len(starting)
		want := all
		if ratio := signal / d.target; math.Abs(ratio-1) > 0.1 {
			want = int(math.Ceil(float64(ready) * ratio)) // copies that are starting count as idle
		}
		want = max(want, fewest)
		if d.floor > 0 && t >= clock(d.from) && t < clock("18:00") {
			want = max(want, d.floor)
		}
		wanted[t/look%len(wanted)] = want
		for ; all < want; all++ {
			starting = append(starting, t+d.startup)
		}
		if highest := slices.Max(wanted[:]); highest < ready && len(starting) == 0 && t > 300 {
			ready = highest // five minutes of wanting fewer: let some go
		}
	}
	return r
}

func commas(f float64) string {
	s := fmt.Sprintf("%.0f", f)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func main() {
	peak := int(math.Ceil(1200 / perCopy / 0.7))
	fmt.Println("PART 1 — one copy carries 100 requests a second. Between 08:55 and 09:00 the load triples, to 1,200.")
	fmt.Println()
	fmt.Printf("  %-50s %9s %7s %9s %8s %10s\n", "how the copies are provided", "copies at", "most", "requests", "minutes", "copy-hours")
	fmt.Printf("  %-50s %9s %7s %9s %8s %10s\n", "", "08:55", "copies", "failed", "short", "in the day")
	part1 := []design{
		{what: "a fixed fleet, sized for the peak at 70% busy", fixed: peak},
		{what: "an autoscaler: 70% busy, 3 minutes to start a copy", target: 0.7, startup: 180},
		{what: "…aiming at 50% busy", target: 0.5, startup: 180},
		{what: "…aiming at 70%, with 30 seconds to start a copy", target: 0.7, startup: 30},
		{what: fmt.Sprintf("…at 70%%, 3 minutes, and %d copies from 08:45", peak), target: 0.7, startup: 180, floor: peak, from: "08:45"},
	}
	var second result
	for i, d := range part1 {
		r := run(d)
		if i == 1 {
			second = r
		}
		fmt.Printf("  %-50s %9d %7d %9s %8.1f %10.0f\n", d.what, r.before, r.most, commas(r.failed), float64(r.over)/60, r.copyHours)
	}
	fmt.Println()
	fmt.Println("  the second row, minute by minute:")
	fmt.Printf("    %5s %8s %14s %10s %22s\n", "at", "load", "copies ready", "starting", "failed in the minute")
	for _, line := range second.trace {
		fmt.Println(line)
	}

	fmt.Println()
	fmt.Println("PART 2 — the same day and an autoscaler that is fast enough: 70% busy, 30 seconds to start a copy.")
	fmt.Println()
	fmt.Printf("  %-58s %7s %12s %14s\n", "what the autoscaler cannot see", "most", "requests", "of the day's")
	fmt.Printf("  %-58s %7s %12s %14s\n", "", "copies", "failed", "requests")
	for _, d := range []design{
		{what: "nothing: its signal is how busy a copy really is"},
		{what: "its signal is CPU, and a copy is full at 40% CPU", fullAt: 0.4},
		{what: "the database has 200 connections, and each copy takes 20", dbLimit: 200 / 20},
		{what: "…and each copy's pool is cut to 8 connections", dbLimit: 200 / 8},
	} {
		d.target, d.startup = 0.7, 30
		r := run(d)
		fmt.Printf("  %-58s %7d %12s %13.1f%%\n", d.what, r.most, commas(r.failed), 100*r.failed/r.total)
	}
}
