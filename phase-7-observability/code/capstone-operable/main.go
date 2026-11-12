// Capstone 7 — the payments API of Capstone 6, for four weeks, and what its operators are told.
//
// A model, not a measurement. It holds what happened to the payments API in every
// minute of four weeks: how many payments were asked for and what became of them,
// the status queries and reports, the payments still undecided, the hourly
// comparison with the bank, and a probe from outside. Eight incidents are in it,
// each of a different kind. The worked design's objectives and alert rules are
// then evaluated over it, minute by minute. Nothing is random.
//
//	part 1  the objectives, and where the budget for payments went
//	part 2  the alerts: who was told about each incident, how soon, and what it had cost by then
//	part 3  the dashboard, at three moments of one incident
//
//	go run ./phase-7-observability/code/capstone-operable
//	go run ./phase-7-observability/code/capstone-operable -without ledger
//
// -without removes one part of the design:
//
//	split      one indicator for every kind of request, in place of one for each
//	burn-rate  a threshold on ten minutes, in place of the burn-rate rule
//	ticket     no tickets for slow burns
//	backlog    no rule on the age of the oldest undecided payment
//	ledger     no hourly comparison with the bank
//	approval   no rule on the share of payments that the bank approves
//	probe      no probe from outside
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
)

const (
	day     = 24 * 60 // minutes
	history = 28      // quiet days before day 1, for the windows to look back on
	days    = 28      // the days that are reported: four weeks
	target  = 0.999   // of payment requests, answered paid, declined or pending
	allowed = 1 - target
)

// One minute of the service.
type minute struct {
	asked, reached                           int // payment requests: sent by shops, and received by the balancer
	paid, declined, pending, refused, failed int // what they were told: 200, 402, 202, 503, 500
	doubled                                  int // paid, and charged twice: nobody is told anything
	late                                     int // answered correctly, but after more than 2 s
	status, statusBad, reports, reportsBad   int // the other two kinds of request
	undecided, oldest                        int // payments still pending at the end of the minute, and the oldest's age in minutes
	mismatches                               int // found by the comparison with the bank, which runs on the hour
	probe                                    bool
	bankMs, breakers                         int // for the dashboard: the bank's median answer, and instances with an open breaker
}

type event struct {
	day, hour, min, minutes int
	name                    string
	cut                     float64 // the share of requests that cannot connect
	refused, failed         float64 // the share of payments answered 503, and 500
	declined                float64 // the share that the bank declines, when it is not the usual 5%
	doubled                 float64 // the share charged twice
	noReconciler            bool    // nothing is settling the pending payments
	reportsBad              float64 // the share of reports refused
	bankMs, breakers        int
}

var incidents = []event{
	{day: 2, hour: 10, minutes: 30, name: "a bad release: 5% of payments fail", failed: 0.05},
	{day: 5, hour: 14, minutes: 12, name: "the bank is slow: 60% of payments refused", refused: 0.60, bankMs: 4000, breakers: 5},
	{day: 8, hour: 2, minutes: 12, name: "the certificate has expired", cut: 1},
	{day: 11, hour: 9, minutes: 360, name: "the reconciler is stopped", noReconciler: true},
	{day: 15, hour: 11, minutes: 120, name: "retries charged twice: 0.2% of payments", doubled: 0.002},
	{day: 18, hour: 16, minutes: 20, name: "a wrong field: the bank declines 45%", declined: 0.45},
	{day: 21, hour: 16, minutes: 40, name: "a zone is lost: 60% of reports refused", reportsBad: 0.60},
	{day: 23, hour: 6, minutes: 48 * 60, name: "one route to the bank: 0.2% refused", refused: 0.002},
}

// Servers restart in turn on weekdays at 10:30. For a minute, 2% of payments fail.
func blips() (list []event) {
	for d := 1 - history; d <= days; d++ {
		if ((d-1)%7+7)%7 < 5 {
			list = append(list, event{day: d, hour: 10, min: 30, minutes: 1, failed: 0.02})
		}
	}
	return list
}

func start(d int) int { return (history + d - 1) * day }

func (e event) span() (from, to int) {
	from = start(e.day) + e.hour*60 + e.min
	return from, from + e.minutes
}

