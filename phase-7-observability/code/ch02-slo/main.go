// Chapter 7.2 — eight weeks of a service, counted from four places, and the error budget that follows from the count.
//
// A model, not a measurement. It holds what happened in every minute of eight
// weeks: how many requests were sent, and what became of them. The first four
// weeks have six incidents. Nothing is random, so every run prints the same numbers.
//
//	part 1  the first four weeks, counted from four places
//	part 2  "fast enough", said three ways
//	part 3  the error budget of an objective, incident by incident; then two kinds of window
//	part 4  the same four weeks against other objectives
//
//	go run ./phase-7-observability/code/ch02-slo
//	go run ./phase-7-observability/code/ch02-slo -target 99.5
package main

import (
	"flag"
	"fmt"
	"math"
	"strings"
)

const (
	day     = 24 * 60 // minutes
	period  = 28      // days in a period: four weeks
	history = 28      // quiet days before day 1, so that a window which looks back has something to look at
	days    = 56      // the days that are reported: two periods

	ordinaryFailures = 0.0002 // on any day, 2 requests in 10,000 fail: a restart, a lost connection, a timeout
	ordinarySlow     = 0.0005 // …and 5 in 10,000 take more than 300 ms
)

// One minute, as it happened. Each observer can see some of these numbers and not others.
type minute struct {
	sent    int  // requests that clients sent
	reached int  // …of which the balancer received
	refused int  // …of which the balancer answered itself, with a 502 or a 503: no server could take them
	failed  int  // …of which a server answered with a 500
	slow    int  // …of which a server answered correctly, in 300 ms to 1 s
	stuck   int  // …of which a server answered correctly, in 1 s to 2 s
	probe   bool // the probe that asks for /healthz once a minute, from outside: did it get an answer?
}

type incident struct {
	day, hour, minutes int
	name, lasted       string
	surge              float64 // traffic is multiplied by this
	cut                float64 // the share of requests that cannot connect at all
	refused            float64 // the share that the balancer has no server for
	failed             float64 // the share that a server answers with an error
	slow, stuck        float64 // the shares of the correct answers that take 300 ms to 1 s, and 1 s to 2 s
}

var incidents = []incident{
	{day: 3, hour: 14, minutes: 40, lasted: "40 min", name: "a bad release: 1 request in 10 fails", failed: 0.10},
	{day: 9, hour: 3, minutes: 4, lasted: "4 min", name: "every server is down, at 03:00", refused: 1},
	{day: 16, hour: 16, minutes: 25, lasted: "25 min", name: "a surge: 1 request in 5 is refused", surge: 1.25, refused: 0.20, slow: 0.30},
	{day: 19, hour: 11, minutes: 12, lasted: "12 min", name: "the certificate has expired", cut: 1},
	{day: 22, hour: 10, minutes: 360, lasted: "6 h", name: "a dependency is slow: 2 requests in 100 fail", failed: 0.02, stuck: 0.20},
	{day: 26, hour: 15, minutes: 30, lasted: "30 min", name: "a bad release: 2 requests in 10 fail", failed: 0.20},
}

// start is the first minute of a day. Day 1 is the first day that is reported.
func start(d int) int { return (history + d - 1) * day }

func (in incident) span() (from, to int) {
	from = start(in.day) + in.hour*60
	return from, from + in.minutes
}

// A share of a whole number of requests is rarely a whole number. carry keeps what is left over
// for the next minute, so that the counts are whole and their sum is right.
type carry float64

func (c *carry) of(n int, share float64) int {
	x := float64(n)*share + float64(*c)
	k := int(x)
	*c = carry(x - float64(k))
	return k
}

func build() []minute {
	ms := make([]minute, (history+days)*day)
	in := make([]*incident, len(ms))
	for i := range incidents {
		from, to := incidents[i].span()
		for m := from; m < to; m++ {
			in[m] = &incidents[i]
		}
	}
	var refused, failed, slow, stuck, probes carry
	for i := range ms {
		m, now := &ms[i], incident{surge: 1}
		if in[i] != nil {
			now = *in[i]
			if now.surge == 0 {
				now.surge = 1
			}
		}
		// Traffic follows the clock: 100 requests a second at 16:00, 10 a second at 04:00.
		hour := float64(i%day) / 60
		rate := 600 + 5400*(1+math.Cos(2*math.Pi*(hour-16)/24))/2
		m.sent = int(rate * now.surge)
		m.reached = m.sent - int(float64(m.sent)*now.cut)
		m.refused = refused.of(m.reached, now.refused)
		served := m.reached - m.refused
		m.failed = failed.of(served, ordinaryFailures+now.failed)
		correct := served - m.failed
		m.stuck = stuck.of(correct, now.stuck)
		m.slow = slow.of(correct-m.stuck, ordinarySlow+now.slow)
		// The probe is one more request. /healthz says only that a server is running, so a server
		// that is running and answering wrongly passes. It fails when it cannot connect, and
		// when the balancer has no server for it.
		m.probe = now.cut < 1 && probes.of(1, now.refused) == 0
	}
	return ms
}

