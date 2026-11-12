// The eight attacks, and the comparison of the two designs.
package main

import (
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"time"
)

// An attack is run against a fresh system. It reports whether the attacker got what they
// came for and, if not, which control refused them.
type attack struct {
	what string
	run  func(s *system, api string) (won bool, got string)
}

// bankDetails tries to read the bank accounts in a backup, with whatever credential the
// attacker holds for the key service: none is "".
func bankDetails(s *system, d dump, credential string) (int, string) {
	read := 0
	for id, sealed := range d.bank {
		key := d.settings // the key is in the dump itself, if the design left it there
		if key == nil {
			var ok bool
			if key, ok = s.unwrap(credential, d.tenant[id], d.wrapped[d.tenant[id]]); !ok {
				continue
			}
		}
		if _, err := open(aead(key), sealed, id); err == nil {
			read++
		}
	}
	return read, fmt.Sprintf("read %d bank accounts", read)
}

var attacks = []attack{
	{"tenant B's member asks for tenant A's invoice by number", func(s *system, api string) (bool, string) {
		status, _ := s.do(api, "GET", "/invoices/1001", s.signIn("bo"), "")
		return status == 200, "read invoice 1001"
	}},
	{"a user disabled an hour ago reads an invoice", func(s *system, api string) (bool, string) {
		token := s.signIn("ana")
		s.accounts["ana"].disabled = true
		s.now = s.now.Add(time.Hour)
		status, _ := s.do(api, "GET", "/invoices/1001", token, "")
		return status == 200, "read it, an hour later"
	}},
	{"a member marks their own invoice paid, unnoticed", func(s *system, api string) (bool, string) {
		s.do(api, "PATCH", "/invoices/1001", s.signIn("ana"), `{"note":"thanks","paid":true}`)
		if !s.invoices["1001"].Paid {
			return false, ""
		}
		if s.on("ledger") && !s.payments["1001"] { // the daily comparison: paid, and no money arrived
			s.stopped = "ledger"
			return false, ""
		}
		return true, "paid, and no money came"
	}},
	{"the URL importer is sent to the metadata service", func(s *system, api string) (bool, string) {
		status, credential := s.do(api, "POST", "/import?url=http://169.254.169.254/latest/meta-data/iam/security-credentials/app", s.signIn("bo"), "")
		if status != 200 {
			return false, ""
		}
		backup, ok := s.readBackup(strings.Trim(credential, `"`))
		if !ok {
			return false, ""
		}
		n, got := bankDetails(s, backup, strings.Trim(credential, `"`))
		return n > 0, got
	}},
	{"an altered dependency runs in the build", func(s *system, _ string) (bool, string) {
		if s.on("pin") { // the hash in the lockfile does not match, and the build stops
			s.stopped = "pin"
			return false, ""
		}
		if s.on("split") { // the code runs, in a step that holds nothing
			s.stopped = "split"
			return false, ""
		}
		backup, ok := s.readBackup(s.pipeline)
		if !ok {
			return false, ""
		}
		n, got := bankDetails(s, backup, s.pipeline)
		return n > 0, got
	}},
	{"a dump of the database is copied to a public place", func(s *system, _ string) (bool, string) {
		n, got := bankDetails(s, s.backup, "")
		return n > 0, got
	}},
	{"a script asks for 2,000 SMS codes for its own range", func(s *system, api string) (bool, string) {
		for i := 0; i < 2000; i++ {
			s.do(api, "POST", "/verify-phone?range=%2B882-34", "", "")
		}
		return s.pence > 2000, fmt.Sprintf("%s sent, for £%d.%02d", commas(s.pence/4), s.pence/100, s.pence%100)
	}},
	{"support opens a tenant with no ticket; the log is cut", func(s *system, api string) (bool, string) {
		status, _ := s.do(api, "GET", "/admin/tenants/A/invoices", s.signIn("sam"), "")
		if status != 200 {
			return false, ""
		}
		s.log = s.log[:len(s.log)-1] // somebody with write access removes the line
		if s.on("audit") && (len(s.log) == 0 || digest(s.log[len(s.log)-1]) != s.anchor) {
			s.stopped = "audit"
			return false, ""
		}
		return true, "read, and no record is left"
	}},
}