type carry float64

func (c *carry) of(n int, share float64) int {
	x := float64(n)*share + float64(*c)
	k := int(x)
	*c = carry(x - float64(k))
	return k
}

func build() []minute {
	ms := make([]minute, (history+days)*day)
	now := make([]event, len(ms))
	for _, e := range append(blips(), incidents...) {
		from, to := e.span()
		for i := from; i < to; i++ {
			n := &now[i]
			n.cut += e.cut
			n.refused += e.refused
			n.failed += e.failed
			n.doubled += e.doubled
			n.reportsBad += e.reportsBad
			n.noReconciler = n.noReconciler || e.noReconciler
			if e.declined > 0 {
				n.declined = e.declined
			}
			if e.bankMs > 0 {
				n.bankMs, n.breakers = e.bankMs, e.breakers
			}
		}
	}
	var refused, failed, pending, declined, doubled, late, reportsBad, probes carry
	var waiting []int // the pending payments of each minute that nothing has settled yet
	found := 0        // charged twice, and not yet seen by the comparison with the bank
	for i := range ms {
		m, e := &ms[i], now[i]
		// Payments follow the clock: 300 a second at 16:00, 30 a second at 04:00.
		hour := float64(i%day) / 60
		m.asked = int(60 * (30 + 270*(1+math.Cos(2*math.Pi*(hour-16)/24))/2))
		m.reached = m.asked - int(float64(m.asked)*e.cut)
		m.refused = refused.of(m.reached, e.refused)
		m.failed = failed.of(m.reached-m.refused, 0.0001+e.failed)
		rest := m.reached - m.refused - m.failed
		m.pending = pending.of(rest, 0.01) // the bank charged or did not, and its answer was lost
		share := 0.05
		if e.declined > 0 {
			share = e.declined
		}
		m.declined = declined.of(rest-m.pending, share)
		m.paid = rest - m.pending - m.declined
		m.doubled = doubled.of(m.paid, e.doubled)
		m.late = late.of(rest, 0.005)
		m.status = 3 * m.reached
		m.statusBad = m.status / 20000
		m.reports = 180
		if e.cut >= 1 {
			m.reports = 0
		}
		m.reportsBad = reportsBad.of(m.reports, 0.001+e.reportsBad)
		m.probe = e.cut < 1 && probes.of(1, e.refused+e.failed) == 0
		m.bankMs, m.breakers = 300, 0
		if e.bankMs > 0 {
			m.bankMs, m.breakers = e.bankMs, e.breakers
		}
		// The reconciler asks the bank about every pending payment, and settles it within the minute.
		waiting = append(waiting, m.pending)
		if !e.noReconciler {
			waiting = waiting[len(waiting)-1:]
		}
		for _, n := range waiting {
			m.undecided += n
		}
		m.oldest = len(waiting) - 1
		// The comparison with the bank's record runs on the hour, over the hour that has ended.
		found += m.doubled
		if i%60 == 59 {
			m.mismatches, found = found, 0
		}
	}
	return ms
}

// ------------------------------------------------------------ rules

type sums struct{ bad, all []int }

func total(ms []minute, bad, all func(m minute) int) sums {
	s := sums{make([]int, len(ms)+1), make([]int, len(ms)+1)}
	for i, m := range ms {
		s.bad[i+1] = s.bad[i] + bad(m)
		s.all[i+1] = s.all[i] + all(m)
	}
	return s
}

// rate is the share of bad in the window of this many minutes that ends with minute i.
func (s sums) rate(i, window int) float64 {
	from := max(0, i+1-window)
	if s.all[i+1] == s.all[from] {
		return 0
	}
	return float64(s.bad[i+1]-s.bad[from]) / float64(s.all[i+1]-s.all[from])
}

func (s sums) burning(i int, allowed float64) bool { // the two paging pairs of windows
	return s.rate(i, 60) > 14.4*allowed && s.rate(i, 5) > 14.4*allowed ||
		s.rate(i, 360) > 6*allowed && s.rate(i, 30) > 6*allowed
}

func (s sums) leaking(i int, allowed float64) bool { // the ticket's pair
	return s.rate(i, 3*day) > allowed && s.rate(i, 360) > allowed
}