// ------------------------------------------------------------ counting

type tally struct {
	minutes, probeFailed                        int
	sent, reached, refused, failed, slow, stuck int
}

func count(ms []minute, from, to int) (t tally) {
	for _, m := range ms[from:to] {
		t.minutes++
		if !m.probe {
			t.probeFailed++
		}
		t.sent += m.sent
		t.reached += m.reached
		t.refused += m.refused
		t.failed += m.failed
		t.slow += m.slow
		t.stuck += m.stuck
	}
	return t
}

func (t tally) minus(u tally) tally {
	return tally{t.minutes - u.minutes, t.probeFailed - u.probeFailed, t.sent - u.sent, t.reached - u.reached,
		t.refused - u.refused, t.failed - u.failed, t.slow - u.slow, t.stuck - u.stuck}
}

// The four observers: what each counts as bad, and out of how many.
func (t tally) probe() (bad, all int)    { return t.probeFailed, t.minutes }
func (t tally) service() (bad, all int)  { return t.failed, t.reached - t.refused }
func (t tally) balancer() (bad, all int) { return t.refused + t.failed, t.reached }
func (t tally) clients() (bad, all int)  { return t.sent - t.reached + t.refused + t.failed, t.sent }

// out prints one line of a table, without the spaces that padding leaves at its end.
func out(format string, a ...any) {
	fmt.Println(strings.TrimRight(fmt.Sprintf(format, a...), " "))
}

func good(bad, all int) string { return fmt.Sprintf("%.2f%%", 100*float64(all-bad)/float64(all)) }

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ------------------------------------------------------------ part 1

func part1(ms []minute) {
	fmt.Println("PART 1 — four weeks, counted from four places. The sales deck says 99.99%.")
	fmt.Println()
	fmt.Printf("  %3s  %-46s %-8s %38s\n", "", "", "", "failures counted in that time, by")
	fmt.Printf("  %3s  %-46s %-8s %10s %12s %13s %12s\n", "day", "what went wrong", "for", "the probe", "the service", "the balancer", "the clients")
	row := func(d, name, lasted string, t tally) {
		p, _ := t.probe()
		s, _ := t.service()
		b, _ := t.balancer()
		c, _ := t.clients()
		fmt.Printf("  %3s  %-46s %-8s %6d min %12s %13s %12s\n", d, name, lasted, p, commas(s), commas(b), commas(c))
	}
	whole := count(ms, start(1), start(period+1))
	rest := whole
	for _, in := range incidents {
		from, to := in.span()
		t := count(ms, from, to)
		rest = rest.minus(t)
		row(fmt.Sprint(in.day), in.name, in.lasted, t)
	}
	row("", "the rest: 2 requests in 10,000 fail", "28 days", rest)
	fmt.Println()
	_, minutes := whole.probe()
	_, served := whole.service()
	_, reached := whole.balancer()
	_, sent := whole.clients()
	fmt.Printf("  %3s  %-55s %6s min %12s %13s %12s\n", "", "counted in all", commas(minutes), commas(served), commas(reached), commas(sent))
	fmt.Printf("  %3s  %-55s %10s %12s %13s %12s\n", "", "of which good", good(whole.probe()), good(whole.service()), good(whole.balancer()), good(whole.clients()))
}

// ------------------------------------------------------------ part 2

// The model has three classes of correct answer, and gives each a typical time.
const fastMs, slowMs, stuckMs = 70, 500, 1500

