// Chapter 9.4 — configuration, and infrastructure, as code.
//
//	part 1  three mistakes in a configuration file, delivered to sixty servers three ways
//	part 2  infrastructure that is declared: a plan, an apply, a drift, and a change to refuse
//
//	go run ./phase-9-shipping/code/ch04-config
//
// Part 1 is a model of a fleet, built round a real loader: the code that parses a
// configuration, checks it, and keeps the last good one. Part 2 is a very small version of
// what tools such as Terraform do. Nothing here is random.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// A config is what an operator can change without a deploy.
type config struct {
	RateLimit int `json:"rate_limit_per_second"` // requests that one server accepts in a second
	TimeoutMS int `json:"timeout_ms"`            // how long it waits for the ledger, which takes about 100 ms
}

const good = `{"rate_limit_per_second": 500, "timeout_ms": 800}`

var mistakes = []struct{ what, file string }{
	{"a comma is missing", `{"rate_limit_per_second": 500 "timeout_ms": 800}`},
	{"a timeout of 8 ms, for 800", `{"rate_limit_per_second": 500, "timeout_ms": 8}`},
	{"a limit of 5 a second, for 500", `{"rate_limit_per_second": 5, "timeout_ms": 800}`},
}

// A server holds its configuration behind an atomic pointer, so that a request never sees
// half of a change.
type server struct {
	current atomic.Pointer[config]
}

// loadTrusting is the loader that most services start with: parse it, and use it.
func (s *server) loadTrusting(file string) {
	var c config
	json.Unmarshal([]byte(file), &c) // an error here is logged, somewhere
	s.current.Store(&c)
}

// loadChecking parses into a new value, checks it, and only then swaps it in. If anything
// is wrong, the server goes on with the configuration that it has.
func (s *server) loadChecking(file string) error {
	var c config
	if err := json.Unmarshal([]byte(file), &c); err != nil {
		return fmt.Errorf("not applied: %w", err)
	}
	if c.RateLimit < 1 || c.RateLimit > 100000 {
		return errors.New("not applied: rate_limit_per_second must be between 1 and 100,000")
	}
	if c.TimeoutMS < 50 || c.TimeoutMS > 30000 {
		return errors.New("not applied: timeout_ms must be between 50 and 30,000")
	}
	s.current.Store(&c)
	return nil
}

// failing says how many of a second's 100 requests fail on this server, with its configuration.
func (s *server) failing() int {
	c := s.current.Load()
	switch {
	case c.TimeoutMS < 100: // the ledger never answers in time
		return 100
	case c.RateLimit < 100:
		return 100 - c.RateLimit
	}
	return 0
}

const (
	servers    = 60
	perRegion  = 20
	horizon    = 10 * 60 // seconds
	personAt   = 5 + 5*60
	stageOne   = 60     // one server, for a minute
	stageTwo   = 5 * 60 // then its region, for five
	offeredSec = 100    // requests a second, to each server
)

// deliver plays ten minutes of one delivery of one file, and returns the requests that failed.
func deliver(file string, checking, staged bool) (failed int, note string) {
	fleet := make([]*server, servers)
	for i := range fleet {
		fleet[i] = &server{}
		fleet[i].loadChecking(good)
	}
	apply := func(s *server) error {
		if checking {
			return s.loadChecking(file)
		}
		s.loadTrusting(file)
		return nil
	}
	stopped := false
	for t := 0; t < horizon; t++ {
		switch {
		case stopped:
		case !staged && t == 0: // everywhere, at once
			for _, s := range fleet {
				if err := apply(s); err != nil {
					stopped, note = true, "every server refused it"
					break
				}
			}
		case !staged && t == personAt: // the page fired within seconds, and a person put the old file back
			for _, s := range fleet {
				s.loadChecking(good)
			}
			stopped, note = true, "a person, after 5 min"
		case staged && t == 0: // one server first
			if err := apply(fleet[0]); err != nil {
				stopped, note = true, "the first server refused it"
			}
		case staged && t == stageOne: // is the first server worse than the others?
			if fleet[0].failing() > fleet[1].failing() {
				fleet[0].loadChecking(good)
				stopped, note = true, "stopped at one server"
				break
			}
			for _, s := range fleet[:perRegion] {
				apply(s)
			}
		case staged && t == stageOne+stageTwo:
			for _, s := range fleet {
				apply(s)
			}
		}
		for _, s := range fleet {
			failed += s.failing()
		}
	}
	return failed, note
}

