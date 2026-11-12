// Chapter 6.1 — three ways in which a small failure becomes a large one.
//
// A model. Time moves in steps of 10 ms. In each step a server finishes a
// fixed amount of work, taking requests from a queue, first come first
// served. A client waits one second for an answer. Nothing here uses the
// network or the clock, so every run prints the same numbers.
//
//	part 1  one server of five becomes slow, and goes on passing its health check
//	part 2  one server of five fails: at 70% load, at 88%, and at 88% with servers that refuse work
//	part 3  a server stalls for 30 seconds: clients that do not retry, and clients that do
//
//	go run ./phase-6-reliability/code/ch01-failures
//	go run ./phase-6-reliability/code/ch01-failures -part 3 -load 300
package main

import (
	"flag"
	"fmt"
)

const (
	hz      = 100 // steps in a second
	timeout = hz  // a client waits one second
	window  = 30  // seconds in each column or row of the output
)

type attempt struct {
	user     int // the user's request that this attempt is for
	n        int // 1 for the first attempt
	server   int
	deadline int // the step at which the client stops waiting
	answered bool
}

type server struct {
	perSecond float64 // how many requests it can finish in a second
	credit    float64
	queue     []*attempt
	downUntil int // crashed or failed: back at this step
	failures  int // requests in a row that got no answer in time, as the balancer saw them
	outUntil  int // the balancer is not using it until this step
}

type options struct {
	servers  int
	each     float64 // requests a second that one server can finish
	load     int     // new requests a second
	attempts int     // how many times a client tries
	seconds  int
	crashAt  int               // a server with more than this many requests waiting runs out of memory
	refuseAt int               // a server with this many waiting refuses any more
	ejectAt  int               // the balancer stops using a server after this many failures in a row
	events   func(s *sim)      // what happens to the system during the run
	admit    func(s *sim) bool // the front door: whether to let an attempt in at all
}

type sim struct {
	options
	now      int
	srv      []*server
	next     int          // round robin
	expiring [][]*attempt // by step: the attempts whose clients stop waiting then
	started  []int        // by window: users' requests made
	good     []int        // by window in which the request was made: answered in time, on some attempt
	done     []bool       // by user
	made     []int        // by user: the window in which the request was made
	sent     []int        // by window: attempts sent, retries included
	longest  int          // the longest queue seen
	late     int          // requests that a server finished after their client had stopped waiting
	arrivals float64
}

func (s *sim) second() int { return s.now / hz }

// send gives one attempt to a server, if there is one to give it to.
func (s *sim) send(user, n int) {
	a := &attempt{user: user, n: n, server: -1, deadline: s.now + timeout}
	s.sent[min(s.second()/window, len(s.sent)-1)]++
	s.expiring[a.deadline] = append(s.expiring[a.deadline], a)
	if s.admit != nil && !s.admit(s) {
		return // turned away at the door. The client finds out when it stops waiting
	}
	for range s.srv { // the balancer: the next server that is up and in use
		s.next = (s.next + 1) % len(s.srv)
		if sv := s.srv[s.next]; s.now >= sv.downUntil && s.now >= sv.outUntil {
			if s.refuseAt > 0 && len(sv.queue) >= s.refuseAt {
				return // refused: the client is told no
			}
			a.server = s.next
			sv.queue = append(sv.queue, a)
			return
		}
	}
}

