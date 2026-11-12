// Parts 1 and 2: the callers, the requests, the policy as a table, and the code that
// compares each version's answers with it.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"
)

// The matrix: who asks, and what they ask for.
type caller struct {
	head [2]string // the two lines of the column's heading
	user *user     // nil is somebody who has not signed in
}

type call struct{ name, method, path string }

var (
	callers = []caller{
		{[2]string{"nobody", ""}, nil},
		{[2]string{"B's", "member"}, &user{"B", "member"}},
		{[2]string{"B's", "owner"}, &user{"B", "owner"}},
		{[2]string{"A's", "member"}, &user{"A", "member"}},
		{[2]string{"A's", "owner"}, &user{"A", "owner"}},
		{[2]string{"support", "staff"}, &user{"", "staff"}},
	}
	calls = []call{
		{"read", "GET", "/invoices/1001"},
		{"list", "GET", "/invoices?tenant=A"},
		{"file", "GET", "/invoices/1001/pdf"},
		{"void", "POST", "/invoices/1001/void"},
		{"export", "GET", "/admin/export"},
	}
	// The policy, written down before any code: what each caller should get from each
	// request. For the list, a cell says whose invoices come back.
	policy = [][]string{
		{"401", "404", "404", "200", "200", "404"},
		{"401", "B's", "B's", "A's", "A's", "nothing"},
		{"401", "404", "404", "200", "200", "404"},
		{"401", "404", "404", "403", "200", "404"},
		{"401", "403", "403", "403", "403", "200"},
	}
)

