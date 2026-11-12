// Chapter 8.3 — authorization: who may do what, and to which object.
//
// An invoicing API that many customers ("tenants") share, served over real HTTP on this
// machine, in three versions. Six kinds of caller send the same five requests to each
// version, and every answer is compared with a policy that was written down first.
// Nothing is attacked except the servers that this program starts.
//
//	part 1  the matrix: three versions of the API against one policy
//	part 2  what one missing check costs: asking for one identifier after another
//	part 3  the same rule in the database: PostgreSQL row-level security
//
//	go run ./phase-8-security/code/ch03-authz
//
// Part 3 needs the database from Phase 2 (DATABASE_URL, as before), and a user that may
// create roles, which the user of Phase 2 may. Without a database it is skipped.
//
// This file is the API: the invoices, the sessions, and the three versions. The callers,
// the policy and the comparison are in matrix.go, and part 3 is in postgres.go.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	tenants   = 50
	perTenant = 40
)

type invoice struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Pence  int    `json:"pence"`
	Void   bool   `json:"void"`
}

type user struct {
	tenant string // the customer this person works for; "" for the provider's own staff
	role   string // "member" or "owner" of that tenant, or "staff"
}

// A store holds every tenant's invoices together, as one shared table does.
type store struct {
	mu   sync.Mutex
	rows map[string]*invoice
	ids  []string // in the order they were made
}

func tenantName(i int) string {
	if i < 2 {
		return string(rune('A' + i))
	}
	return fmt.Sprintf("T%02d", i+1)
}

func random128() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// newStore makes 2,000 invoices. The tenants take turns, so one tenant's identifiers lie
// among everyone else's. Identifiers count up from 1001, or are 128 random bits.
func newStore(random bool) *store {
	s := &store{rows: map[string]*invoice{}}
	for k := 0; k < tenants*perTenant; k++ {
		id := strconv.Itoa(1001 + k)
		if random {
			id = random128()
		}
		s.rows[id] = &invoice{ID: id, Tenant: tenantName(k % tenants), Pence: 1000 + (37*k)%9000}
		s.ids = append(s.ids, id)
	}
	return s
}

// any finds an invoice by its identifier alone:    WHERE id = $1
func (s *store) any(id string) *invoice {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inv, ok := s.rows[id]; ok {
		c := *inv
		return &c
	}
	return nil
}

// scoped finds it only for its own tenant:    WHERE id = $1 AND tenant = $2
func (s *store) scoped(tenant, id string) *invoice {
	if inv := s.any(id); inv != nil && inv.Tenant == tenant {
		return inv
	}
	return nil
}

// list is one tenant's invoices. all is everybody's.
func (s *store) list(tenant string) []invoice {
	out := []invoice{}
	for _, inv := range s.all() {
		if inv.Tenant == tenant {
			out = append(out, inv)
		}
	}
	return out
}

func (s *store) all() []invoice {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []invoice{}
	for _, id := range s.ids {
		out = append(out, *s.rows[id])
	}
	return out
}

func (s *store) void(id string, to bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.rows[id]
	if ok {
		inv.Void = to
	}
	return ok
}

// sessions maps a session ID to a user, as the first design of Chapter 8.2 does.
type sessions map[string]user

func (s sessions) signIn(u user) string {
	id := random128()
	s[id] = u
	return id
}

func (s sessions) who(r *http.Request) (user, bool) {
	u, ok := s[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	return u, ok
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		json.NewEncoder(w).Encode(body)
	}
}

type handler func(w http.ResponseWriter, r *http.Request, u user)

// signedIn is all the checking that version 1 does, and where version 2 starts.
func signedIn(s sessions, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.who(r)
		if !ok {
			reply(w, 401, nil)
			return
		}
		h(w, r, u)
	}
}

// Version 1 — signed in is enough. No handler asks whose invoice it is, or who is asking.
func version1(db *store, s sessions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /invoices", signedIn(s, func(w http.ResponseWriter, r *http.Request, _ user) {
		reply(w, 200, db.list(r.URL.Query().Get("tenant"))) // the web page always sends its own tenant
	}))
	show := signedIn(s, func(w http.ResponseWriter, r *http.Request, _ user) {
		inv := db.any(r.PathValue("id"))
		if inv == nil {
			reply(w, 404, nil)
			return
		}
		reply(w, 200, inv)
	})
	mux.HandleFunc("GET /invoices/{id}", show)
	mux.HandleFunc("GET /invoices/{id}/pdf", show) // the same invoice as a file; here, the same JSON
	mux.HandleFunc("POST /invoices/{id}/void", signedIn(s, func(w http.ResponseWriter, r *http.Request, _ user) {
		if !db.void(r.PathValue("id"), true) {
			reply(w, 404, nil)
			return
		}
		reply(w, 200, nil)
	}))
	mux.HandleFunc("GET /admin/export", signedIn(s, func(w http.ResponseWriter, _ *http.Request, _ user) {
		reply(w, 200, db.all()) // no page that customers see has a link to this
	}))
	return mux
}

