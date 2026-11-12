// Chapter 9.8 — a cloud bill that tripled, read four ways.
//
//	part 1  the bill as the provider prints it, and by the tags on its resources
//	part 2  the same money, once every line has an owner: what grew, and what a customer costs
//	part 3  what one more line of logging costs: measured, and not guessed
//	part 4  what more redundancy costs, and what a nine is worth
//	part 5  five changes, and what each saves
//
//	go run ./phase-9-shipping/code/ch08-cost
//
// The usage and the prices are invented for the example: round numbers, in pounds. Put your
// own provider's price list in their place. The arithmetic is the point. Part 3 is measured:
// it runs a real logger and counts the bytes.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Prices, in pounds.
const (
	copyHour = 0.10 // one copy of a service, for an hour
	dbHour   = 1.50 // one unit of database server, for an hour
	gbStored = 0.02 // a gigabyte kept for the month
	gbOut    = 0.08 // a gigabyte sent out to the internet
	gbZones  = 0.02 // a gigabyte sent between zones: charged on the way out and on the way in
	gbLogs   = 0.50 // a gigabyte of logs taken in, indexed and kept
	hours    = 730  // in a month
)

var (
	customers = [2]float64{2000, 4000} // last June, this June
	calls     = [2]float64{3e9, 6e9}   // calls in a month, from outside and between services
)

type shares map[string]float64

func only(owner string) [2]shares { return [2]shares{{owner: 1}, {owner: 1}} }

// How shared things are divided between the features that use them, last June and this June.
var (
	byCopies  = [2]shares{{"invoicing": 18, "payments": 10, "reminders": 6, "search": 8}, {"invoicing": 30, "payments": 16, "reminders": 10, "search": 14, "exports": 45}}
	byQueries = [2]shares{{"invoicing": 55, "payments": 20, "reminders": 10, "search": 15}, {"invoicing": 45, "payments": 15, "reminders": 5, "search": 10, "exports": 25}}
	byLogs    = [2]shares{{"invoicing": 50, "payments": 25, "reminders": 10, "search": 15}, {"invoicing": 40, "payments": 20, "reminders": 10, "search": 10, "exports": 20}}
	byAnswers = [2]shares{{"invoicing": 60, "payments": 15, "search": 25}, {"invoicing": 60, "payments": 15, "search": 25}}
)

const (
	idle  = "environments that sit idle"
	debug = "debug logging"
	zones = "traffic between zones"
	plan  = "support plan"
)

// An item is one line of usage: what the provider measured, and what it charges for it.
type item struct {
	service string     // the heading that the provider puts it under
	name    string     // the resource
	tag     string     // its "feature" tag, if anyone gave it one
	units   [2]float64 // how much was used: last June, this June
	price   float64    // pounds for each unit
	owner   [2]shares  // what it is really for: worked out afterwards, by people
}

func items(infoGB, debugGB [2]float64) []item {
	c, d := func(copies float64) float64 { return copies * hours }, func(units float64) float64 { return units * hours }
	return []item{
		{"compute", "invoicing-api", "invoicing", [2]float64{c(18), c(30)}, copyHour, only("invoicing")},
		{"compute", "payments-api", "payments", [2]float64{c(10), c(16)}, copyHour, only("payments")},
		{"compute", "reminders-worker", "reminders", [2]float64{c(6), c(10)}, copyHour, only("reminders")},
		{"compute", "search-api", "search", [2]float64{c(8), c(14)}, copyHour, only("search")},
		{"compute", "export-worker", "", [2]float64{0, c(45)}, copyHour, only("exports")},
		{"compute", "staging", "", [2]float64{c(24), c(40)}, copyHour, only(idle)},
		{"compute", "load-test, left running since March", "", [2]float64{0, c(30)}, copyHour, only(idle)},
		{"compute", "the cluster's own: system, gateway, proxies", "", [2]float64{c(14), c(20)}, copyHour, byCopies},
		{"database", "main database", "", [2]float64{d(4), d(6)}, dbHour, byQueries},
		{"database", "staging database", "", [2]float64{d(1), d(1)}, dbHour, only(idle)},
		{"database", "load-test database", "", [2]float64{0, d(1)}, dbHour, only(idle)},
		{"storage", "invoice PDFs", "invoicing", [2]float64{30000, 60000}, gbStored, only("invoicing")},
		{"storage", "export files, kept for ever", "", [2]float64{0, 330000}, gbStored, only("exports")},
		{"storage", "database backups", "", [2]float64{80000, 150000}, gbStored, byQueries},
		{"storage", "log archive", "", [2]float64{20000, 50000}, gbStored, byLogs},
		{"data transfer", "answers to callers, out", "", [2]float64{8000, 15000}, gbOut, byAnswers},
		{"data transfer", "export downloads, out", "", [2]float64{0, 60000}, gbOut, only("exports")},
		{"data transfer", "between zones", "", [2]float64{25000, 150000}, gbZones, only(zones)},
		{"logs, metrics, traces", "logs at INFO", "", infoGB, gbLogs, byLogs},
		{"logs, metrics, traces", "logs at DEBUG, left on since February", "", debugGB, gbLogs, only(debug)},
		{"logs, metrics, traces", "metrics and traces", "", [2]float64{2000, 3500}, 1, byLogs},
		{"support plan", "support plan", "", [2]float64{2000, 3000}, 1, only(plan)},
	}
}

