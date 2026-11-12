// Chapter 7.3 — eight weeks of a service, and seven alert rules watching it.
//
// A model, not a measurement. The service and its first six incidents are those
// of Chapter 7.2. Two things are added. Small blips, which every real service
// has: servers restart on weekdays, and one is lost twice a week. And two slow
// incidents in the second four weeks. Each rule is evaluated once a minute, as
// a monitoring system would. Nothing is random, so every run prints the same numbers.
//
//	part 1  what a burn rate means: who is told, and after how long
//	part 2  eight weeks, eight incidents, seven rules: when each rule sent a notification
//	part 3  two ways to run a pager
//
//	go run ./phase-7-observability/code/ch03-alerts
package main

import (
	"fmt"
	"math"
	"strings"
)

const (
	day     = 24 * 60 // minutes
	history = 28      // quiet days before day 1, so that a window which looks back has something to look at
	days    = 56      // the days that are reported

	objective = 0.999             // 99.9% of requests succeed, counted at the balancer, over four weeks
	allowed   = 1 - objective     // the error rate that spends the budget exactly: a burn rate of 1
	ordinary  = 0.0002            // on any day, 2 requests in 10,000 fail
	fourWeeks = 28 * day          // minutes in the objective's window
	noNotice  = math.MaxInt32     // "this rule sent nothing"
	alreadyOn = math.MaxInt32 - 1 // "this rule was firing before the incident began"
)

// One minute, as the monitoring system can see it.
type minute struct {
	reached int     // requests that the balancer received
	bad     int     // …of which it answered with a 5xx, its own or a server's
	probe   bool    // the probe that asks for /healthz once a minute, from outside: did it get an answer?
	cpu     float64 // how busy the servers are, from 0 to 1
}

type event struct {
	day, hour, min, minutes int
	name                    string
	surge                   float64 // traffic is multiplied by this
	cut                     float64 // the share of requests that cannot connect at all
	refused                 float64 // the share that the balancer has no server for
	failed                  float64 // the share that a server answers with an error
}

// The incidents: events that a person should hear about. The first six are Chapter 7.2's.
var incidents = []event{
	{day: 3, hour: 14, minutes: 40, name: "a bad release: 10% fail, for 40 min", failed: 0.10},
	{day: 9, hour: 3, minutes: 4, name: "every server is down, for 4 min", refused: 1},
	{day: 16, hour: 16, minutes: 25, name: "a surge: 20% refused, for 25 min", surge: 1.25, refused: 0.20},
	{day: 19, hour: 11, minutes: 12, name: "the certificate has expired, for 12 min", cut: 1},
	{day: 22, hour: 10, minutes: 360, name: "a dependency is failing: 2% fail, for 6 h", failed: 0.02},
	{day: 26, hour: 15, minutes: 30, name: "a bad release: 20% fail, for 30 min", failed: 0.20},
	{day: 33, hour: 9, minutes: 480, name: "a cache node is failing: 1% fail, for 8 h", failed: 0.01},
	{day: 40, hour: 0, minutes: 5 * day, name: "a slow leak: 0.25% fail, for 5 days", failed: 0.0025},
}

// The blips: events that spend almost nothing and need nobody.
func blips() (list []event) {
	for d := 1 - history; d <= days; d++ {
		switch weekday := ((d-1)%7 + 7) % 7; { // day 1 is a Monday
		case weekday < 5: // servers restart in turn: a release, or a change of configuration
			list = append(list, event{day: d, hour: 10, min: 30, minutes: 1, failed: 0.03})
		}
		switch ((d-1)%7 + 7) % 7 {
		case 1: // a server is lost, and the requests it was holding with it
			list = append(list, event{day: d, hour: 5, min: 20, minutes: 1, failed: 0.05})
		case 5:
			list = append(list, event{day: d, hour: 22, min: 40, minutes: 1, failed: 0.05})
		}
	}
	return list
}

// start is the first minute of a day. Day 1 is the first day that is reported.
func start(d int) int { return (history + d - 1) * day }

func (e event) span() (from, to int) {
	from = start(e.day) + e.hour*60 + e.min
	return from, from + e.minutes
}

// carry keeps the fraction of a request that is left over, for the next minute.
type carry float64

func (c *carry) of(n int, share float64) int {
	x := float64(n)*share + float64(*c)
	k := int(x)
	*c = carry(x - float64(k))
	return k
}