func part2(ms []minute) {
	fmt.Println("PART 2 — \"fast enough\", said three ways. Requests that were answered correctly, timed at the balancer.")
	fmt.Println()
	from, to := incidents[4].span()
	spans := []tally{count(ms, start(period+1), start(days+1)), count(ms, start(1), start(period+1)), count(ms, from, to)}
	out("  %-36s %20s %20s %20s", "", "four quiet weeks", "the four weeks", "day 22, 10:00–16:00")
	out("  %-36s %20s %20s %20s", "", "(days 29–56)", "of part 1", "")
	line := func(name string, value func(correct, slow, stuck int) string) {
		fmt.Printf("  %-36s", name)
		for _, t := range spans {
			fmt.Printf(" %20s", value(t.reached-t.refused-t.failed, t.slow, t.stuck))
		}
		fmt.Println()
	}
	line("mean", func(correct, slow, stuck int) string {
		return fmt.Sprintf("%.0f ms", float64((correct-slow-stuck)*fastMs+slow*slowMs+stuck*stuckMs)/float64(correct))
	})
	line("99th percentile", func(correct, slow, stuck int) string {
		switch rank := correct / 100; { // the slowest 1% begins here, counting from the slowest
		case stuck >= rank:
			return "1 s to 2 s"
		case stuck+slow >= rank:
			return "300 ms to 1 s"
		}
		return "under 300 ms"
	})
	line("answered within 300 ms", func(correct, slow, stuck int) string { return good(slow+stuck, correct) })
	line("…so this many were not", func(correct, slow, stuck int) string { return commas(slow + stuck) })
}

// ------------------------------------------------------------ part 3

// daily[d] is the tally of day d. Days 0, -1, … are the quiet days of the history.
type calendar struct {
	daily  map[int]tally
	normal tally // a day on which nothing happens
}

func byDay(ms []minute) calendar {
	c := calendar{daily: map[int]tally{}}
	for d := 1 - history; d <= days; d++ {
		c.daily[d] = count(ms, start(d), start(d+1))
	}
	c.normal = c.daily[0]
	return c
}

// rolling: the failures of the last four weeks, and the budget that those four weeks' requests allow.
func (c calendar) rolling(d int, target float64) (failures, budget float64) {
	var t tally
	for i := d - period + 1; i <= d; i++ {
		t.reached += c.daily[i].reached
		t.refused += c.daily[i].refused
		t.failed += c.daily[i].failed
	}
	bad, all := t.balancer()
	return float64(bad), (1 - target) * float64(all)
}

// fixed: the failures since the period began, and the budget of the whole period. The period is
// not over, so its requests are those so far and those expected: an ordinary day for each day left.
func (c calendar) fixed(d int, target float64) (failures, budget float64) {
	first := (d-1)/period*period + 1
	var t tally
	for i := first; i <= d; i++ {
		t.reached += c.daily[i].reached
		t.refused += c.daily[i].refused
		t.failed += c.daily[i].failed
	}
	bad, all := t.balancer()
	daysLeft := first + period - 1 - d
	return float64(bad), (1 - target) * float64(all+daysLeft*c.normal.reached)
}

func left(failures, budget float64) string { return fmt.Sprintf("%.0f%%", 100*(1-failures/budget)) }

// halted lists the days on which the budget of the window was spent, as ranges.
func halted(target float64, window func(int, float64) (float64, float64)) (ranges []string, n int) {
	from := 0
	for d := 1; d <= days+1; d++ {
		over := false
		if d <= days {
			failures, budget := window(d, target)
			over = failures > budget
		}
		switch {
		case over && from == 0:
			from = d
		case !over && from != 0:
			ranges = append(ranges, fmt.Sprintf("day %d to day %d", from, d-1))
			n += d - from
			from = 0
		}
	}
	if n == 0 {
		return []string{"never"}, 0
	}
	return ranges, n
}