func (it item) cost(month int) float64 { return it.units[month] * it.price }

// sum adds up the items under a key, for both months, and returns the keys in their first order.
func sum(its []item, key func(item, int) shares) (keys []string, total map[string][2]float64) {
	total = map[string][2]float64{}
	for _, it := range its {
		for month := 0; month < 2; month++ {
			sh, all := key(it, month), 0.0
			for _, s := range sh {
				all += s
			}
			names := make([]string, 0, len(sh))
			for name := range sh {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if _, seen := total[name]; !seen {
					keys = append(keys, name)
				}
				t := total[name]
				t[month] += it.cost(month) * sh[name] / all
				total[name] = t
			}
		}
	}
	return keys, total
}

func money(f float64) string {
	s := fmt.Sprintf("%.0f", f)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func main() {
	info, debugLine := measure()
	gb := func(bytes float64, month int) float64 { return bytes * calls[month] / 1e9 }
	its := items([2]float64{gb(info, 0), gb(info, 1)}, [2]float64{0, gb(debugLine, 1)})
	var bill [2]float64
	for _, it := range its {
		bill[0], bill[1] = bill[0]+it.cost(0), bill[1]+it.cost(1)
	}

	fmt.Println("PART 1 — the bill for June, in pounds, as the provider prints it. (Usage and prices are invented.)")
	fmt.Println()
	fmt.Printf("  %-34s %10s %10s %9s\n", "", "last June", "this June", "grew by")
	keys, total := sum(its, func(it item, _ int) shares { return shares{it.service: 1} })
	for _, k := range keys {
		fmt.Printf("  %-34s %10s %10s %8.1f×\n", k, money(total[k][0]), money(total[k][1]), total[k][1]/total[k][0])
	}
	fmt.Printf("  %-34s %10s %10s %8.1f×\n", "total", money(bill[0]), money(bill[1]), bill[1]/bill[0])
	fmt.Println()
	fmt.Println("  this June again, by the \"feature\" tag on each resource:")
	keys, total = sum(its, func(it item, _ int) shares {
		if it.tag == "" {
			return shares{"(no tag)": 1}
		}
		return shares{it.tag: 1}
	})
	sort.SliceStable(keys, func(a, b int) bool { return keys[b] == "(no tag)" })
	for _, k := range keys {
		fmt.Printf("  %-34s %10s %10s %8.0f%%\n", k, "", money(total[k][1]), 100*total[k][1]/bill[1])
	}

	fmt.Println()
	fmt.Println("PART 2 — the same money, once each line has an owner and shared things are divided by use.")
	fmt.Println()
	fmt.Printf("  %-34s %10s %10s %10s %14s\n", "what the money goes on", "last June", "this June", "change", "of the growth")
	keys, owned := sum(its, func(it item, month int) shares { return it.owner[month] })
	sort.SliceStable(keys, func(a, b int) bool { return rank(keys[a]) < rank(keys[b]) })
	for _, k := range keys {
		grew := owned[k][1] - owned[k][0]
		fmt.Printf("  %-34s %10s %10s %10s %13.0f%%\n", k, money(owned[k][0]), money(owned[k][1]), "+"+money(grew), 100*grew/(bill[1]-bill[0]))
	}
	fmt.Printf("  %-34s %10s %10s %10s\n", "total", money(bill[0]), money(bill[1]), "+"+money(bill[1]-bill[0]))
	fmt.Printf("  %-34s %10s %10s\n", "customers", money(customers[0]), money(customers[1]))
	fmt.Printf("  %-34s %10.2f %10.2f\n", "for each customer", bill[0]/customers[0], bill[1]/customers[1])
	fmt.Println()
	const users, standard, business = 120, 25.0, 99.0 // customers who use exports; the two prices, a month
	rest := (bill[1] - owned["exports"][1]) / customers[1]
	fmt.Printf("  %-34s %10s %10s %10s %14s\n", "this June, a customer who", "how many", "costs", "pays", "leaves")
	fmt.Printf("  %-34s %10s %10.2f %10.2f %+14.2f\n", "does not use exports", money(customers[1]-users), rest, standard, standard-rest)
	fmt.Printf("  %-34s %10d %10.2f %10.2f %+14.2f\n", "uses exports", users, rest+owned["exports"][1]/users, business, business-rest-owned["exports"][1]/users)

	fmt.Println()
	fmt.Println("PART 3 — what a line of logging costs. Measured: a real logger, and a count of the bytes it wrote.")
	fmt.Println()
	fmt.Printf("  %-34s %10s %18s %12s\n", "one call, through one service", "bytes", "a month of calls", "costs")
	fmt.Printf("  %-34s %10s %15s GB %12s\n", "at INFO: one line", money(info), money(gb(info, 1)), money(gb(info, 1)*gbLogs))
	fmt.Printf("  %-34s %10s %15s GB %12s\n", "at DEBUG: ten lines more", money(debugLine), money(gb(debugLine, 1)), money(gb(debugLine, 1)*gbLogs))

	fmt.Println()
	fmt.Println("PART 4 — what more redundancy for invoicing costs, and what the SRE book's sum says a nine is worth.")
	fmt.Println()
	revenue := (customers[1]-users)*standard + users*business
	designs := []struct {
		what                    string
		copies, db, zone, inter float64 // copies; units of database; GB between zones; GB between regions
	}{
		{"one zone: 15 copies, one database", 15, 4, 0, 0},
		{"three zones: 18 copies, and a standby database", 18, 8, 50000, 0},
		{"two regions, each like the row above", 36, 16, 100000, 30000},
	}
	fmt.Printf("  %-48s %12s %16s\n", "the design", "costs", "more than the")
	fmt.Printf("  %-48s %12s %16s\n", "", "a month", "row above")
	before := 0.0
	for i, d := range designs {
		cost := d.copies*hours*copyHour + d.db*hours*dbHour + d.zone*gbZones + d.inter*2*gbZones // between regions costs twice as much
		more := ""
		if i > 0 {
			more = "+" + money(cost-before)
		}
		fmt.Println(strings.TrimRight(fmt.Sprintf("  %-48s %12s %16s", d.what, money(cost), more), " "))
		before = cost
	}
	fmt.Println()
	fmt.Printf("  %-48s %12s %16s\n", "an availability of", "allows, in a", "the step to it")
	fmt.Printf("  %-48s %12s %16s\n", "", "month, down", "is worth")
	for i, a := range []float64{0.999, 0.9999, 0.99999} {
		worth := ""
		if i > 0 {
			worth = fmt.Sprintf("%.0f a month", revenue*(a-[]float64{0.999, 0.9999}[i-1]))
		}
		fmt.Println(strings.TrimRight(fmt.Sprintf("  %-48s %8.1f min %16s", strings.TrimRight(fmt.Sprintf("%.3f", a*100), "0")+"%", (1-a)*hours*60, worth), " "))
	}
	fmt.Printf("  (revenue is %s a month; the sum is revenue × the difference in availability)\n", money(revenue))

	fmt.Println()
	fmt.Println("PART 5 — five changes to this June's bill, in the order of what each saves.")
	fmt.Println()
	find := func(name string) float64 {
		for _, it := range its {
			if strings.HasPrefix(it.name, name) {
				return it.cost(1)
			}
		}
		panic(name)
	}
	always := 0.0 // what runs day and night for the features: their copies, the cluster's own, the main database
	for _, name := range []string{"invoicing-api", "payments-api", "reminders-worker", "search-api", "export-worker", "the cluster", "main database"} {
		always += find(name)
	}
	changes := []struct {
		what  string
		saves float64
	}{
		{"turn debug logging off", find("logs at DEBUG")},
		{"delete the load-test environment; run staging 60 hours a week", find("load-test,") + find("load-test database") + (find("staging")+find("staging database"))*(1-60.0/168)},
		{"delete export files after 30 days, and not never", find("export files") * (1 - 1.0/8)},
		{"commit for a year to what always runs: 30% off 70% of it", always * 0.7 * 0.3},
		{"send a call to a copy in the caller's own zone when there is one", find("between zones") * 0.8},
	}
	sort.SliceStable(changes, func(a, b int) bool { return changes[a].saves > changes[b].saves })
	fmt.Printf("  %-66s %8s %9s %9s\n", "the change", "saves", "the bill", "for each")
	fmt.Printf("  %-66s %8s %9s %9s\n", "", "", "is then", "customer")
	left := bill[1]
	fmt.Printf("  %-66s %8s %9s %9.2f\n", "(as it is)", "", money(left), left/customers[1])
	for _, ch := range changes {
		left -= ch.saves
		fmt.Printf("  %-66s %8s %9s %9.2f\n", ch.what, money(ch.saves), money(left), left/customers[1])
	}
}

// rank puts the features first, then the things that are nobody's feature.
func rank(owner string) int {
	for i, o := range []string{"invoicing", "payments", "reminders", "search", "exports", idle, debug, zones, plan} {
		if o == owner {
			return i
		}
	}
	return 99
}
