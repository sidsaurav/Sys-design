// Chapter 5.6 — choosing one leader among five machines, when the network may split.
//
// A model in one process: five nodes, a counter of ticks for time, and a
// network the program can cut in two. It implements only the part of Raft
// that elects a leader and decides when a write is safe to confirm — terms,
// votes, majorities — not the whole algorithm.
//
//	part 1  the home-made rule: "I have not heard from a leader, so I am the leader"
//	part 2  the same split, with terms, votes and majorities
//	part 3  500 runs with the network cut at random: how often does each go wrong?
//
//	go run ./phase-5-distributed-data/code/ch06-election
package main

import (
	"flag"
	"fmt"
	"math/rand"
)

type entry struct {
	term  int
	value int
}

type node struct {
	id       int
	term     int // the election this node believes is current
	votedFor int // in that term; -1 if nobody yet
	leader   bool
	log      []entry
	deadline int // if no leader has been heard from by this tick, do something about it
}

func (n *node) lastTerm() int {
	if len(n.log) == 0 {
		return 0
	}
	return n.log[len(n.log)-1].term
}

type cluster struct {
	nodes     []*node
	side      []int // which side of a split each node is on; all equal when the network is whole
	rng       *rand.Rand
	votes     bool         // elect by terms and majorities, or by the home-made rule
	confirmed map[int]bool // writes that a client was told had succeeded
	next      int          // the next value a client will write

	twoBelieve, twoInOneTerm int // ticks on which two nodes believed they led; and did so in the same term
}

func newCluster(votes bool, seed int64) *cluster {
	c := &cluster{votes: votes, rng: rand.New(rand.NewSource(seed)), confirmed: map[int]bool{}, side: make([]int, 5)}
	for i := 0; i < 5; i++ {
		c.nodes = append(c.nodes, &node{id: i, votedFor: -1})
		c.nodes[i].deadline = c.timeout(0)
	}
	return c
}

func (c *cluster) timeout(now int) int {
	if c.votes {
		return now + 10 + c.rng.Intn(10) // random, so that two nodes seldom stand at the same moment
	}
	return now + 10
}

func (c *cluster) reach(a, b *node) bool { return c.side[a.id] == c.side[b.id] }

func (c *cluster) step(tick int) {
	// 1. Every node that believes it is the leader sends its log to everyone it can reach.
	for _, l := range c.nodes {
		if !l.leader {
			continue
		}
		have := 1
		for _, p := range c.nodes {
			if p == l || !c.reach(l, p) {
				continue
			}
			if c.votes {
				if p.term > l.term { // a later election has happened: this leader is out of date
					l.leader, l.term, l.votedFor = false, p.term, -1
					break
				}
				p.term, p.leader = l.term, false
			} else if p.leader {
				if p.id < l.id {
					continue // two leaders have met: the lower number wins, and will overwrite this one
				}
				p.leader = false
			}
			p.log = append([]entry(nil), l.log...)
			p.deadline = c.timeout(tick)
			have++
		}
		// A write is confirmed to its client when the leader says so. With
		// votes, that is when a majority has it. Without, at once.
		if l.leader && (!c.votes || have*2 > len(c.nodes) && l.lastTerm() == l.term) {
			for _, e := range l.log {
				c.confirmed[e.value] = true
			}
		}
	}

	// 2. A node that has heard from no leader for too long acts.
	for _, n := range c.nodes {
		if n.leader || tick < n.deadline {
			continue
		}
		n.deadline = c.timeout(tick)
		if !c.votes {
			n.leader = true // "nobody is leading, so I am"
			continue
		}
		n.term++ // a new election, with this node as the candidate
		n.votedFor = n.id
		votes := 1
		for _, p := range c.nodes {
			if p == n || !c.reach(n, p) {
				continue
			}
			if p.term > n.term {
				n.term, n.votedFor, votes = p.term, -1, 0
				break
			}
			if p.term < n.term {
				p.term, p.votedFor, p.leader = n.term, -1, false
			}
			// One vote per term, and only for a candidate whose log is not behind the voter's.
			behind := n.lastTerm() < p.lastTerm() || n.lastTerm() == p.lastTerm() && len(n.log) < len(p.log)
			if p.votedFor == -1 && !behind {
				p.votedFor = n.id
				p.deadline = c.timeout(tick)
				votes++
			}
		}
		if votes*2 > len(c.nodes) {
			n.leader = true
		}
	}

	// 3. Two clients, one near node 0 and one near node 4, each write a value
	// through whichever node on their side says it is the leader.
	for _, near := range []int{0, 4} {
		var l *node
		for _, n := range c.nodes {
			if n.leader && c.reach(n, c.nodes[near]) && (l == nil || n.term > l.term) {
				l = n
			}
		}
		if l != nil {
			c.next++
			l.log = append(l.log, entry{l.term, c.next})
		}
	}

	// 4. Count the ticks on which there were two leaders.
	var leaders []*node
	for _, n := range c.nodes {
		if n.leader {
			leaders = append(leaders, n)
		}
	}
	if len(leaders) > 1 {
		c.twoBelieve++
		for i, a := range leaders {
			for _, b := range leaders[i+1:] {
				if c.votes && a.term == b.term {
					c.twoInOneTerm++
				}
			}
		}
	}
}

