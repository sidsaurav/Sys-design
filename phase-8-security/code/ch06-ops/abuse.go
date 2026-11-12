// Part 1: a model of one hour's requests to an action that costs money each time.
package main

import "fmt"

const pencePerMessage = 4

// An ask is one request to send a verification code by SMS.
type ask struct {
	peer     string // the address that really connected, as our own proxy reports it
	claimed  string // the first address in X-Forwarded-For, which the client wrote
	account  string
	numbers  string // the range that the destination number is in: its first digits
	script   bool
	newcomer bool // this account was created for this request
}

// The hour's traffic. 300 people ask once each: 40 of them share one office's address, and
// their numbers are spread over 30 ranges. A script asks 20,000 times, for numbers in one
// range that it earns money from, through 500 addresses, and makes accounts as it needs them.
func traffic(perAccount int) []ask {
	var out []ask
	for i := 0; i < 300; i++ {
		peer := fmt.Sprintf("198.51.100.%d", i)
		if i < 40 {
			peer = "203.0.113.7" // the office
		}
		out = append(out, ask{peer: peer, claimed: peer, account: fmt.Sprint("person-", i), numbers: fmt.Sprint("+44 77009 ", i%30)})
	}
	for i := 0; i < 20000; i++ {
		out = append(out, ask{
			peer:     fmt.Sprintf("192.0.2.%d", i%500),
			claimed:  fmt.Sprintf("10.%d.%d.%d", i>>16, i>>8&255, i&255), // a new one every time
			account:  fmt.Sprint("bot-", i/perAccount),                   // as many as its limit makes it need
			newcomer: i%perAccount == 0,
			numbers:  "+882 34",
			script:   true,
		})
	}
	return out
}

// A limit counts by some key and refuses after a number of requests.
type limit struct {
	max   int
	key   func(ask) string
	only  func(ask) bool // nil: applies to every request
	count map[string]int
}

func (l *limit) allow(a ask) bool {
	if l.only != nil && !l.only(a) {
		return true
	}
	if l.count == nil {
		l.count = map[string]int{}
	}
	k := l.key(a)
	if l.count[k] >= l.max {
		return false
	}
	l.count[k]++
	return true
}

func part1() {
	byClaimed := func(a ask) string { return a.claimed }
	byPeer := func(a ask) string { return a.peer }
	byAccount := func(a ask) string { return a.account }
	byRange := func(a ask) string { return a.numbers }
	designs := []struct {
		what   string
		limits []*limit
	}{
		{"no limit", nil},
		{"5 an hour for each address in X-Forwarded-For", []*limit{{max: 5, key: byClaimed}}},
		{"5 an hour for each address that really connected", []*limit{{max: 5, key: byPeer}}},
		{"3 a day for each account, and anyone may sign up", []*limit{{max: 3, key: byAccount}}},
		{"…and 3 new accounts an hour for each address", []*limit{
			{max: 3, key: byPeer, only: func(a ask) bool { return a.newcomer }}, {max: 3, key: byAccount}}},
		{"30 an hour for each range of numbers", []*limit{{max: 30, key: byRange}}},
	}
	fmt.Println("PART 1 — an hour of \"send me a code by SMS\", at 4p a message. 300 people ask once each, 40 of")
	fmt.Println("them from one office. A script asks 20,000 times, for numbers in one range, through 500 addresses.")
	fmt.Println()
	fmt.Printf("  %-52s %16s %10s %14s\n", "the limit", "sent for the", "cost of", "people")
	fmt.Printf("  %-52s %16s %10s %14s\n", "", "script", "those", "refused")
	for _, d := range designs {
		sent, refused := 0, 0
		blocked := map[string]bool{} // accounts that the script was not allowed to create
		for _, a := range traffic(3) {
			ok := !blocked[a.account]
			for _, l := range d.limits {
				if ok && !l.allow(a) {
					ok = false
					if a.newcomer && l.only != nil {
						blocked[a.account] = true
					}
				}
			}
			switch {
			case ok && a.script:
				sent++
			case !ok && !a.script:
				refused++
			}
		}
		fmt.Printf("  %-52s %16s %10s %14d\n", d.what, commas(sent), fmt.Sprintf("£%d.%02d", sent*pencePerMessage/100, sent*pencePerMessage%100), refused)
	}
}