func part1() {
	total := servers * offeredSec * horizon
	fmt.Printf("PART 1 — a change to a configuration file, for %d servers that take %s requests in ten minutes.\n", servers, commas(total))
	fmt.Println()
	designs := []struct {
		what             string
		checking, staged bool
	}{
		{"A  everywhere at once, and used as given", false, false},
		{"B  everywhere at once, and checked first", true, false},
		{"C  one server, one region, then all; checked", true, true},
	}
	for _, m := range mistakes {
		fmt.Printf("  The mistake: %s.   %s\n", m.what, m.file)
		for _, d := range designs {
			failed, note := deliver(m.file, d.checking, d.staged)
			fmt.Printf("    %-44s %10s failed  %5.1f%%  %s\n", d.what, commas(failed), 100*float64(failed)/float64(total), note)
		}
		fmt.Println()
	}
}

// A resource is one thing that exists in the cloud account: a server, a database.
type resource struct {
	kind    string
	attrs   string // its settings, as one string, to keep this small
	protect bool   // never destroy this without a person changing the declaration first
}

// plan compares what is declared with what exists, and lists what would be done.
func plan(declared, actual map[string]resource) (steps []string, err error) {
	names := map[string]bool{}
	for n := range declared {
		names[n] = true
	}
	for n := range actual {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		want, wanted := declared[n]
		have, exists := actual[n]
		switch {
		case wanted && !exists:
			steps = append(steps, fmt.Sprintf("+ create   %-9s %-14s %s", want.kind, n, want.attrs))
		case !wanted && exists && have.protect:
			err = fmt.Errorf("refused: this plan would destroy %s %s, which is protected", have.kind, n)
			steps = append(steps, fmt.Sprintf("- destroy  %-9s %-14s", have.kind, n))
		case !wanted && exists:
			steps = append(steps, fmt.Sprintf("- destroy  %-9s %-14s", have.kind, n))
		case want.attrs != have.attrs:
			steps = append(steps, fmt.Sprintf("~ change   %-9s %-14s %s  →  %s", want.kind, n, have.attrs, want.attrs))
		}
	}
	return steps, err
}

// apply makes what exists equal to what is declared.
func apply(declared, actual map[string]resource) {
	for n := range actual {
		if _, wanted := declared[n]; !wanted {
			delete(actual, n)
		}
	}
	for n, r := range declared {
		actual[n] = r
	}
}

func part2() {
	fmt.Println("PART 2 — infrastructure that is declared in a file. The tool compares the file with what exists.")
	declared := map[string]resource{
		"web-1":       {"server", "size=medium", false},
		"web-2":       {"server", "size=medium", false},
		"web-lb":      {"balancer", "port=443", false},
		"invoices-db": {"database", "size=large, backups=daily", true},
	}
	actual := map[string]resource{}
	show := func(when string) error {
		steps, err := plan(declared, actual)
		fmt.Println()
		fmt.Println("  " + when)
		if len(steps) == 0 {
			fmt.Println("    no changes: what exists is what is declared")
		}
		for _, s := range steps {
			fmt.Println("    " + strings.TrimRight(s, " "))
		}
		if err != nil {
			fmt.Println("    " + err.Error())
		}
		return err
	}
	show("The plan, in an empty account:")
	apply(declared, actual)
	show("The plan, after it has been applied:")

	web2 := actual["web-2"]
	web2.attrs = "size=large" // somebody resized it by hand, during an incident, and did not say
	actual["web-2"] = web2
	actual["debug-box"] = resource{"server", "size=small, port 22 open to all", false}
	show("The plan, after two changes made by hand in the console:")
	apply(declared, actual)

	db := declared["invoices-db"] // a tidy-up: the database is given a better name in the file
	delete(declared, "invoices-db")
	declared["invoices"] = db
	if show("The plan, after the database is renamed in the file:") == nil {
		apply(declared, actual)
	}
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func main() {
	part1()
	part2()
}