func tally(succeeded int) string {
	if succeeded == 1 {
		return fmt.Sprintf("1 of %d attacks succeeds", len(attacks))
	}
	return fmt.Sprintf("%d of %d attacks succeed", succeeded, len(attacks))
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// try runs one attack against a fresh system that lacks the given controls: over HTTP, or
// straight to the handlers.
func try(a attack, without []string, overHTTP bool) (won bool, got, stopped string) {
	s := newSystem(without...)
	api := ""
	if overHTTP {
		srv := httptest.NewServer(s.mux)
		defer srv.Close()
		api = srv.URL
	}
	won, got = a.run(s, api)
	return won, got, s.stopped
}

// fewest finds the smallest set of controls that must all be missing for an attack to work.
func fewest(a attack) []string {
	var names []string
	for _, c := range controls {
		names = append(names, c.name)
	}
	var search func(start int, chosen []string, size int) []string
	search = func(start int, chosen []string, size int) []string {
		if len(chosen) == size {
			if won, _, _ := try(a, chosen, false); won {
				return append([]string(nil), chosen...)
			}
			return nil
		}
		for i := start; i < len(names); i++ {
			if found := search(i+1, append(chosen, names[i]), size); found != nil {
				return found
			}
		}
		return nil
	}
	for size := 1; size <= 4; size++ {
		if found := search(0, nil, size); found != nil {
			return found
		}
	}
	return nil
}

func main() {
	without := flag.String("without", "", "controls to remove from the reviewed design, separated by commas")
	flag.Parse()
	var all []string
	for _, c := range controls {
		all = append(all, c.name)
	}

	if *without != "" {
		removed := strings.Split(*without, ",")
		fmt.Printf("THE REVIEWED DESIGN, WITHOUT: %s\n\n", strings.Join(removed, ", "))
		succeeded := 0
		for i, a := range attacks {
			won, got, stopped := try(a, removed, true)
			result := "stopped by: " + stopped
			if won {
				succeeded++
				result = "SUCCEEDS: " + got
			}
			fmt.Printf("  %d  %-56s %s\n", i+1, a.what, result)
		}
		fmt.Printf("\n  %s.\n", tally(succeeded))
		if succeeded > 0 {
			os.Exit(1)
		}
		return
	}

	fmt.Println("THE DESIGN IN THE BRIEF — eight attacks, each on a system that this program starts.")
	fmt.Println()
	succeeded := 0
	for i, a := range attacks {
		won, got, _ := try(a, all, true)
		result := "stopped"
		if won {
			succeeded++
			result = "SUCCEEDS: " + got
		}
		fmt.Printf("  %d  %-56s %s\n", i+1, a.what, result)
	}
	fmt.Printf("\n  %s.\n\n", tally(succeeded))

	fmt.Printf("THE REVIEWED DESIGN — the same attacks, against the %d controls of the worked design.\n\n", len(controls))
	fmt.Printf("  %-59s %-11s %s\n", "the attack", "stopped by", "all of these must fail")
	succeeded, thinnest, where := 0, len(controls), 0
	for i, a := range attacks {
		won, _, stopped := try(a, nil, true)
		if won {
			succeeded++
			stopped = "NOTHING"
		}
		needs := fewest(a)
		if len(needs) < thinnest {
			thinnest, where = len(needs), i+1
		}
		fmt.Printf("  %d  %-56s %-11s %d: %s\n", i+1, a.what, stopped, len(needs), strings.Join(needs, ", "))
	}
	fmt.Printf("\n  %s. The thinnest defence is %d control deep: attack %d.\n", tally(succeeded), thinnest, where)
	if succeeded > 0 {
		os.Exit(1)
	}
}
