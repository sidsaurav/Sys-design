// Chapter 5.2 — more than one server accepts writes. What happens to two writes that meet?
//
// Everything here is a model inside one process, with no clocks running and
// no network: replicas are structs, and "replication" is a function call
// made at a moment the program chooses. That is what makes the outcomes exact.
//
//	part 1  two regions each accept writes; the newest timestamp wins
//	part 2  the same writes, with version vectors: which really came first?
//	part 3  three replicas and no leader: how many must answer a write, and a read
//
//	go run ./phase-5-distributed-data/code/ch02-conflicts
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// ------------------------------------------------------------ a value with its history

// vector says how many writes from each region a version has seen.
type vector map[string]int

func (v vector) copyOf() vector {
	c := vector{}
	for k, n := range v {
		c[k] = n
	}
	return c
}

// atLeast reports whether v has seen everything that w has.
func (v vector) atLeast(w vector) bool {
	for k, n := range w {
		if v[k] < n {
			return false
		}
	}
	return true
}

type version struct {
	items []string // the value: a shopping cart, or a one-item "address"
	time  float64  // the writer's clock, in seconds
	seen  vector
}

func has(items []string, item string) bool {
	for _, i := range items {
		if i == item {
			return true
		}
	}
	return false
}

// A region holds one or more versions of the value. More than one means
// that it has received writes it cannot put in order.
type region struct {
	name     string
	skew     float64 // how far this region's clock is from the true time
	versions []version
}

// write changes the value in this region, as a user's request would: read
// what is here, change it, and store the result as a new version.
func (r *region) write(now float64, change func(items []string) []string) {
	var items []string
	seen := vector{}
	for _, v := range r.versions { // start from everything this region knows
		for _, i := range v.items {
			if !has(items, i) {
				items = append(items, i)
			}
		}
		for k, n := range v.seen {
			seen[k] = max(seen[k], n)
		}
	}
	seen[r.name]++
	r.versions = []version{{change(items), now + r.skew, seen}}
}

// receive is replication arriving from the other region.
func (r *region) receive(incoming []version, useVectors bool) {
	for _, in := range incoming {
		if !useVectors {
			// Last write wins: keep whichever has the later timestamp.
			if len(r.versions) == 0 || in.time > r.versions[0].time {
				r.versions = []version{in}
			}
			continue
		}
		var kept []version
		obsolete := false
		for _, mine := range r.versions {
			switch {
			case mine.seen.atLeast(in.seen):
				obsolete = true // I already have this, or something that came after it
				kept = append(kept, mine)
			case in.seen.atLeast(mine.seen):
				// The incoming version came after mine: mine is replaced.
			default:
				kept = append(kept, mine) // neither saw the other: both are kept
			}
		}
		if !obsolete {
			kept = append(kept, version{append([]string(nil), in.items...), in.time, in.seen.copyOf()})
		}
		r.versions = kept
	}
}

// read returns the value: the union of whatever versions are held.
func (r *region) read() []string {
	var items []string
	for _, v := range r.versions {
		for _, i := range v.items {
			if !has(items, i) {
				items = append(items, i)
			}
		}
	}
	sort.Strings(items)
	return items
}

func add(item string) func([]string) []string {
	return func(items []string) []string { return append(append([]string(nil), items...), item) }
}

func set(item string) func([]string) []string {
	return func([]string) []string { return []string{item} }
}

func remove(item string) func([]string) []string {
	return func(items []string) (out []string) {
		for _, i := range items {
			if i != item {
				out = append(out, i)
			}
		}
		return out
	}
}

// scenario runs one story in two regions and returns what each ends with.
func scenario(useVectors bool, skewUS float64, story func(eu, us *region, sync func())) string {
	eu, us := &region{name: "EU"}, &region{name: "US", skew: skewUS}
	sync := func() {
		fromEU, fromUS := append([]version(nil), eu.versions...), append([]version(nil), us.versions...)
		us.receive(fromEU, useVectors)
		eu.receive(fromUS, useVectors)
	}
	story(eu, us, sync)
	sync()
	if a, b := strings.Join(eu.read(), ", "), strings.Join(us.read(), ", "); a != b {
		return "EU has [" + a + "], US has [" + b + "]"
	}
	return "[" + strings.Join(eu.read(), ", ") + "]"
}