func part3(ms []minute, target float64) {
	c := byDay(ms)
	whole := count(ms, start(1), start(period+1))
	_, all := whole.balancer()
	budget := (1 - target) * float64(all)
	fmt.Printf("PART 3 — the objective: %s%% of requests succeed, counted at the balancer, over four weeks.\n", trim(100*target))
	fmt.Printf("  Requests in the four weeks: %s. The error budget is %s%% of them: %s failures.\n", commas(all), trim(100*(1-target)), commas(int(budget)))
	fmt.Println()
	fmt.Printf("  %3s  %-46s %10s %15s %22s\n", "day", "what happened", "failures", "of the budget", "budget left, at the")
	fmt.Printf("  %3s  %-46s %10s %15s %22s\n", "", "", "", "", "end of that day")
	rest := whole
	for _, in := range incidents {
		from, to := in.span()
		t := count(ms, from, to)
		rest = rest.minus(t)
		bad, _ := t.balancer()
		soFar, _ := count(ms, start(1), start(in.day+1)).balancer()
		fmt.Printf("  %3d  %-46s %10s %14.0f%% %22s\n", in.day, in.name, commas(bad), 100*float64(bad)/budget, left(float64(soFar), budget))
	}
	bad, _ := rest.balancer()
	total, _ := whole.balancer()
	out("  %3s  %-46s %10s %14.0f%% %22s", "", "the rest: 2 requests in 10,000 fail", commas(bad), 100*float64(bad)/budget, "")
	fmt.Printf("  %3s  %-46s %10s %14.0f%% %22s\n", "28", "the four weeks", commas(total), 100*float64(total)/budget, left(float64(total), budget))
	fmt.Println()
	nf, nt := incidents[1].span()
	night, _ := count(ms, nf, nt).balancer()
	peak := 0
	for _, m := range ms[start(9)+16*60 : start(9)+16*60+incidents[1].minutes] {
		peak += m.reached
	}
	fmt.Printf("  Day 9's four minutes cost %s failures. The same four minutes at 16:00 would have cost %s: %.0f%% of the budget.\n", commas(night), commas(peak), 100*float64(peak)/budget)
	fmt.Println()
	fmt.Println("  The policy: while the budget of the window is spent, no releases other than fixes.")
	fmt.Println()
	out("  %-12s %-34s %-34s", "at the end", "the last four weeks (rolling)", "this period (days 1–28, then 29–56)")
	out("  %-12s %-14s %-19s %-14s %-19s", "of day", "budget left", "releases", "budget left", "releases")
	say := func(failures, budget float64) string {
		if failures > budget {
			return "halted"
		}
		return "allowed"
	}
	for d := 7; d <= days; d += 7 {
		rf, rb := c.rolling(d, target)
		ff, fb := c.fixed(d, target)
		out("  %-12d %-14s %-19s %-14s %-19s", d, left(rf, rb), say(rf, rb), left(ff, fb), say(ff, fb))
	}
	fmt.Println()
	for _, w := range []struct {
		name   string
		window func(int, float64) (float64, float64)
	}{{"rolling window", c.rolling}, {"fixed periods", c.fixed}} {
		ranges, n := halted(target, w.window)
		fmt.Printf("  releases halted, %-16s %s", w.name+":", strings.Join(ranges, ", "))
		if n > 0 {
			fmt.Printf("   (%d of the %d days)", n, days)
		}
		fmt.Println()
	}
}

func trim(x float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", x), "0"), ".")
}

// ------------------------------------------------------------ part 4

func span(minutes float64) string {
	switch m := int(math.Round(minutes)); {
	case minutes < 1:
		return fmt.Sprintf("%.0f s", minutes*60)
	case m < 60:
		return fmt.Sprintf("%d min", m)
	default:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
}

func part4(ms []minute) {
	c := byDay(ms)
	whole := count(ms, start(1), start(period+1))
	quiet := count(ms, start(period+1), start(days+1))
	bad, all := whole.balancer()
	ordinary, quietAll := quiet.balancer()
	fmt.Printf("PART 4 — the same four weeks against other objectives. The balancer counted %s good.\n", good(bad, all))
	fmt.Println()
	fmt.Printf("  %-10s %16s %16s %16s %16s %25s\n", "objective", "the budget, in", "or a complete", "spent in four", "spent in the", "days on which releases")
	fmt.Printf("  %-10s %16s %16s %16s %16s %25s\n", "", "failed requests", "outage of *", "quiet weeks", "four weeks", "were halted, of 56 **")
	for _, target := range []float64{0.99, 0.995, 0.999, 0.9995, 0.9999, 0.99999} {
		budget := (1 - target) * float64(all)
		_, n := halted(target, c.rolling)
		fmt.Printf("  %-10s %16s %16s %16s %16s %25d\n", trim(100*target)+"%", commas(int(budget)), span((1-target)*period*day),
			commas(int(math.Round(100*float64(ordinary)/((1-target)*float64(quietAll)))))+"%", commas(int(math.Round(100*float64(bad)/budget)))+"%", n)
	}
	fmt.Println()
	fmt.Println("  *  at the average rate of traffic     ** by the policy of part 3, with a rolling window")
}

func main() {
	target := flag.Float64("target", 99.9, "the objective of part 3: the percentage of requests that must succeed")
	flag.Parse()
	if *target <= 0 || *target >= 100 {
		fmt.Println("the target must be more than 0 and less than 100")
		return
	}
	ms := build()
	part1(ms)
	fmt.Println()
	part2(ms)
	fmt.Println()
	part3(ms, *target/100)
	fmt.Println()
	part4(ms)
}