func build() []minute {
	ms := make([]minute, (history+days)*day)
	now := make([]event, len(ms)) // what is going on in each minute: the events that overlap, added up
	for i := range now {
		now[i].surge = 1
	}
	for _, e := range append(blips(), incidents...) {
		from, to := e.span()
		for i := from; i < to; i++ {
			if e.surge > 0 {
				now[i].surge = e.surge
			}
			now[i].cut += e.cut
			now[i].refused += e.refused
			now[i].failed += e.failed
		}
	}
	var refused, failed, probes carry
	for i := range ms {
		m, e := &ms[i], now[i]
		// Traffic follows the clock: 100 requests a second at 16:00, 10 a second at 04:00.
		hour := float64(i%day) / 60
		rate := 600 + 5400*(1+math.Cos(2*math.Pi*(hour-16)/24))/2
		sent := int(rate * e.surge)
		m.reached = sent - int(float64(sent)*e.cut)
		m.bad = refused.of(m.reached, e.refused)
		m.bad += failed.of(m.reached-m.bad, ordinary+e.failed)
		m.probe = e.cut < 1 && probes.of(1, e.refused) == 0
		// The servers are sized so that the afternoon peak uses 85% of them. A batch job
		// runs every night from 02:00 to 02:40 and uses most of what is free.
		m.cpu = math.Min(1, 0.20+0.65*float64(sent)/6000)
		if hour >= 2 && hour < 2+40.0/60 {
			m.cpu = math.Min(1, m.cpu+0.60)
		}
	}
	return ms
}

// ------------------------------------------------------------ the rules

// A watcher answers one question for each minute: is this rule's condition true?
type watcher struct {
	ms       []minute
	bad, all []int // running totals, so that the error rate of any window is two subtractions
}

func watch(ms []minute) *watcher {
	w := &watcher{ms: ms, bad: make([]int, len(ms)+1), all: make([]int, len(ms)+1)}
	for i, m := range ms {
		w.bad[i+1] = w.bad[i] + m.bad
		w.all[i+1] = w.all[i] + m.reached
	}
	return w
}

// rate is the error rate of the window of this many minutes that ends with minute i.
func (w *watcher) rate(i, window int) float64 {
	from := max(0, i+1-window)
	all := w.all[i+1] - w.all[from]
	if all == 0 {
		return 0 // no requests, so no rate. A rule that compares it with anything is not true
	}
	return float64(w.bad[i+1]-w.bad[from]) / float64(all)
}

// every reports whether the test holds for each of the last n minutes.
func every(i, n int, test func(int) bool) bool {
	for j := i; j > i-n; j-- {
		if j < 0 || !test(j) {
			return false
		}
	}
	return true
}

type rule struct {
	name, how string
	on        func(w *watcher, i int) bool
}

var rules = []rule{
	{"cpu", "CPU above 80% for 5 min", func(w *watcher, i int) bool {
		return every(i, 5, func(j int) bool { return w.ms[j].cpu > 0.80 })
	}},
	{"threshold", "error rate of the last 10 min at or above 0.1%", func(w *watcher, i int) bool {
		return w.rate(i, 10) >= allowed
	}},
	{"for an hour", "error rate of each minute above 0.1%, for 60 min", func(w *watcher, i int) bool {
		return every(i, 60, func(j int) bool { return w.rate(j, 1) > allowed })
	}},
	{"burn, 1 h", "error rate of the last 1 h above 14.4 × 0.1%", func(w *watcher, i int) bool {
		return w.rate(i, 60) > 14.4*allowed
	}},
	{"burn, 2 windows", "the last 1 h and 5 min above 14.4 × 0.1%, or the last 6 h and 30 min above 6 × 0.1%", func(w *watcher, i int) bool {
		return w.rate(i, 60) > 14.4*allowed && w.rate(i, 5) > 14.4*allowed ||
			w.rate(i, 360) > 6*allowed && w.rate(i, 30) > 6*allowed
	}},
	{"slow burn", "the last 3 days and the last 6 h above 0.1%", func(w *watcher, i int) bool {
		return w.rate(i, 3*day) > allowed && w.rate(i, 360) > allowed
	}},
	{"probe", "the probe failed 3 times running", func(w *watcher, i int) bool {
		return every(i, 3, func(j int) bool { return !w.ms[j].probe })
	}},
}

// A record is what one rule did in the eight weeks.
type record struct {
	on      []bool // for each minute: is the condition true?
	notices []int  // the minutes in which it became true: one notification each
	delay   []int  // for each incident: minutes from its start to the first notification during it
	after   int    // the longest that the rule went on firing after an incident it had reported was over
	quiet   int    // notifications that began when no incident was in progress
	firing  int    // minutes spent firing
}