var stories = []struct {
	name   string
	should string
	skew   float64
	tell   func(eu, us *region, sync func())
}{
	{"A cart holds a book. At the same moment, a pen is added in the EU and a lamp in the US.", "[book, lamp, pen]", 0,
		func(eu, us *region, sync func()) {
			eu.write(1, add("book"))
			sync()
			eu.write(10, add("pen"))
			us.write(10.2, add("lamp")) // before either has heard from the other
		}},
	{"An address is set to \"Elm St\" in the EU. A second later — replication has arrived — it is corrected to \"Oak St\" in the US, whose clock is 3 seconds slow.", "[Oak St]", -3,
		func(eu, us *region, sync func()) {
			eu.write(10, set("Elm St"))
			sync()
			us.write(11, set("Oak St"))
		}},
	{"A cart holds a book and a pen. At the same moment, the book is removed in the EU and a lamp is added in the US.", "[lamp, pen]", 0,
		func(eu, us *region, sync func()) {
			eu.write(1, add("book"))
			eu.write(2, add("pen"))
			sync()
			eu.write(10, remove("book"))
			us.write(10.2, add("lamp"))
		}},
}

func part1and2() {
	fmt.Println("PARTS 1 and 2 — two regions, each accepting writes and sending them to the other afterwards.")
	for i, s := range stories {
		fmt.Printf("\n  %d. %s\n", i+1, s.name)
		fmt.Printf("     %-44s %s\n", "what the user expects", s.should)
		fmt.Printf("     %-44s %s\n", "the latest timestamp wins", scenario(false, s.skew, s.tell))
		fmt.Printf("     %-44s %s\n", "version vectors; concurrent versions merged", scenario(true, s.skew, s.tell))
	}
}

// ------------------------------------------------------------ part 3: quorums

func part3() {
	fmt.Println("PART 3 — three replicas and no leader. A write is confirmed when W replicas have it; the third gets it")
	fmt.Println("         a little later. A read asks R replicas and takes the newest answer. 10,000 times: write, then")
	fmt.Println("         read at once.")
	fmt.Println()
	fmt.Printf("  %-4s %-4s %-12s %18s %30s %30s\n", "W", "R", "W + R > 3", "reads that were", "with one replica down,", "with one replica down,")
	fmt.Printf("  %-4s %-4s %-12s %18s %30s %30s\n", "", "", "", "out of date", "can it write?", "can it read?")
	rng := rand.New(rand.NewSource(2))
	for _, c := range [][2]int{{1, 1}, {2, 1}, {1, 2}, {2, 2}, {3, 1}, {1, 3}} {
		w, r := c[0], c[1]
		stale := 0
		var replicas [3]int // the newest write each replica has
		for write := 1; write <= 10000; write++ {
			order := rng.Perm(3)
			for _, i := range order[:w] { // the first W to answer
				replicas[i] = write
			}
			newest := 0
			for _, i := range rng.Perm(3)[:r] { // the read asks R of them
				newest = max(newest, replicas[i])
			}
			if newest < write {
				stale++
			}
			for _, i := range order[w:] { // replication catches up, after the read
				replicas[i] = write
			}
		}
		yes := func(need int) string {
			if need <= 2 {
				return "yes"
			}
			return "NO"
		}
		overlap := "no"
		if w+r > 3 {
			overlap = "yes"
		}
		fmt.Printf("  %-4d %-4d %-12s %17.0f%% %30s %30s\n", w, r, overlap, float64(stale)/100, yes(w), yes(r))
	}
}

func main() {
	part := flag.Int("part", 0, "run only parts 1 and 2 (1) or part 3 (3)")
	flag.Parse()
	if *part == 0 || *part == 1 || *part == 2 {
		part1and2()
		fmt.Println()
	}
	if *part == 0 || *part == 3 {
		part3()
	}
}
