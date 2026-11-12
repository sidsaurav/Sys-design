// Chapter 5.3 — dividing data between machines: where each key goes, and what that decides.
//
// Nothing here is a database. The program hashes and counts, which is all
// that placing data is, and reports where a million keys land.
//
//	part 1  three ways to partition a table of orders
//	part 2  a ninth shard is added: how many keys have to move?
//	part 3  one customer is a fifth of the traffic
//	part 4  a query that must ask every shard
//
//	go run ./phase-5-distributed-data/code/ch03-partitions
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
)

func hash(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	// FNV alone spreads short, similar keys poorly; mix the bits once more.
	x := h.Sum32()
	x ^= x >> 16
	x *= 0x7feb352d
	x ^= x >> 15
	x *= 0x846ca68b
	x ^= x >> 16
	return x
}

func busiest(counts []int) float64 {
	total, most := 0, 0
	for _, c := range counts {
		total += c
		most = max(most, c)
	}
	return float64(most) * 100 / float64(total)
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — a table of orders on 8 shards. Order IDs increase with time; each order belongs to one of")
	fmt.Println("         10,000 customers. Three ways to decide which shard an order is on.")
	fmt.Println()
	const shards, existing, fresh = 8, 1000000, 100000
	customers := make([]int, existing+fresh) // the customer of each order
	rng := rand.New(rand.NewSource(1))
	for i := range customers {
		customers[i] = rng.Intn(10000)
	}
	fmt.Printf("  %-32s %26s %26s %26s\n", "", "the fullest shard holds", "of 100,000 new orders,", "one customer's orders")
	fmt.Printf("  %-32s %26s %26s %26s\n", "", "(of 1,000,000 orders)", "the busiest shard takes", "are on this many shards")
	for _, s := range []struct {
		name  string
		shard func(order int) int
	}{
		{"by ranges of the order ID", func(order int) int { return min(shards-1, order*shards/existing) }},
		{"by a hash of the order ID", func(order int) int { return int(hash(fmt.Sprint("o", order)) % shards) }},
		{"by a hash of the customer ID", func(order int) int { return int(hash(fmt.Sprint("c", customers[order])) % shards) }},
	} {
		stored, writes := make([]int, shards), make([]int, shards)
		holds := map[int]bool{} // the shards on which customer 4242 has orders
		for order := 0; order < existing+fresh; order++ {
			at := s.shard(order)
			if order < existing {
				stored[at]++
				if customers[order] == 4242 {
					holds[at] = true
				}
			} else {
				writes[at]++ // new orders have the highest IDs
			}
		}
		fmt.Printf("  %-32s %25.1f%% %25.1f%% %21d of %d\n", s.name, busiest(stored), busiest(writes), len(holds), shards)
	}
}

// ------------------------------------------------------------ part 2

// ring is consistent hashing: nodes are placed at points on a circle, and a
// key belongs to the first node at or after its own point.
type ring struct {
	points []uint32
	owner  map[uint32]int
}

func newRing(nodes, pointsEach int) *ring {
	r := &ring{owner: map[uint32]int{}}
	for n := 0; n < nodes; n++ {
		for p := 0; p < pointsEach; p++ {
			at := hash(fmt.Sprint("node", n, "#", p))
			r.points = append(r.points, at)
			r.owner[at] = n
		}
	}
	sort.Slice(r.points, func(i, j int) bool { return r.points[i] < r.points[j] })
	return r
}

func (r *ring) node(key string) int {
	h := hash(key)
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i] >= h })
	if i == len(r.points) {
		i = 0
	}
	return r.owner[r.points[i]]
}

func part2() {
	fmt.Println("PART 2 — 1,000,000 keys on 8 shards. A ninth is added. Ideally one key in nine moves — to the new")
	fmt.Println("         shard — and every shard then holds 11.1%.")
	fmt.Println()
	fmt.Printf("  %-52s %16s %32s\n", "a key's shard is found by...", "keys that move", "the fullest shard afterwards")
	const keys = 1000000
	for _, v := range []struct {
		name          string
		before, after func(key string) int
	}{
		{"hash modulo the number of shards",
			func(k string) int { return int(hash(k) % 8) },
			func(k string) int { return int(hash(k) % 9) }},
		{"a ring, each shard at one point",
			newRing(8, 1).node, newRing(9, 1).node},
		{"a ring, each shard at 200 points",
			newRing(8, 200).node, newRing(9, 200).node},
		{"1,024 fixed partitions, assigned to shards by a table",
			func(k string) int { return int(hash(k)%1024) % 8 },
			func(k string) int { // the new shard is given every ninth partition; the rest stay where they were
				p := int(hash(k) % 1024)
				if p%9 == 8 {
					return 8
				}
				return p % 8
			}},
	} {
		moved := 0
		counts := make([]int, 9)
		for i := 0; i < keys; i++ {
			k := fmt.Sprint("key-", i)
			a, b := v.before(k), v.after(k)
			if a != b {
				moved++
			}
			counts[b]++
		}
		fmt.Printf("  %-52s %15.0f%% %31.1f%%\n", v.name, float64(moved)*100/keys, busiest(counts))
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — 1,000,000 requests, partitioned by a hash of the customer. One customer makes a fifth of them.")
	fmt.Println()
	fmt.Printf("  %-52s %26s %20s\n", "", "requests on the busiest", "an even share")
	fmt.Printf("  %-52s %26s %20s\n", "", "shard", "would be")
	rng := rand.New(rand.NewSource(3))
	for _, v := range []struct {
		name   string
		shards int
		split  int // the busy customer's data is spread over this many keys
	}{
		{"8 shards", 8, 1},
		{"16 shards", 16, 1},
		{"64 shards", 64, 1},
		{"8 shards, the busy customer's key split in 8", 8, 8},
	} {
		counts := make([]int, v.shards)
		for i := 0; i < 1000000; i++ {
			key := fmt.Sprint("c", rng.Intn(100000))
			if rng.Intn(5) == 0 {
				key = fmt.Sprint("c-the-busy-one-", rng.Intn(v.split))
			}
			counts[int(hash(key))%v.shards]++
		}
		fmt.Printf("  %-52s %25.1f%% %19.1f%%\n", v.name, busiest(counts), 100/float64(v.shards))
	}
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — each shard answers in 5 ms, except one time in a hundred, when it takes 100 ms.")
	fmt.Println("         A query that needs every shard is as slow as the slowest. 100,000 queries each.")
	fmt.Println()
	fmt.Printf("  %-30s %30s\n", "the query asks...", "queries that took 100 ms")
	rng := rand.New(rand.NewSource(4))
	for _, shards := range []int{1, 8, 64} {
		slow := 0
		for q := 0; q < 100000; q++ {
			for s := 0; s < shards; s++ {
				if rng.Intn(100) == 0 {
					slow++
					break
				}
			}
		}
		name := fmt.Sprintf("all %d shards", shards)
		if shards == 1 {
			name = "one shard"
		}
		fmt.Printf("  %-30s %29.1f%%\n", name, float64(slow)/1000)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3, part4} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
