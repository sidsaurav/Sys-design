// Chapter 9.5 — what a service boundary costs, and where to put one.
//
//	part 1  one operation, in one process and as services: the time it takes, and how often it fails
//	part 2  twenty-four changes to a product, and three ways of cutting it into services
//
//	go run ./phase-9-shipping/code/ch05-boundaries
//
// Part 1 uses real HTTP servers on this machine, and adds the half millisecond that a round
// trip inside a datacenter takes (Chapter 1.1). Its timings will differ a little on yours.
// Part 2 is arithmetic on a list of changes that is written out in this file.
package main

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	work      = time.Millisecond       // what each of the five steps costs
	roundTrip = 500 * time.Microsecond // the network, there and back, inside a datacenter
	callFails = 0.001                  // one call in a thousand fails: a restart, a timeout, a full queue
	lines     = 20                     // the lines on an invoice
)

var steps = []string{"customer", "pricing", "tax", "ledger", "notify"}

// step is the work itself, wherever it runs.
func step() { wait(work) }

// call makes one request to another service, across the network.
func call(url string) {
	if resp, err := http.Get(url); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	wait(roundTrip)
}

// wait passes a short time exactly. A sleep of half a millisecond is rounded up by many
// systems to a whole one or more, which would make the network look slower than it is.
func wait(d time.Duration) {
	for start := time.Now(); time.Since(start) < d; {
		runtime.Gosched()
	}
}

// A shape is one way of arranging the same five steps.
type shape struct {
	what  string
	calls int              // calls over the network inside one request, after the caller's own
	do    func(svc string) // carries out one request; svc is the address of the services
}

func shapes() []shape {
	return []shape{
		{"one process: five function calls", 0, func(string) {
			for range steps {
				step()
			}
		}},
		{"five services, called one after another", 5, func(svc string) {
			for _, s := range steps {
				call(svc + "/" + s)
			}
		}},
		{"…with pricing called once for each line", 4 + lines, func(svc string) {
			call(svc + "/customer")
			for i := 0; i < lines; i++ {
				call(svc + "/pricing-of-one-line")
			}
			call(svc + "/tax")
			call(svc + "/ledger")
			call(svc + "/notify")
		}},
		{"…with three of them asked at once", 5, func(svc string) {
			var wg sync.WaitGroup
			for _, s := range []string{"customer", "pricing", "tax"} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					call(svc + "/" + s)
				}()
			}
			wg.Wait()
			call(svc + "/ledger")
			call(svc + "/notify")
		}},
	}
}

func part1() {
	// One server plays all five services: each request to it does one step's work.
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "one-line") {
			wait(work / lines) // pricing one line is a twentieth of pricing the invoice
		} else {
			step()
		}
	}))
	defer svc.Close()
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 200

	fmt.Println("PART 1 — \"create an invoice\" takes five steps of 1 ms each. A call to another service adds half a")
	fmt.Println("millisecond of network, and one call in a thousand fails.")
	fmt.Println()
	fmt.Printf("  %-44s %12s %14s %18s\n", "the same work, as", "calls over", "time for one", "requests that")
	fmt.Printf("  %-44s %12s %14s %18s\n", "", "the network", "request", "fail, in 100,000")
	r := rand.New(rand.NewSource(95))
	for _, s := range shapes() {
		// Time 200 real requests, two at a time, so that they do not get in each other's way.
		took := make([]time.Duration, 200)
		var wg sync.WaitGroup
		gate := make(chan struct{}, 2)
		for i := range took {
			wg.Add(1)
			gate <- struct{}{}
			go func() {
				defer wg.Done()
				start := time.Now()
				s.do(svc.URL)
				took[i] = time.Since(start)
				<-gate
			}()
		}
		wg.Wait()
		sort.Slice(took, func(a, b int) bool { return took[a] < took[b] })
		// Failure is drawn, and not waited for: a request fails if any of its calls does.
		// The caller's own call to the system is one more.
		failed := 0
		for i := 0; i < 100000; i++ {
			for c := 0; c <= s.calls; c++ {
				if r.Float64() < callFails {
					failed++
					break
				}
			}
		}
		fmt.Printf("  %-44s %12d %11.1f ms %18s\n", s.what, s.calls, ms(took[len(took)/2]), commas(failed)) // the median
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// A change is one piece of work that was shipped. It names the capability that it was
// about, and the layers of the code that it had to touch.
type change struct {
	what       string
	capability string
	layers     string // w: the web layer, l: the business logic, d: the data layer
	models     bool   // it changed the shape of a record that other code reads
}

var changes = []change{
	{"show the due date on the invoice page", "invoicing", "wl", false},
	{"add a discount field to invoices", "invoicing", "wld", true},
	{"fix rounding of tax on invoices", "invoicing", "l", false},
	{"export invoices as CSV", "invoicing", "wl", false},
	{"an index for the invoice list", "invoicing", "d", false},
	{"credit notes", "invoicing", "wld", true},
	{"retry a failed card payment", "payments", "l", false},
	{"store the payment provider's reference", "payments", "ld", true},
	{"a page of failed payments", "payments", "wl", false},
	{"mark an invoice paid when money arrives", "payments+invoicing", "ld", true},
	{"refunds", "payments", "wld", true},
	{"a customer's billing address", "customers", "wld", true},
	{"merge duplicate customers", "customers", "ld", false},
	{"search customers by name", "customers", "wld", false},
	{"a customer's default currency, used on new invoices", "customers+invoicing", "wld", true},
	{"email a reminder for an overdue invoice", "notifications", "l", false},
	{"let a customer turn reminders off", "notifications+customers", "wld", true},
	{"templates for emails", "notifications", "wld", false},
	{"send by SMS as well", "notifications", "ld", false},
	{"faster PDF rendering", "invoicing", "l", false},
	{"payment links in reminder emails", "notifications+payments", "l", false},
	{"an audit entry for every change to an invoice", "invoicing", "ld", false},
	{"a limit on reminders for each customer", "notifications", "ld", false},
	{"show payments on the customer's page", "customers+payments", "wl", false},
}

// A cut is a way of dividing the product into services. It says which services a change touches.
type cut struct {
	what    string
	touches func(c change) []string
}

var cuts = []cut{
	{"three services, one for each layer", func(c change) (out []string) {
		for _, l := range c.layers {
			out = append(out, string(l))
		}
		return out
	}},
	{"four services, one for each capability", func(c change) []string {
		return strings.Split(c.capability, "+")
	}},
	{"…that share one library of record types", func(c change) []string {
		if c.models { // every service is built with the library, so every service ships again
			return []string{"invoicing", "payments", "customers", "notifications"}
		}
		return strings.Split(c.capability, "+")
	}},
}

func part2() {
	fmt.Printf("PART 2 — the last %d changes to the product, and three ways of cutting it into services.\n", len(changes))
	fmt.Println()
	fmt.Printf("  %-44s %14s %10s %12s\n", "the product is cut into", "changes that", "…needed", "…needed three")
	fmt.Printf("  %-44s %14s %10s %12s\n", "", "shipped alone", "two", "or more")
	for _, c := range cuts {
		alone, two, more := 0, 0, 0
		for _, ch := range changes {
			switch n := len(c.touches(ch)); {
			case n == 1:
				alone++
			case n == 2:
				two++
			default:
				more++
			}
		}
		fmt.Printf("  %-44s %14d %10d %12d\n", c.what, alone, two, more)
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
	fmt.Println()
	part2()
}
