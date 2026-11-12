// Chapter 4.7 — three things that go wrong when services talk through events.
//
//	part 1  the shape of an event changes, and an old consumer is still running
//	part 2  one order, two ways to coordinate the services that handle it
//	part 3  counting events per minute when some of them arrive late
//
// Everything is inside this one process.
//
//	go run ./phase-4-async/code/ch07-events
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// ------------------------------------------------------------ part 1: the shape of an event

// What a consumer written against version 1 of the event expects.
type orderPlacedV1 struct {
	OrderID    int `json:"order_id"`
	TotalCents int `json:"total_cents"`
}

// A consumer written later, when the event had gained a currency.
type orderPlacedV2 struct {
	OrderID    int    `json:"order_id"`
	TotalCents int    `json:"total_cents"`
	Currency   string `json:"currency"`
}

func part1() {
	fmt.Println("PART 1 — a consumer written for version 1 of \"order placed\" adds up revenue. The producer changes")
	fmt.Println("         the event. 1,000 orders of 49.99 each: the answer should be 49,990.00.")
	fmt.Println()
	fmt.Printf("  %-52s %10s %18s   %s\n", "the producer now sends...", "errors", "revenue counted", "")

	for _, v := range []struct {
		change string
		event  string
		strict bool
	}{
		{"version 1, as before", `{"order_id": %d, "total_cents": 4999}`, false},
		{"a new field: currency", `{"order_id": %d, "total_cents": 4999, "currency": "EUR"}`, false},
		{"total_cents renamed to amount_cents", `{"order_id": %d, "amount_cents": 4999}`, false},
		{"total_cents as text: \"49.99\"", `{"order_id": %d, "total_cents": "49.99"}`, false},
		{"a new field — to a consumer that rejects unknowns", `{"order_id": %d, "total_cents": 4999, "currency": "EUR"}`, true},
	} {
		var errors, revenue int
		for id := 1; id <= 1000; id++ {
			decoder := json.NewDecoder(strings.NewReader(fmt.Sprintf(v.event, id)))
			if v.strict {
				decoder.DisallowUnknownFields()
			}
			var e orderPlacedV1
			if err := decoder.Decode(&e); err != nil {
				errors++
				continue
			}
			revenue += e.TotalCents
		}
		verdict := "correct"
		switch {
		case errors > 0:
			verdict = "every event fails: loud, and stops the consumer"
		case revenue != 4999000:
			verdict = "WRONG, and nothing failed"
		}
		fmt.Printf("  %-52s %10d %18.2f   %s\n", v.change, errors, float64(revenue)/100, verdict)
	}

	// The other direction: a new consumer reading old events — a replay, or a producer not yet upgraded.
	var e orderPlacedV2
	json.NewDecoder(bytes.NewBufferString(`{"order_id": 1, "total_cents": 4999}`)).Decode(&e)
	if e.Currency == "" {
		e.Currency = "USD" // the default that version 2 documented for events without the field
	}
	fmt.Printf("\n  A version-2 consumer reading a version-1 event finds no currency, and uses the documented default: %s.\n", e.Currency)
}

// ------------------------------------------------------------ part 2: who decides what happens next

type message struct {
	kind  string // an event ("payment declined") or a command ("charge payment")
	order int
}

type shop struct {
	trace    []string
	reserved map[int]bool // stock held for an order
	shipped  map[int]bool
	declines map[int]bool // orders whose payment will be declined
}

func newShop() *shop {
	return &shop{reserved: map[int]bool{}, shipped: map[int]bool{}, declines: map[int]bool{}}
}

func (s *shop) say(who, what string) { s.trace = append(s.trace, fmt.Sprintf("%-14s %s", who, what)) }

// Choreography: nobody is in charge. Each service reacts to events, and
// announces what it did. The services were written by different teams.
func (s *shop) choreography(order int) {
	queue := []message{{"order placed", order}}
	s.say("orders", "event:   order placed")
	paid := map[int]bool{}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		switch m.kind {
		case "order placed":
			// The stock team reserves as soon as an order exists.
			s.reserved[m.order] = true
			s.say("stock", "event:   stock reserved")
			queue = append(queue, message{"stock reserved", m.order})
			// The payments team charges as soon as an order exists.
			if s.declines[m.order] {
				s.say("payments", "event:   payment declined")
				queue = append(queue, message{"payment declined", m.order})
			} else {
				s.say("payments", "event:   payment taken")
				queue = append(queue, message{"payment taken", m.order})
			}
		case "payment taken":
			paid[m.order] = true
			fallthrough
		case "stock reserved":
			if paid[m.order] && s.reserved[m.order] && !s.shipped[m.order] {
				s.shipped[m.order] = true
				s.say("shipping", "event:   order shipped")
			}
		case "payment declined":
			// Nobody listens to this. The stock team did not know that payments could fail.
		}
	}
}

