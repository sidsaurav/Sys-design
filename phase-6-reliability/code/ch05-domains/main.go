// Chapter 6.5 — copies that fail together, and how far one fault reaches.
//
// A model, with no network and no clock: every run prints the same numbers.
//
//	part 1  three copies of some data: in one rack, in three racks, in three zones
//	part 2  a zone is lost: capacity that is already there, and capacity that has to be asked for
//	part 3  one customer's requests break every server that handles them: who else is affected
//
// The failure rates in part 1 are assumptions, chosen to be easy to follow.
// They are not measurements of any real data centre.
//
//	go run ./phase-6-reliability/code/ch05-domains
package main

import (
	"flag"
	"fmt"
	"math/rand"
)

// ------------------------------------------------------------ part 1

const (
	zones        = 3
	racksPerZone = 10
	perRack      = 10
	years        = 50
	hours        = years * 8760
)

// A thing that can fail: a machine, a rack or a zone.
type part struct {
	everyHours float64 // it fails once in this many hours, on average
	forHours   int     // and is down for this long
	downUntil  int
}

func (p *part) step(now int, rng *rand.Rand) bool {
	if now >= p.downUntil && rng.Float64() < 1/p.everyHours {
		p.downUntil = now + p.forHours
	}
	return now < p.downUntil
}

type placement struct {
	name        string
	copies      [][3]int // for each piece of data, the three machines that hold it
	none, fewer int      // hours in which a piece of data had no copy up; and fewer than two
}