type rule struct {
	name, kind string // kind: "page" or "ticket"
	on         func(i int) bool
}

func rules(ms []minute, without string) []rule {
	payments := total(ms, func(m minute) int { return m.refused + m.failed }, func(m minute) int { return m.reached })
	if without == "split" { // every kind of request in one indicator
		payments = total(ms, func(m minute) int { return m.refused + m.failed + m.statusBad + m.reportsBad },
			func(m minute) int { return m.reached + m.status + m.reports })
	}
	reports := total(ms, func(m minute) int { return m.reportsBad }, func(m minute) int { return m.reports })
	approved := total(ms, func(m minute) int { return m.paid }, func(m minute) int { return m.paid + m.declined })
	list := []rule{
		{"burn rate", "page", func(i int) bool { return payments.burning(i, allowed) }},
		{"slow burn", "ticket", func(i int) bool { return payments.leaking(i, allowed) }},
		{"reports", "ticket", func(i int) bool { return reports.burning(i, 0.01) }},
		{"undecided", "page", func(i int) bool { return ms[i].oldest >= 10 }},
		{"the ledger", "page", func(i int) bool { return ms[i-i%60+59-60].mismatches > 0 }},
		{"approvals", "page", func(i int) bool {
			decided := approved.all[i+1] - approved.all[i+1-5]
			return decided > 0 && approved.rate(i, 5) < 0.7*approved.rate(i-7*day, 5)
		}},
		{"the probe", "page", func(i int) bool { return !ms[i].probe && !ms[i-1].probe && !ms[i-2].probe }},
	}
	drop := map[string]string{"backlog": "undecided", "ledger": "the ledger", "approval": "approvals", "probe": "the probe"}
	var kept []rule
	for _, r := range list {
		switch {
		case without == "ticket" && r.kind == "ticket", drop[without] == r.name:
			continue
		case without == "burn-rate" && r.name == "burn rate":
			r = rule{"threshold", "page", func(i int) bool { return payments.rate(i, 10) >= allowed }}
		}
		kept = append(kept, r)
	}
	return kept
}

// ------------------------------------------------------------ printing

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func span(m int) string {
	switch {
	case m < 120:
		return fmt.Sprintf("%d min", m)
	case m < 2*day:
		return fmt.Sprintf("%d h %02d", m/60, m%60)
	}
	return fmt.Sprintf("%d d %d h", m/day, m%day/60)
}

func out(format string, a ...any) {
	fmt.Println(strings.TrimRight(fmt.Sprintf(format, a...), " "))
}

func count(ms []minute, from, to int, f func(m minute) int) (n int) {
	for _, m := range ms[from:to] {
		n += f(m)
	}
	return n
}

// ------------------------------------------------------------ part 1

func part1(ms []minute) {
	from, to := start(1), start(days+1)
	fmt.Println("PART 1 — four weeks, against the objectives.")
	fmt.Println()
	out("  %-58s %10s %12s %14s", "objective", "target", "achieved", "budget spent")
	line := func(name string, target float64, bad, all int) {
		out("  %-58s %9.2f%% %11.3f%% %13.0f%%", name, 100*target, 100*(1-float64(bad)/float64(all)), 100*float64(bad)/((1-target)*float64(all)))
	}
	reached := count(ms, from, to, func(m minute) int { return m.reached })
	bad := count(ms, from, to, func(m minute) int { return m.refused + m.failed })
	answered := reached - bad
	line("payments answered: paid, declined or pending", target, bad, reached)
	line("payments answered within 2 s", 0.99, count(ms, from, to, func(m minute) int { return m.late }), answered)
	line("status queries answered", 0.9995, count(ms, from, to, func(m minute) int { return m.statusBad }), count(ms, from, to, func(m minute) int { return m.status }))
	line("reports answered", 0.99, count(ms, from, to, func(m minute) int { return m.reportsBad }), count(ms, from, to, func(m minute) int { return m.reports }))
	out("  %-58s %10s %12s", "payments charged twice, or charged and not recorded", "none", commas(count(ms, from, to, func(m minute) int { return m.doubled })))
	fmt.Println()
	budget := allowed * float64(reached)
	fmt.Printf("  Payments asked for: %s. The budget for the first objective is 0.1%% of them: %s.\n", commas(reached), commas(int(budget)))
	fmt.Println()
	out("  %3s  %-44s %12s %16s", "day", "where it went", "payments", "of the budget")
	rest := bad
	for _, in := range incidents {
		f, t := in.span()
		n := count(ms, f, t, func(m minute) int { return m.refused + m.failed })
		if in.refused+in.failed == 0 {
			continue
		}
		rest -= n
		out("  %3d  %-44s %12s %15.0f%%", in.day, in.name, commas(n), 100*float64(n)/budget)
	}
	out("  %3s  %-44s %12s %15.0f%%", "", "everything else: restarts, ordinary failures", commas(rest), 100*float64(rest)/budget)
}