// Orchestration: one component knows the whole flow. It tells each service
// what to do, and decides what follows from the answer.
func (s *shop) orchestration(order int) {
	s.say("orchestrator", "command: charge payment")
	if s.declines[order] {
		s.say("payments", "reply:   declined")
		s.say("orchestrator", "the order is cancelled; nothing else was started")
		return
	}
	s.say("payments", "reply:   taken")
	s.say("orchestrator", "command: reserve stock")
	s.reserved[order] = true
	s.say("stock", "reply:   reserved")
	s.say("orchestrator", "command: ship")
	s.shipped[order] = true
	s.say("shipping", "reply:   shipped")
}

func part2() {
	fmt.Println("PART 2 — an order needs a payment, stock, and a shipment. 100 orders; 10 payments are declined.")
	for _, style := range []string{"choreography", "orchestration"} {
		s := newShop()
		for order := 1; order <= 100; order++ {
			s.declines[order] = order%10 == 0
			if style == "choreography" {
				s.choreography(order)
			} else {
				s.orchestration(order)
			}
			if order == 9 { // keep the messages of two orders: one that succeeds, and the first that is declined
				s.trace = s.trace[len(s.trace)-countFor(style, false):]
			}
			if order > 10 {
				s.trace = s.trace[:countFor(style, false)+countFor(style, true)]
			}
		}
		stuck := 0
		for order := range s.reserved {
			if !s.shipped[order] {
				stuck++
			}
		}
		fmt.Printf("\n  %s — order 9 succeeds; order 10 is declined:\n", strings.ToUpper(style))
		for i, line := range s.trace {
			if i == countFor(style, false) {
				fmt.Println()
			}
			fmt.Println("    " + line)
		}
		fmt.Printf("\n    shipped: %d      stock still held for orders that were never paid: %d\n", len(s.shipped), stuck)
	}
}

// countFor is how many lines one order's messages take, in each style.
func countFor(style string, declined bool) int {
	switch {
	case style == "choreography" && declined:
		return 3
	case style == "choreography":
		return 4
	case declined:
		return 3
	}
	return 6
}

// ------------------------------------------------------------ part 3: events that arrive late

type click struct {
	happened float64 // seconds: when it happened, according to the device
	arrived  float64 // when the stream processor received it
}

func part3() {
	fmt.Println("PART 3 — orders are counted per minute, for five minutes. One order in ten reaches the counter late —")
	fmt.Println("         by up to 90 seconds — because the phone it came from had no signal.")
	fmt.Println()
	rng := rand.New(rand.NewSource(11))
	var events []click
	for i := 0; i < 3000; i++ { // ten a second
		t := float64(i) / 10
		e := click{t, t + 0.05}
		if rng.Intn(10) == 0 {
			e.arrived = t + 1 + rng.Float64()*89
		}
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].arrived < events[j].arrived })

	truth := make([]int, 5)
	byArrival := make([]int, 7)
	for _, e := range events {
		truth[int(e.happened/60)]++
		byArrival[int(e.arrived/60)]++
	}
	// Counting by when it happened: a minute's count is published once the
	// counter has waited `allow` seconds after the minute ended. Anything
	// for that minute arriving later is too late to be included.
	byEvent := func(allow float64) (counts []int, late int) {
		counts = make([]int, 5)
		for _, e := range events {
			minute := int(e.happened / 60)
			if e.arrived > float64(minute+1)*60+allow {
				late++
				continue
			}
			counts[minute]++
		}
		return counts, late
	}
	short, lateShort := byEvent(30)
	long, lateLong := byEvent(120)

	fmt.Printf("  %-10s %10s %22s %30s %30s\n", "minute", "true", "counted on arrival", "by when it happened,", "by when it happened,")
	fmt.Printf("  %-10s %10s %22s %30s %30s\n", "", "", "", "waiting 30 s for stragglers", "waiting 120 s for stragglers")
	for m := 0; m < 5; m++ {
		fmt.Printf("  %-10d %10d %22d %30d %30d\n", m+1, truth[m], byArrival[m], short[m], long[m])
	}
	fmt.Printf("  %-10s %10s %22d %30s %30s\n", "after", "", byArrival[5]+byArrival[6], "", "")
	fmt.Printf("  %-10s %10s %22s %30d %30d\n", "too late", "", "", lateShort, lateLong)
	fmt.Printf("  %-10s %10s %22s %30s %30s\n", "published", "", "at once", "30 s after each minute", "120 s after each minute")
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 3)")
	flag.Parse()
	for i, run := range []func(){part1, part2, part3} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
