// Chapter 8.1 — one invoice, four attackers, and the controls between them.
//
// Three real HTTP servers in this process: a gateway that the internet can
// reach, an invoice service behind it, and a store behind that. One invoice
// belongs to tenant A. Four attackers try to read it. Each control in the
// design can be made to fail, alone or with others, to see which attacks then
// succeed. The attackers attack only the servers that this program starts.
//
//	part 1  the design as built: every control working, and each one failing in turn
//	part 2  how many controls must fail for each attack to succeed
//	part 3  the same, after one change to the design
//
//	go run ./phase-8-security/code/ch01-boundaries
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// The controls. Each is one check, in one place, that a mistake could remove.
var controls = []string{"session", "strip", "caller", "object", "scope"}

const tokenControl = "token" // part 3 adds this one

type invoice struct{ tenant, text string }

type system struct {
	failed   map[string]bool   // the controls that are not working
	design2  bool              // the service checks the session itself
	sessions map[string]string // session token → tenant
	invoices map[string]invoice
	secret   []byte // known to the gateway and the service, and to nobody else
	gateway  *httptest.Server
	service  *httptest.Server
	store    *httptest.Server
}

func (s *system) works(control string) bool { return !s.failed[control] }

func (s *system) sign(tenant string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(tenant))
	return hex.EncodeToString(mac.Sum(nil))
}

// The gateway is on the internet. It is the first trust boundary: on one side, anybody.
func (s *system) atGateway(w http.ResponseWriter, r *http.Request) {
	tenant := s.sessions[r.Header.Get("Authorization")]
	// Control "session": no valid session, no entry.
	if s.works("session") && tenant == "" {
		http.Error(w, "gateway: sign in first", http.StatusUnauthorized)
		return
	}
	out, _ := http.NewRequest(http.MethodGet, s.service.URL+r.URL.Path, nil)
	out.Header.Set("Authorization", r.Header.Get("Authorization"))
	// Control "strip": the tenant comes from the session, whatever header the client sent.
	// Without it, a header that the client sent is passed on as if the gateway had set it.
	if sent := r.Header.Get("X-Tenant"); sent != "" && !s.works("strip") {
		tenant = sent
	}
	out.Header.Set("X-Tenant", tenant)
	out.Header.Set("X-Gateway-Signature", s.sign(tenant))
	resp, err := http.DefaultClient.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// The service is behind the gateway. The second boundary: on one side, the gateway, and
// anything else that can reach this port.
func (s *system) atService(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get("X-Tenant")
	// Control "caller": only the gateway can have made this signature.
	if s.works("caller") && !hmac.Equal([]byte(r.Header.Get("X-Gateway-Signature")), []byte(s.sign(tenant))) {
		http.Error(w, "service: you are not the gateway", http.StatusForbidden)
		return
	}
	// Control "token", in the second design: the service asks who the session belongs to,
	// and takes no notice of the header.
	if s.design2 && s.works(tokenControl) {
		if tenant = s.sessions[r.Header.Get("Authorization")]; tenant == "" {
			http.Error(w, "service: sign in first", http.StatusUnauthorized)
			return
		}
	}
	id := strings.TrimPrefix(r.URL.Path, "/invoices/")
	resp, err := http.Get(s.store.URL + "/rows?id=" + id + "&tenant=" + tenant)
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, "service: no such invoice", http.StatusNotFound)
		return
	}
	defer resp.Body.Close()
	row, _ := io.ReadAll(resp.Body)
	owner, text, _ := strings.Cut(string(row), "|")
	// Control "object": the invoice must belong to whoever is asking.
	if s.works("object") && owner != tenant {
		http.Error(w, "service: no such invoice", http.StatusNotFound) // not 403: that would say that it exists
		return
	}
	fmt.Fprint(w, text)
}