func (s *sim) step() {
	if s.events != nil {
		s.events(s)
	}
	// New requests from users.
	if s.second() < s.seconds {
		for s.arrivals += float64(s.load) / hz; s.arrivals >= 1; s.arrivals-- {
			w := s.second() / window
			s.started[w]++
			s.done, s.made = append(s.done, false), append(s.made, w)
			s.send(len(s.done)-1, 1)
		}
	}
	// Servers work.
	for i, sv := range s.srv {
		if s.now < sv.downUntil {
			continue
		}
		if s.crashAt > 0 && len(sv.queue) > s.crashAt {
			sv.queue, sv.downUntil = nil, s.now+(20+2*i)*hz // everything it held is lost; it restarts later
			continue
		}
		s.longest = max(s.longest, len(sv.queue))
		for sv.credit += sv.perSecond / hz; sv.credit >= 1 && len(sv.queue) > 0; sv.credit-- {
			a := sv.queue[0]
			sv.queue = sv.queue[1:]
			a.answered = true
			switch {
			case s.now > a.deadline:
				s.late++ // the client has gone: this work was for nobody
			case !s.done[a.user]:
				s.done[a.user] = true
				s.good[s.made[a.user]]++
				sv.failures = 0
			default:
				sv.failures = 0
			}
		}
		if len(sv.queue) == 0 {
			sv.credit = 0
		}
	}
	// Clients whose second is up.
	for _, a := range s.expiring[s.now] {
		if a.answered {
			continue
		}
		if a.server >= 0 && s.ejectAt > 0 { // the balancer saw a request to this server go unanswered
			if sv := s.srv[a.server]; s.now >= sv.outUntil {
				if sv.failures++; sv.failures >= s.ejectAt {
					sv.failures, sv.outUntil = 0, s.now+30*hz
				}
			}
		}
		if !s.done[a.user] && a.n < s.attempts {
			s.send(a.user, a.n+1) // try again, at once
		}
	}
	s.expiring[s.now] = nil
	s.now++
}

func run(o options) *sim {
	windows := o.seconds / window
	s := &sim{options: o, started: make([]int, windows), good: make([]int, windows), sent: make([]int, windows+1)}
	for i := 0; i < o.servers; i++ {
		s.srv = append(s.srv, &server{perSecond: o.each})
	}
	steps := (o.seconds + o.attempts + 1) * hz // run on until the last requests are settled
	s.expiring = make([][]*attempt, steps+timeout+1)
	for s.now < steps {
		s.step()
	}
	return s
}

func (s *sim) share(w int) string {
	return fmt.Sprintf("%.0f%%", 100*float64(s.good[w])/float64(s.started[w]))
}