// ------------------------------------------------------------ part 2

// harm says what an incident had cost, from its start to minute `to`, in the unit that fits it.
func harm(ms []minute, in event, from, to int) string {
	n := func(f func(m minute) int) string { return commas(count(ms, from, to, f)) }
	switch {
	case in.cut > 0:
		return n(func(m minute) int { return m.asked - m.reached }) + " could not connect"
	case in.noReconciler:
		return fmt.Sprintf("%s undecided, oldest %s", commas(ms[to-1].undecided), span(ms[to-1].oldest))
	case in.doubled > 0:
		return n(func(m minute) int { return m.doubled }) + " charged twice"
	case in.declined > 0: // more than the usual 5%
		return n(func(m minute) int { return m.declined - (m.paid+m.declined)/20 }) + " wrongly declined"
	case in.reportsBad > 0:
		return n(func(m minute) int { return m.reportsBad }) + " reports refused"
	}
	return n(func(m minute) int { return m.refused + m.failed }) + " payments failed"
}

func part2(ms []minute, list []rule, without string) (problems int) {
	fmt.Println("PART 2 — the alerts. Who was told about each incident, how soon, and what it had cost by then.")
	fmt.Println()
	on := make([][]bool, len(list))
	for k, r := range list {
		on[k] = make([]bool, len(ms))
		for i := start(1) - 1; i < len(ms); i++ {
			on[k][i] = r.on(i)
		}
	}
	inIncident := make([]bool, len(ms)+1)
	for _, in := range incidents {
		from, to := in.span()
		for i := from; i < to; i++ {
			inIncident[i] = true
		}
	}
	out("  %3s  %-42s %-18s %8s  %-28s %s", "day", "incident", "first told by", "after", "the cost by then", "the cost in all")
	unreported := 0
	for _, in := range incidents {
		from, to := in.span()
		who, when := "nobody", -1
		for k, r := range list {
			for i := from; i < to; i++ {
				if on[k][i] && !on[k][from-1] {
					if when < 0 || i < when {
						who, when = r.kind+": "+r.name, i
					}
					break
				}
			}
		}
		after, cost := "—", "—"
		if when >= 0 {
			after, cost = span(when-from+1), harm(ms, in, from, when+1)
		}
		note := ""
		if when < 0 {
			unreported++
			note = "   ← NOBODY WAS TOLD"
		}
		out("  %3d  %-42s %-18s %8s  %-28s %s%s", in.day, in.name, who, after, cost, harm(ms, in, from, to), note)
	}
	pages, tickets, forNothing, nights := 0, 0, 0, map[int]bool{}
	var sent []string
	for i := start(1); i < len(ms); i++ {
		for k, r := range list {
			if !on[k][i] || on[k][i-1] {
				continue
			}
			if r.kind == "ticket" {
				tickets++
				continue
			}
			pages++
			if len(sent) < 12 {
				sent = append(sent, fmt.Sprintf("day %d %02d:%02d %s", i/day-history+1, i%day/60, i%60, r.name))
			}
			if !inIncident[i] {
				forNothing++
			}
			if h := i % day / 60; h >= 22 || h < 7 {
				nights[(i+2*60)/day] = true // a night runs from 22:00 to 07:00
			}
		}
	}
	fmt.Println()
	plural := func(n int, word string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", word)
		}
		return fmt.Sprintf("%d %ss", n, word)
	}
	fmt.Printf("  In four weeks: %s and %s. Nights with a page: %d.\n", plural(pages, "page"), plural(tickets, "ticket"), len(nights))
	if pages > len(sent) {
		sent = append(sent, fmt.Sprintf("and %d more", pages-len(sent)))
	}
	for i := 0; i < len(sent); i += 4 {
		fmt.Printf("    %s\n", strings.Join(sent[i:min(i+4, len(sent))], "; "))
	}
	fmt.Println()
	fmt.Println("CHECKS")
	check := func(name string, n int) {
		fmt.Printf("  %-62s %d\n", name, n)
		problems += n
	}
	check("incidents that nobody was told about", unreported)
	check("pages that began with no incident in progress", forNothing)
	return problems
}