// lost counts the confirmed writes that the final leader does not have.
func (c *cluster) lost() (n int) {
	var final *node
	for _, x := range c.nodes {
		if x.leader && (final == nil || x.term > final.term) {
			final = x
		}
	}
	if final == nil {
		return 0
	}
	have := map[int]bool{}
	for _, e := range final.log {
		have[e.value] = true
	}
	for v := range c.confirmed {
		if !have[v] {
			n++
		}
	}
	return n
}

// isolateLeader cuts the network so that the current leader and one other
// node are on one side, and the remaining three on the other.
func (c *cluster) isolateLeader() {
	leader := 0
	for _, n := range c.nodes {
		if n.leader {
			leader = n.id
		}
	}
	for i := range c.side {
		c.side[i] = 1
	}
	c.side[leader], c.side[(leader+1)%5] = 0, 0
}

func (c *cluster) heal() {
	for i := range c.side {
		c.side[i] = 0
	}
}

func oneSplit(votes bool) {
	c := newCluster(votes, 1)
	tick := 0
	run := func(ticks int) {
		for end := tick + ticks; tick < end; tick++ {
			c.step(tick)
		}
	}
	run(60)
	c.isolateLeader()
	before := len(c.confirmed)
	run(100)
	during := len(c.confirmed) - before
	c.heal()
	run(60)
	fmt.Printf("  ticks on which two nodes believed they were the leader     %6d\n", c.twoBelieve)
	if votes {
		fmt.Printf("  ticks on which two nodes led in the SAME term             %6d\n", c.twoInOneTerm)
	}
	fmt.Printf("  writes confirmed to clients during the split               %6d\n", during)
	fmt.Printf("  confirmed writes missing once the network was whole again  %6d\n", c.lost())
}

func part3() {
	fmt.Println("PART 3 — 500 runs of 400 ticks. Every 20 to 60 ticks the network is cut in two at random, or mended.")
	fmt.Println()
	fmt.Printf("  %-30s %26s %26s %24s\n", "", "runs in which two nodes", "runs with two leaders", "runs that lost a")
	fmt.Printf("  %-30s %26s %26s %24s\n", "", "believed they led", "in the same term", "confirmed write")
	for _, votes := range []bool{false, true} {
		believed, sameTerm, lostRuns := 0, 0, 0
		for run := 0; run < 500; run++ {
			c := newCluster(votes, int64(run))
			change := 30
			for tick := 0; tick < 400; tick++ {
				if tick == change {
					if c.rng.Intn(3) == 0 {
						c.heal()
					} else {
						for i := range c.side {
							c.side[i] = c.rng.Intn(2)
						}
					}
					change += 20 + c.rng.Intn(40)
				}
				c.step(tick)
			}
			c.heal()
			for tick := 400; tick < 460; tick++ {
				c.step(tick)
			}
			if c.twoBelieve > 0 {
				believed++
			}
			if c.twoInOneTerm > 0 {
				sameTerm++
			}
			if c.lost() > 0 {
				lostRuns++
			}
		}
		name := "the home-made rule"
		if votes {
			name = "terms, votes and majorities"
		}
		same := fmt.Sprint(sameTerm)
		if !votes {
			same = "(it has no terms)"
		}
		fmt.Printf("  %-30s %26d %26s %24d\n", name, believed, same, lostRuns)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 3)")
	flag.Parse()
	if *part == 0 || *part == 1 {
		fmt.Println("PART 1 — five nodes. The rule: a node that hears from no leader for 10 ticks becomes the leader.")
		fmt.Println("         After 60 ticks the network splits, with the leader and one other node on the small side,")
		fmt.Println("         for 100 ticks.")
		fmt.Println()
		oneSplit(false)
		fmt.Println()
	}
	if *part == 0 || *part == 2 {
		fmt.Println("PART 2 — the same five nodes and the same split. The rule: a node that hears from no leader starts")
		fmt.Println("         an election in a new term, and leads only if a majority of all five vote for it. A leader")
		fmt.Println("         confirms a write only when a majority has it.")
		fmt.Println()
		oneSplit(true)
		fmt.Println()
	}
	if *part == 0 || *part == 3 {
		part3()
	}
}