// The store is behind the service. The third boundary.
func (s *system) atStore(w http.ResponseWriter, r *http.Request) {
	inv, found := s.invoices[r.URL.Query().Get("id")]
	// Control "scope": every query names the tenant, and sees only that tenant's rows.
	// With it:    WHERE id = $1 AND tenant = $2.    Without it:    WHERE id = $1.
	if s.works("scope") && inv.tenant != r.URL.Query().Get("tenant") {
		found = false
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	fmt.Fprint(w, inv.tenant+"|"+inv.text)
}

func build(design2 bool) *system {
	s := &system{
		failed:   map[string]bool{},
		design2:  design2,
		sessions: map[string]string{"Bearer alice": "A", "Bearer mallory": "B"},
		invoices: map[string]invoice{"1001": {"A", "INVOICE 1001: Acme Ltd, 4,200.00"}, "2001": {"B", "INVOICE 2001: Mallory Ltd, 10.00"}},
		secret:   []byte("known only to the gateway and the service"),
	}
	s.store = httptest.NewServer(http.HandlerFunc(s.atStore))
	s.service = httptest.NewServer(http.HandlerFunc(s.atService))
	s.gateway = httptest.NewServer(http.HandlerFunc(s.atGateway))
	return s
}

func (s *system) close() {
	s.gateway.Close()
	s.service.Close()
	s.store.Close()
}

// ------------------------------------------------------------ the attacks

// An attack is one request. It succeeds if tenant A's invoice comes back.
type attack struct {
	name    string
	request func(s *system) *http.Request
}

func get(url string, headers ...string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

var attacks = []attack{
	{"no session at all", func(s *system) *http.Request {
		return get(s.gateway.URL + "/invoices/1001")
	}},
	{"another tenant's ID, with her own session", func(s *system) *http.Request {
		return get(s.gateway.URL+"/invoices/1001", "Authorization", "Bearer mallory")
	}},
	{"a forged header, through the gateway", func(s *system) *http.Request {
		return get(s.gateway.URL+"/invoices/1001", "Authorization", "Bearer mallory", "X-Tenant", "A")
	}},
	{"straight to the service, past the gateway", func(s *system) *http.Request {
		return get(s.service.URL+"/invoices/1001", "X-Tenant", "A")
	}},
}

// try makes the attack against the system with these controls failing, and says how it ended.
func (s *system) try(a attack, failing ...string) (leaked bool, how string) {
	s.failed = map[string]bool{}
	for _, c := range failing {
		s.failed[c] = true
	}
	resp, err := http.DefaultClient.Do(a.request(s))
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "Acme Ltd") {
		return true, "LEAKED"
	}
	by, _, _ := strings.Cut(string(body), ":")
	return false, fmt.Sprintf("%d %s", resp.StatusCode, by)
}

// fewest finds the smallest set of controls whose failure lets the attack through, by trying
// every combination, smallest first.
func (s *system) fewest(a attack, all []string) []string {
	var best []string
	for set := 1; set < 1<<len(all); set++ {
		var failing []string
		for i, c := range all {
			if set&(1<<i) != 0 {
				failing = append(failing, c)
			}
		}
		if best != nil && len(failing) >= len(best) {
			continue
		}
		if leaked, _ := s.try(a, failing...); leaked {
			best = failing
		}
	}
	return best
}

func depth(s *system, all []string) {
	fmt.Printf("  %-44s %s\n", "attack", "the fewest failures that let it through")
	for _, a := range attacks {
		set := s.fewest(a, all)
		fmt.Printf("  %-44s %d: %s\n", a.name, len(set), strings.Join(set, ", "))
	}
}

func main() {
	s := build(false)
	fmt.Println("PART 1 — the design as built. Invoice 1001 belongs to tenant A. Four attackers ask for it.")
	fmt.Println()
	head1, head2 := fmt.Sprintf("  %-44s %-14s", "", "every control"), fmt.Sprintf("  %-44s %-14s", "attack", "working")
	for _, c := range controls {
		head1 += fmt.Sprintf(" %-14s", "without")
		head2 += fmt.Sprintf(" %-14s", c)
	}
	fmt.Println(strings.TrimRight(head1, " "))
	fmt.Println(strings.TrimRight(head2, " "))
	for _, a := range attacks {
		_, how := s.try(a)
		line := fmt.Sprintf("  %-44s %-14s", a.name, how)
		for _, c := range controls {
			_, how := s.try(a, c)
			line += fmt.Sprintf(" %-14s", how)
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
	fmt.Println()
	fmt.Println("PART 2 — how deep is the defence? Every combination of failing controls, tried.")
	fmt.Println()
	depth(s, controls)
	s.close()

	s = build(true)
	fmt.Println()
	fmt.Println("PART 3 — one change: the service asks whose session it is, and takes no notice of the header.")
	fmt.Println()
	depth(s, append(append([]string{}, controls...), tokenControl))
	s.close()
}