func run(w *watcher, r rule) record {
	rec := record{on: make([]bool, len(w.ms)), delay: make([]int, len(incidents))}
	inIncident := make([]bool, len(w.ms))
	for _, in := range incidents {
		from, to := in.span()
		for i := from; i < to; i++ {
			inIncident[i] = true
		}
	}
	for i := range w.ms {
		rec.on[i] = r.on(w, i)
		if i < start(1) {
			continue
		}
		if rec.on[i] {
			rec.firing++
		}
		if rec.on[i] && !rec.on[i-1] {
			rec.notices = append(rec.notices, i)
			if !inIncident[i] {
				rec.quiet++
			}
		}
	}
	for k, in := range incidents {
		from, to := in.span()
		rec.delay[k] = noNotice
		if rec.on[from-1] {
			rec.delay[k] = alreadyOn
			continue
		}
		for i := from; i < to; i++ {
			if rec.on[i] {
				rec.delay[k] = i - from + 1 // the rule is evaluated at the end of the minute
				break
			}
		}
		if rec.delay[k] != noNotice {
			i := to
			for i < len(rec.on) && rec.on[i] {
				i++
			}
			rec.after = max(rec.after, i-to)
		}
	}
	return rec
}

// ------------------------------------------------------------ printing

func span(minutes float64) string {
	switch m := int(math.Round(minutes)); {
	case minutes < 1:
		return fmt.Sprintf("%.0f s", minutes*60)
	case m < 120:
		return fmt.Sprintf("%d min", m)
	case m < 2*day:
		return fmt.Sprintf("%d h %02d", m/60, m%60)
	case m%day < 60:
		return fmt.Sprintf("%d days", m/day)
	default:
		return fmt.Sprintf("%d d %d h", m/day, m%day/60)
	}
}