// ------------------------------------------------------------ part 3

func part3(ms []minute) {
	in := incidents[1]
	from, _ := in.span()
	moments := []int{from - 5, from + 3, from + in.minutes + 15}
	payments := total(ms, func(m minute) int { return m.refused + m.failed }, func(m minute) int { return m.reached })
	spent := func(i int) float64 {
		w := start(1)
		return float64(payments.bad[i+1]-payments.bad[w]) / (allowed * float64(payments.all[start(days+1)]-payments.all[w]))
	}
	fmt.Printf("PART 3 — the dashboard on day %d: five minutes before the bank slowed, three minutes in, and fifteen minutes after it recovered.\n", in.day)
	fmt.Println()
	row := func(name string, value func(i int) string) {
		line := fmt.Sprintf("  %-50s", name)
		for _, i := range moments {
			line += fmt.Sprintf(" %18s", value(i))
		}
		out("%s", line)
	}
	row("", func(i int) string { return fmt.Sprintf("%02d:%02d", i%day/60, i%60) })
	row("payments answered, last 5 min", func(i int) string { return fmt.Sprintf("%.2f%%", 100*(1-payments.rate(i, 5))) })
	row("burn rate: last hour, and last 5 min", func(i int) string {
		return fmt.Sprintf("%.1f and %.1f", payments.rate(i, 60)/allowed, payments.rate(i, 5)/allowed)
	})
	row("budget of the four weeks, spent so far", func(i int) string { return fmt.Sprintf("%.0f%%", 100*spent(i)) })
	row("payments a second: paid", func(i int) string { return fmt.Sprint(ms[i].paid / 60) })
	row("                   declined", func(i int) string { return fmt.Sprint(ms[i].declined / 60) })
	row("                   pending", func(i int) string { return fmt.Sprint(ms[i].pending / 60) })
	row("                   refused: nothing was charged", func(i int) string { return fmt.Sprint(ms[i].refused / 60) })
	row("the bank: median answer", func(i int) string { return fmt.Sprintf("%d ms", ms[i].bankMs) })
	row("          instances with the breaker open, of 6", func(i int) string { return fmt.Sprint(ms[i].breakers) })
	row("payments undecided, and the oldest", func(i int) string { return fmt.Sprintf("%s, under 1 min", commas(ms[i].undecided)) })
	row("status queries answered, last 5 min", func(i int) string {
		s := total(ms[i-4:i+1], func(m minute) int { return m.statusBad }, func(m minute) int { return m.status })
		return fmt.Sprintf("%.2f%%", 100*(1-s.rate(4, 5)))
	})
	row("the probe", func(i int) string {
		if ms[i].probe {
			return "passing"
		}
		return "FAILING"
	})
	row("the last release", func(i int) string { return fmt.Sprintf("10:30, %s ago", span(i%day-(10*60+30))) })
}

func main() {
	without := flag.String("without", "", "remove one part of the design: split, burn-rate, ticket, backlog, ledger, approval or probe")
	flag.Parse()
	switch *without {
	case "", "split", "burn-rate", "ticket", "backlog", "ledger", "approval", "probe":
	default:
		fmt.Println("-without takes one of: split, burn-rate, ticket, backlog, ledger, approval, probe")
		os.Exit(2)
	}
	ms := build()
	if *without != "" {
		fmt.Printf("WITHOUT %s\n\n", *without)
	}
	part1(ms)
	fmt.Println()
	problems := part2(ms, rules(ms, *without), *without)
	fmt.Println()
	part3(ms)
	if problems > 0 {
		os.Exit(1)
	}
}