func get(method, url, session string) *http.Response {
	req, _ := http.NewRequest(method, url, nil)
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

// ask sends one request and says what came back: a status or, for the list, whose invoices.
func ask(base string, c call, session string) string {
	resp := get(c.method, base+c.path, session)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || c.name != "list" {
		io.Copy(io.Discard, resp.Body)
		return strconv.Itoa(resp.StatusCode)
	}
	var list []invoice
	json.NewDecoder(resp.Body).Decode(&list)
	owners := map[string]bool{}
	for _, inv := range list {
		owners[inv.Tenant] = true
	}
	switch len(owners) {
	case 0:
		return "nothing"
	case 1:
		return list[0].Tenant + "'s"
	}
	return "several"
}

// judge compares an answer with the policy. A cross is an answer on the wrong side of the
// line: let in where the policy refuses, or the reverse. A tilde is a refusal with the
// wrong status, which is safe but gives something away.
func judge(got, want string) string {
	refusal := func(s string) bool { return strings.HasPrefix(s, "4") }
	switch {
	case got == want:
		return ""
	case refusal(got) && refusal(want):
		return " ~"
	}
	return " ✗"
}

func row(c call, cells []string) {
	line := fmt.Sprintf("    %-7s %-4s %-21s", c.name, c.method, c.path)
	for _, cell := range cells {
		line += fmt.Sprintf(" %-8s", cell)
	}
	fmt.Println(strings.TrimRight(line, " "))
}

type version struct {
	name  string
	build func(*store, sessions) http.Handler
}

var versions = []version{
	{"Version 1 — signed in is enough", version1},
	{"Version 2 — a check written by hand into each handler", version2},
	{"Version 3 — one gate, one policy, and every lookup through the caller's tenant", version3},
}

func part1() {
	fmt.Println("PART 1 — the matrix. Six callers send five requests. Invoice 1001 belongs to tenant A.")
	fmt.Println()
	for line := 0; line < 2; line++ {
		head := strings.Repeat(" ", 38)
		for _, c := range callers {
			head += fmt.Sprintf(" %-8s", c.head[line])
		}
		fmt.Println(strings.TrimRight(head, " "))
	}
	fmt.Println("  The policy, written down first")
	for i, c := range calls {
		row(c, policy[i])
	}
	for _, v := range versions {
		db, s := newStore(false), sessions{}
		srv := httptest.NewServer(v.build(db, s))
		wrong, leaky := 0, 0
		fmt.Println()
		fmt.Println("  " + v.name)
		for i, c := range calls {
			cells := []string{}
			for j, who := range callers {
				session := ""
				if who.user != nil {
					session = s.signIn(*who.user)
				}
				got := ask(srv.URL, c, session)
				db.void("1001", false) // put back whatever the request changed
				mark := judge(got, policy[i][j])
				switch mark {
				case " ✗":
					wrong++
				case " ~":
					leaky++
				}
				cells = append(cells, got+mark)
			}
			row(c, cells)
		}
		srv.Close()
		total := len(calls) * len(callers)
		switch {
		case wrong == 0 && leaky == 0:
			fmt.Printf("    all %d answers are the policy's\n", total)
		case leaky == 0:
			fmt.Printf("    ✗ %d of %d answers are on the wrong side of the policy\n", wrong, total)
		default:
			fmt.Printf("    ✗ %d of %d answers are on the wrong side of the policy    ~ %d refuse, but with the wrong status\n", wrong, total, leaky)
		}
	}
}

func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// walk asks for the file of one identifier after another, as a member of tenant B. It
// counts the invoices of other tenants that come back, and the requests that are refused.
func walk(build func(*store, sessions) http.Handler, db *store, ids []string) (read, exposed, refused int, took time.Duration) {
	s := sessions{}
	srv := httptest.NewServer(build(db, s))
	defer srv.Close()
	session := s.signIn(user{"B", "member"})
	seen := map[string]bool{}
	start := time.Now()
	for _, id := range ids {
		resp := get("GET", srv.URL+"/invoices/"+id+"/pdf", session)
		var inv invoice
		if resp.StatusCode != 200 {
			refused++
		} else if json.NewDecoder(resp.Body).Decode(&inv) == nil && inv.Tenant != "B" {
			read++
			seen[inv.Tenant] = true
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return read, len(seen), refused, time.Since(start)
}

func part2() {
	n := tenants * perTenant
	fmt.Printf("PART 2 — what one missing check costs. %d tenants, %d invoices each. The caller is B's member, who\n", tenants, perTenant)
	fmt.Println("asks for the file of one invoice after another.")
	fmt.Println()
	fmt.Printf("  %-9s %-26s %10s %16s %9s %9s %9s\n", "version", "identifiers", "requests", "other tenants'", "tenants", "answers", "time")
	fmt.Printf("  %-9s %-26s %10s %16s %9s %9s\n", "", "", "", "invoices read", "exposed", "of 404")
	line := func(v, what string, ids []string, read, exposed, refused int, took time.Duration) {
		out := fmt.Sprintf("  %-9s %-26s %10s %16s %9d %9s", v, what, commas(len(ids)), commas(read), exposed, commas(refused))
		if len(ids) > 1 {
			out += fmt.Sprintf(" %7.2f s", took.Seconds())
		}
		fmt.Println(out)
	}

	counting := []string{}
	for k := 0; k < n; k++ {
		counting = append(counting, strconv.Itoa(1001+k))
	}
	guesses := []string{}
	for k := 0; k < n; k++ {
		guesses = append(guesses, random128())
	}
	numbered, unguessable := newStore(false), newStore(true)

	read, exposed, refused, took := walk(version2, numbered, counting)
	line("2", "1001, 1002, 1003 …", counting, read, exposed, refused, took)
	read, exposed, refused, took = walk(version2, unguessable, guesses)
	line("2", "128 random bits, guessed", guesses, read, exposed, refused, took)
	leaked := unguessable.ids[:1] // the first invoice is A's
	read, exposed, refused, took = walk(version2, unguessable, leaked)
	line("2", "…and one link, forwarded", leaked, read, exposed, refused, took)
	read, exposed, refused, took = walk(version3, numbered, counting)
	line("3", "1001, 1002, 1003 …", counting, read, exposed, refused, took)
}