func delay(d int) string {
	switch d {
	case noNotice:
		return "—"
	case alreadyOn:
		return "(on)"
	}
	return span(float64(d))
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// out prints one line of a table, without the spaces that padding leaves at its end.
func out(format string, a ...any) {
	fmt.Println(strings.TrimRight(fmt.Sprintf(format, a...), " "))
}

// ------------------------------------------------------------ part 1

// The three tiers of the recommended rule: a long window, and how many times the allowed error rate.
var tiers = []struct {
	name   string
	window float64 // minutes
	burn   float64
}{{"a page: the 1 h window", 60, 14.4}, {"a page: the 6 h window", 360, 6}, {"a ticket: the 3 day window", 3 * day, 1}}

func part1() {
	fmt.Println("PART 1 — what a burn rate means, for an objective of 99.9% over four weeks and steady traffic.")
	fmt.Println()
	out("  %-12s %10s %16s    %-28s %10s %14s", "error rate", "burn rate", "the budget", "who is told first", "after", "budget spent")
	out("  %-12s %10s %16s    %-28s %10s %14s", "", "", "is gone in", "", "", "by then")
	for _, rate := range []float64{1, 0.20, 0.10, 0.02, 0.01, 0.0025, 0.001, 0.0005} {
		burn := rate / allowed
		gone := "never"
		if burn >= 1 {
			gone = span(fourWeeks / burn)
		}
		who, after, spent := "nobody", "—", "—"
		best := math.Inf(1)
		for _, t := range tiers {
			// The average of the long window passes the threshold when enough of the window is bad.
			if wait := t.window * t.burn * allowed / rate; rate > t.burn*allowed && wait < best {
				best = wait
				who = t.name
				after = span(wait)
				spent = fmt.Sprintf("%.1f%%", 100*burn*wait/fourWeeks)
			}
		}
		out("  %-12s %10s %16s    %-28s %10s %14s", pct(rate), commas(int(math.Round(burn*10))/10)+tenth(burn), gone, who, after, spent)
	}
}

func pct(x float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", 100*x), "0"), ".") + "%"
}

// tenth is the first decimal of a burn rate, if it has one.
func tenth(burn float64) string {
	if d := int(math.Round(burn*10)) % 10; d != 0 {
		return fmt.Sprintf(".%d", d)
	}
	return ""
}

// ------------------------------------------------------------ part 2

func part2(recs []record) {
	fmt.Println("PART 2 — eight weeks, eight incidents, seven rules. For each incident: how long until each rule sent a notification.")
	fmt.Println()
	for k, r := range rules {
		out("  %d  %-16s %s", k+1, r.name, r.how)
	}
	fmt.Println()
	head := fmt.Sprintf("  %3s  %-42s", "day", "incident")
	for k := range rules {
		head += fmt.Sprintf(" %9s", fmt.Sprintf("rule %d", k+1))
	}
	out("%s", head)
	for k, in := range incidents {
		line := fmt.Sprintf("  %3d  %-42s", in.day, in.name)
		for _, rec := range recs {
			line += fmt.Sprintf(" %9s", delay(rec.delay[k]))
		}
		out("%s", line)
	}
	fmt.Println()
	row := func(name string, value func(record) string) {
		line := fmt.Sprintf("  %3s  %-42s", "", name)
		for _, rec := range recs {
			line += fmt.Sprintf(" %9s", value(rec))
		}
		out("%s", line)
	}
	row("notifications in the eight weeks", func(r record) string { return fmt.Sprint(len(r.notices)) })
	row("…that began with no incident in progress", func(r record) string { return fmt.Sprint(r.quiet) })
	row("time spent firing", func(r record) string {
		if r.firing < 120 {
			return fmt.Sprintf("%d min", r.firing)
		}
		return fmt.Sprintf("%d h", (r.firing+30)/60)
	})
	row("still firing after an incident, at most", func(r record) string {
		if r.after == 0 {
			return "—"
		}
		return span(float64(r.after))
	})
	fmt.Println()
	fmt.Println("  (on): the rule was already firing when the incident began, so it sent nothing new.")
}

// ------------------------------------------------------------ part 3

func part3(w *watcher, recs []record) {
	fmt.Println("PART 3 — two ways to run a pager.")
	fmt.Println()
	inIncident := func(i int) bool {
		for _, in := range incidents {
			if from, to := in.span(); i >= from && i < to {
				return true
			}
		}
		return false
	}
	setup := func(name string, pages ...int) {
		total, real := 0, 0
		unpaged := []string{}
		for _, k := range pages {
			total += len(recs[k].notices)
			for _, i := range recs[k].notices {
				if inIncident(i) {
					real++
				}
			}
		}
		for k, in := range incidents {
			told := false
			for _, p := range pages {
				told = told || recs[p].delay[k] < alreadyOn
			}
			if !told {
				unpaged = append(unpaged, fmt.Sprintf("day %d", in.day))
			}
		}
		if len(unpaged) == 0 {
			unpaged = []string{"none"}
		}
		out("  %-50s %6d %10.1f %14d %14d    %s", name, total, float64(total)/8, real, total-real, strings.Join(unpaged, ", "))
	}
	out("  %-50s %6s %10s %14s %14s    %s", "", "pages", "a week", "began in an", "began in no", "no new page for")
	out("  %-50s %6s %10s %14s %14s", "", "", "", "incident", "incident")
	setup("before: rules 1 and 2 page", 0, 1)
	setup("after: rules 5 and 7 page, rule 6 opens a ticket", 4, 6)
	fmt.Println()

	// The budget of four weeks: 0.1% of the requests of an average four weeks.
	budget := allowed * float64(w.all[len(w.ms)]-w.all[start(1)]) / 2
	fmt.Println("  After, incident by incident:")
	out("  %3s  %-42s %-18s %9s %16s %16s", "day", "incident", "first told by", "after", "budget spent", "budget spent")
	out("  %3s  %-42s %-18s %9s %16s %16s", "", "", "", "", "by then", "in all")
	for k, in := range incidents {
		from, to := in.span()
		who, wait := "nobody", noNotice
		for _, c := range []struct {
			name string
			rule int
		}{{"a page: burn rate", 4}, {"a page: the probe", 6}, {"a ticket", 5}} {
			if d := recs[c.rule].delay[k]; d < wait {
				who, wait = c.name, d
			}
		}
		spent, whole := "—", "—"
		if wait < alreadyOn && in.cut < 1 {
			spent = fmt.Sprintf("%.1f%%", 100*float64(w.bad[from+wait]-w.bad[from])/budget)
			whole = fmt.Sprintf("%.0f%%", 100*float64(w.bad[to]-w.bad[from])/budget)
		}
		out("  %3d  %-42s %-18s %9s %16s %16s", in.day, in.name, who, delay(wait), spent, whole)
	}
	fmt.Println()
	fmt.Println("  Day 19 has no figures: the balancer, which the budget is counted at, received nothing.")
}

func main() {
	ms := build()
	w := watch(ms)
	recs := make([]record, len(rules))
	for k, r := range rules {
		recs[k] = run(w, r)
	}
	part1()
	fmt.Println()
	part2(recs)
	fmt.Println()
	part3(w, recs)
}