func (s *sim) failed() (n, of int) {
	for w := range s.started {
		n, of = n+s.started[w]-s.good[w], of+s.started[w]
	}
	return n, of
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — five servers, 180 requests a second each, of the 250 that each can do. At second 30 one")
	fmt.Println("         server becomes ten times slower. It goes on answering its health check, every 5 seconds.")
	fmt.Println()
	fmt.Printf("  %-52s %16s %18s\n", "", "health checks", "users' requests")
	fmt.Printf("  %-52s %16s %18s\n", "the balancer takes a server out of use when…", "passed", "not answered")
	for _, eject := range []int{0, 5} {
		checks := 0
		s := run(options{servers: 5, each: 250, load: 900, attempts: 1, seconds: 300, ejectAt: eject, events: func(s *sim) {
			if s.now == 30*hz {
				s.srv[2].perSecond /= 10
			}
			if s.now%(5*hz) == 0 && s.second() < s.seconds {
				checks += len(s.srv) // the check is a separate, cheap request: it does not wait in the queue
			}
		}})
		name := "…it fails its health check"
		if eject > 0 {
			name = "…that, or 5 requests in a row get no answer"
		}
		n, of := s.failed()
		fmt.Printf("  %-52s %9d of %d %10d of %d  (%.1f%%)\n", name, checks, checks, n, of, 100*float64(n)/float64(of))
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — five servers, 250 requests a second each. At second 30 one of them fails. It is back at second 90.")
	fmt.Println("         A server with more than 400 requests waiting runs out of memory, and takes 20 to 30 seconds to restart.")
	fmt.Println()
	fmt.Printf("  %-36s %-44s %12s\n", "", "users' requests answered in time, by half-minute", "servers up")
	fmt.Printf("  %-36s", "")
	for w := 0; w < 8; w++ {
		fmt.Printf(" %5s", fmt.Sprint(w*window))
	}
	fmt.Printf(" %10s\n", "at the end")
	for _, c := range []struct {
		name     string
		load     int
		refuseAt int
	}{
		{"70% load: 875 a second", 875, 0},
		{"88% load: 1,100 a second", 1100, 0},
		{"88%, and servers refuse the excess", 1100, 100},
	} {
		crashAt := 400
		if c.refuseAt > 0 {
			crashAt = 0
		}
		s := run(options{servers: 5, each: 250, load: c.load, attempts: 1, seconds: 240, crashAt: crashAt, refuseAt: c.refuseAt,
			events: func(s *sim) {
				if s.now == 30*hz {
					s.srv[0].queue, s.srv[0].downUntil = nil, 90*hz
				}
			}})
		fmt.Printf("  %-36s", c.name)
		for w := range s.started {
			fmt.Printf(" %5s", s.share(w))
		}
		up := 0
		for _, sv := range s.srv {
			if 240*hz >= sv.downUntil {
				up++
			}
		}
		fmt.Printf(" %10d\n", up)
	}
}

// ------------------------------------------------------------ part 3

func part3(load int) {
	fmt.Printf("PART 3 — one service that can finish 1,000 requests a second, and is sent %d. From second 30 to\n", load)
	fmt.Println("         second 60 it finishes nothing. Then it is as it was.")
	fmt.Println()
	type variant struct {
		name     string
		attempts int
		restart  bool // at second 240, the service is restarted: its queue is emptied
		half     bool // and for the next 30 seconds, every second attempt is turned away at the door
	}
	variants := []variant{
		{"clients do not retry", 1, false, false},
		{"clients try three times", 3, false, false},
		{"as (b); and at second 240 the service is restarted, which empties its queue", 3, true, false},
		{"as (c); and for the next 30 seconds every second attempt is turned away at the door", 3, true, true},
	}
	var runs []*sim
	for _, v := range variants {
		turn := 0
		runs = append(runs, run(options{servers: 1, each: 1000, load: load, attempts: v.attempts, seconds: 480,
			events: func(s *sim) {
				switch {
				case s.now == 30*hz:
					s.srv[0].perSecond = 0
				case s.now == 60*hz:
					s.srv[0].perSecond = 1000
				case s.now == 240*hz && v.restart:
					s.srv[0].queue = nil
				}
			},
			admit: func(s *sim) bool {
				if !v.half || s.second() < 240 || s.second() >= 270 {
					return true
				}
				turn++
				return turn%2 == 0
			}}))
	}
	for i, v := range variants {
		fmt.Printf("  (%c) %s\n", 'a'+i, v.name)
	}
	fmt.Println()
	fmt.Println("  users' requests answered in time, by the half-minute in which they were made")
	fmt.Println()
	fmt.Printf("  %-9s", "seconds")
	for i := range variants {
		fmt.Printf(" %8s", fmt.Sprintf("(%c)", 'a'+i))
	}
	fmt.Println()
	for w := 0; w < 480/window; w++ {
		fmt.Printf("  %3d–%-5d", w*window, (w+1)*window)
		for _, s := range runs {
			fmt.Printf(" %8s", s.share(w))
		}
		fmt.Println()
	}
	fmt.Println()
	fmt.Printf("  %-5s %18s %20s %22s %16s\n", "", "requests made", "attempts sent,", "finished by the service", "longest queue")
	fmt.Printf("  %-5s %18s %20s %22s %16s\n", "", "by users", "retries included", "too late to matter", "")
	for i := range variants {
		s, attempts := runs[i], 0
		for _, n := range s.sent {
			attempts += n
		}
		_, of := s.failed()
		fmt.Printf("  (%c)   %18d %20d %22d %16d\n", 'a'+i, of, attempts, s.late, s.longest)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 3)")
	load := flag.Int("load", 800, "part 3: how many requests a second users make")
	flag.Parse()
	for i, f := range []func(){part1, part2, func() { part3(*load) }} {
		if *part == 0 || *part == i+1 {
			f()
			fmt.Println()
		}
	}
}