// Version 2 — after the bug report, a check is written by hand into each handler.
// One handler is missed, and one check is half a check.
func version2(db *store, s sessions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /invoices", signedIn(s, func(w http.ResponseWriter, _ *http.Request, u user) {
		reply(w, 200, db.list(u.tenant)) // fixed: the tenant comes from the session
	}))
	mux.HandleFunc("GET /invoices/{id}", signedIn(s, func(w http.ResponseWriter, r *http.Request, u user) {
		inv := db.any(r.PathValue("id"))
		if inv == nil || inv.Tenant != u.tenant { // fixed: whose invoice is it?
			reply(w, 404, nil)
			return
		}
		reply(w, 200, inv)
	}))
	mux.HandleFunc("GET /invoices/{id}/pdf", signedIn(s, func(w http.ResponseWriter, r *http.Request, _ user) {
		inv := db.any(r.PathValue("id")) // missed: this handler lives in another file
		if inv == nil {
			reply(w, 404, nil)
			return
		}
		reply(w, 200, inv)
	}))
	mux.HandleFunc("POST /invoices/{id}/void", signedIn(s, func(w http.ResponseWriter, r *http.Request, u user) {
		inv := db.any(r.PathValue("id"))
		if inv == nil {
			reply(w, 404, nil)
			return
		}
		if u.role != "owner" { // half a check: an owner, but of which tenant?
			reply(w, 403, nil)
			return
		}
		db.void(inv.ID, true)
		reply(w, 200, nil)
	}))
	mux.HandleFunc("GET /admin/export", signedIn(s, func(w http.ResponseWriter, _ *http.Request, u user) {
		if u.role != "staff" { // fixed: who is asking?
			reply(w, 403, nil)
			return
		}
		reply(w, 200, db.all())
	}))
	return mux
}

// allow is the whole policy of version 3. Anything that it does not name is refused.
func allow(u user, action string, inv *invoice) bool {
	switch action {
	case "invoice.list":
		return true // anyone signed in; the list itself holds only the caller's tenant
	case "invoice.read":
		return inv != nil && inv.Tenant == u.tenant
	case "invoice.void":
		return inv != nil && inv.Tenant == u.tenant && u.role == "owner"
	case "admin.export":
		return u.role == "staff"
	}
	return false
}

// Version 3 — one gate in front of every handler. A route cannot be added without naming
// its action, an invoice is looked up through the caller's tenant, and the handlers hold
// no checks at all.
func version3(db *store, s sessions) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern, action string, h func(w http.ResponseWriter, u user, inv *invoice)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			u, ok := s.who(r)
			if !ok {
				reply(w, 401, nil)
				return
			}
			var inv *invoice
			if id := r.PathValue("id"); id != "" {
				if inv = db.scoped(u.tenant, id); inv == nil {
					reply(w, 404, nil) // not this tenant's: for this caller it does not exist
					return
				}
			}
			if !allow(u, action, inv) {
				reply(w, 403, nil)
				return
			}
			h(w, u, inv)
		})
	}
	show := func(w http.ResponseWriter, _ user, inv *invoice) { reply(w, 200, inv) }
	route("GET /invoices", "invoice.list", func(w http.ResponseWriter, u user, _ *invoice) {
		reply(w, 200, db.list(u.tenant))
	})
	route("GET /invoices/{id}", "invoice.read", show)
	route("GET /invoices/{id}/pdf", "invoice.read", show)
	route("POST /invoices/{id}/void", "invoice.void", func(w http.ResponseWriter, _ user, inv *invoice) {
		db.void(inv.ID, true)
		reply(w, 200, nil)
	})
	route("GET /admin/export", "admin.export", func(w http.ResponseWriter, _ user, _ *invoice) {
		reply(w, 200, db.all())
	})
	return mux
}

func main() {
	part1()
	fmt.Println()
	part2()
	fmt.Println()
	part3()
}