func part1() {
	fmt.Println("PART 1 — three zones of ten racks of ten machines, for 50 years, an hour at a time. Assumed: a machine is")
	fmt.Println("         down for 2 hours, four times a year; a rack for 2 hours, once a year; a zone for 4 hours, once in")
	fmt.Println("         five years. 1,000 pieces of data, each kept on three machines.")
	fmt.Println()
	rng := rand.New(rand.NewSource(5))
	machines := zones * racksPerZone * perRack
	machine := func(zone, rack, n int) int { return (zone*racksPerZone+rack)*perRack + n }
	layouts := []*placement{
		{name: "one copy (for comparison)"},
		{name: "three copies, in one rack"},
		{name: "three copies, in three racks of one zone"},
		{name: "three copies, in three zones"},
	}
	for i := 0; i < 1000; i++ {
		z, r := rng.Intn(zones), rng.Perm(racksPerZone)
		m := rng.Perm(perRack)
		one := machine(z, r[0], m[0])
		layouts[0].copies = append(layouts[0].copies, [3]int{one, one, one})
		layouts[1].copies = append(layouts[1].copies, [3]int{machine(z, r[0], m[0]), machine(z, r[0], m[1]), machine(z, r[0], m[2])})
		layouts[2].copies = append(layouts[2].copies, [3]int{machine(z, r[0], m[0]), machine(z, r[1], m[1]), machine(z, r[2], m[2])})
		layouts[3].copies = append(layouts[3].copies, [3]int{machine(0, r[0], m[0]), machine(1, r[1], m[1]), machine(2, r[2], m[2])})
	}
	ms := make([]part, machines)
	rs := make([]part, zones*racksPerZone)
	zs := make([]part, zones)
	for i := range ms {
		ms[i] = part{everyHours: 8760 / 4, forHours: 2}
	}
	for i := range rs {
		rs[i] = part{everyHours: 8760, forHours: 2}
	}
	for i := range zs {
		zs[i] = part{everyHours: 5 * 8760, forHours: 4}
	}
	down := make([]bool, machines)
	for now := 0; now < hours; now++ {
		anyDown := false
		for z := range zs {
			zoneDown := zs[z].step(now, rng)
			for r := 0; r < racksPerZone; r++ {
				rackDown := rs[z*racksPerZone+r].step(now, rng)
				for n := 0; n < perRack; n++ {
					i := machine(z, r, n)
					down[i] = ms[i].step(now, rng) || rackDown || zoneDown
					anyDown = anyDown || down[i]
				}
			}
		}
		if !anyDown {
			continue
		}
		for _, l := range layouts {
			for _, c := range l.copies {
				up := 0
				for _, m := range c {
					if !down[m] {
						up++
					}
				}
				if up == 0 {
					l.none++
				}
				if up < 2 {
					l.fewer++
				}
			}
		}
	}
	fmt.Printf("  %-44s %28s %30s\n", "", "no copy can be reached:", "fewer than two can be reached:")
	fmt.Printf("  %-44s %28s %30s\n", "", "hours a year", "hours a year")
	for i, l := range layouts {
		perYear := func(n int) string { return fmt.Sprintf("%.3f", float64(n)/float64(len(l.copies))/years) }
		quorum := perYear(l.fewer)
		if i == 0 {
			quorum = "—"
		}
		fmt.Printf("  %-44s %28s %30s\n", l.name, perYear(l.none), quorum)
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — a service whose busiest hour needs 1,000 requests a second, spread evenly over its zones. One")
	fmt.Println("         zone is lost.")
	fmt.Println()
	fmt.Printf("  %-8s %30s %24s %30s\n", "zones", "each zone must be able to do", "capacity to pay for,", "each zone is this busy")
	fmt.Printf("  %-8s %30s %24s %30s\n", "", "(requests a second)", "against the peak", "at the peak, on a normal day")
	for _, n := range []int{2, 3, 4, 6} {
		each := 1000.0 / float64(n-1)
		fmt.Printf("  %-8d %30.0f %23.0f%% %29.0f%%\n", n, each, 100*each*float64(n)/1000, 100*float64(n-1)/float64(n))
	}
	fmt.Println()
	fmt.Println("  Three zones, and the peak. The zone is lost at minute 0. Servers that refuse what they cannot do (Chapter 6.4).")
	fmt.Println()
	fmt.Printf("  %-64s %18s %22s\n", "", "requests refused", "full service again")
	for _, c := range []struct {
		name    string
		perZone float64 // what one zone can do
		arrives int     // minutes until replacement capacity is serving; -1 if none is needed
	}{
		{"each zone can do 500: nothing needs to happen", 500, -1},
		{"each zone can do 400; new servers are serving after 5 minutes", 400, 5},
		{"each zone can do 400; nothing can be launched for 66 minutes", 400, 66 + 5},
	} {
		refused, minute := 0.0, 0
		for capacity := 2 * c.perZone; capacity < 1000; minute++ {
			refused += (1000 - capacity) * 60
			if minute+1 >= c.arrives {
				capacity = 3 * c.perZone
			}
		}
		again := "at once"
		if minute > 0 {
			again = fmt.Sprintf("after %d minutes", minute)
		}
		fmt.Printf("  %-64s %18.0f %22s\n", c.name, refused, again)
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — eight servers and 1,000 customers. One customer sends requests that break every server which")
	fmt.Println("         handles them.")
	fmt.Println()
	rng := rand.New(rand.NewSource(5))
	const customers, servers = 1000, 8
	layouts := []struct {
		name   string
		assign func(c int) []int
	}{
		{"every customer is served by all eight", func(int) []int { return []int{0, 1, 2, 3, 4, 5, 6, 7} }},
		{"four cells of two servers; a customer belongs to one cell", func(c int) []int { return []int{2 * (c % 4), 2*(c%4) + 1} }},
		{"each customer is given two of the eight, at random", func(int) []int { return rng.Perm(servers)[:2] }},
	}
	fmt.Printf("  %-62s %20s %20s %16s\n", "", "customers with", "customers with", "customers")
	fmt.Printf("  %-62s %20s %20s %16s\n", "", "no server left", "one of two left", "untouched")
	for _, l := range layouts {
		broken := map[int]bool{}
		assigned := make([][]int, customers)
		for c := range assigned {
			assigned[c] = l.assign(c)
		}
		for _, s := range assigned[0] { // customer 0 is the one
			broken[s] = true
		}
		none, some, untouched := 0, 0, 0
		for c := 1; c < customers; c++ {
			left := 0
			for _, s := range assigned[c] {
				if !broken[s] {
					left++
				}
			}
			switch left {
			case 0:
				none++
			case len(assigned[c]):
				untouched++
			default:
				some++
			}
		}
		share := func(n int) string { return fmt.Sprintf("%d  (%.1f%%)", n, 100*float64(n)/float64(customers-1)) }
		fmt.Printf("  %-62s %20s %20s %16s\n", l.name, share(none), share(some), share(untouched))
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
