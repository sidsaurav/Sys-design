// Capstone 8 — a security review of the invoicing API.
//
// The product of Phase 8, small enough to read: an API served over HTTP, and behind it a
// key service, a metadata service, a cloud account, an SMS provider and a build pipeline,
// which here are objects in the same process. Seventeen controls from the worked design
// can each be switched off. Eight attacks are run against it.
//
//	go run ./phase-8-security/code/capstone-review                    the brief's design, then the reviewed one
//	go run ./phase-8-security/code/capstone-review -without gate       the reviewed design, less one control
//	go run ./phase-8-security/code/capstone-review -without gate,scope
//
// With -without, the program ends with an error if any attack succeeds. Nothing is attacked
// except what this program starts. attacks.go holds the attacks and the comparison.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"
)

// The controls of the worked design, in the order of the phase's chapters.
var controls = []struct{ name, chapter, what string }{
	{"expiry", "8.2", "an access token lasts fifteen minutes"},
	{"lookup", "8.2", "the account is looked up on every request"},
	{"gate", "8.3", "one policy check stands in front of every handler"},
	{"scope", "8.3", "every lookup names the caller's tenant"},
	{"envelope", "8.4", "bank details are encrypted under keys that a key service wraps"},
	{"bind", "8.5", "a request is decoded into a type that has only the fields it may change"},
	{"dial", "8.5", "the URL fetcher refuses internal addresses as it connects"},
	{"imds", "8.5", "the metadata service wants a session token first"},
	{"role", "8.6", "the service's cloud role cannot read backups"},
	{"ledger", "8.6", "invoices marked paid are compared with payments received"},
	{"pin", "8.6", "the build checks each dependency against the hash in the lockfile"},
	{"split", "8.6", "the build step holds no credential"},
	{"least", "8.6", "the pipeline's token may deploy, and nothing else"},
	{"range", "8.6", "SMS codes are limited for each range of numbers"},
	{"cap", "8.6", "what the SMS provider may bill in an hour is capped"},
	{"grant", "8.6", "staff see a tenant only with a grant made for a ticket"},
	{"audit", "8.6", "staff access is logged, and the log's latest hash is kept elsewhere"},
}

type invoice struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Paid   bool   `json:"paid"`
	Note   string `json:"note"`
	bank   []byte // the payee's bank account, as stored
}

type account struct {
	tenant, role string
	disabled     bool
}

// A dump is what a backup of the database holds.
type dump struct {
	bank     map[string][]byte // invoice → the encrypted bank account
	tenant   map[string]string // invoice → its tenant
	wrapped  map[string][]byte // tenant → its data key, wrapped by the key service
	settings []byte            // the key itself, where the design keeps it in a table
}

type system struct {
	off      map[string]bool
	now      time.Time
	mux      *http.ServeMux
	stopped  string // the control that refused the last request
	signing  []byte
	accounts map[string]*account
	invoices map[string]*invoice
	payments map[string]bool // invoices for which money really arrived
	kek      cipher.AEAD     // the key service's key, which never leaves it
	backup   dump
	appRole  string // the cloud credential that the metadata service hands to the service
	pipeline string // the pipeline's cloud credential
	sent     map[string]int
	pence    int
	grants   map[string]bool // "agent/tenant"
	log      []string        // the audit log: each line ends with the hash of the line before
	anchor   string          // the latest hash, kept where the log's writers cannot reach
}

func (s *system) on(control string) bool { return !s.off[control] }

func aead(key []byte) cipher.AEAD {
	block, _ := aes.NewCipher(key)
	a, _ := cipher.NewGCM(block)
	return a
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func seal(k cipher.AEAD, plain []byte, about string) []byte {
	nonce := random(k.NonceSize())
	return k.Seal(nonce, nonce, plain, []byte(about))
}

func open(k cipher.AEAD, sealed []byte, about string) ([]byte, error) {
	n := k.NonceSize()
	return k.Open(nil, sealed[:n], sealed[n:], []byte(about))
}

func newSystem(without ...string) *system {
	s := &system{
		off: map[string]bool{}, now: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		signing: random(32), kek: aead(random(32)),
		accounts: map[string]*account{
			"ana": {"A", "member", false}, "omar": {"A", "owner", false}, "bo": {"B", "member", false},
			"sam": {"", "support", false},
		},
		invoices: map[string]*invoice{}, payments: map[string]bool{}, sent: map[string]int{}, grants: map[string]bool{},
		appRole: hex.EncodeToString(random(8)), pipeline: hex.EncodeToString(random(8)),
	}
	for _, c := range without {
		s.off[c] = true
	}
	// Three invoices. Their bank details are stored encrypted, and the design decides where the key is.
	s.backup = dump{bank: map[string][]byte{}, tenant: map[string]string{}, wrapped: map[string][]byte{}}
	keys := map[string]cipher.AEAD{}
	if !s.on("envelope") {
		s.backup.settings = random(32) // one key, in a table of the same database
	}
	for id, tenant := range map[string]string{"1001": "A", "1002": "B", "1003": "A"} {
		if keys[tenant] == nil {
			dataKey := random(32)
			keys[tenant] = aead(dataKey)
			s.backup.wrapped[tenant] = seal(s.kek, dataKey, tenant)
		}
		key := keys[tenant]
		if !s.on("envelope") {
			key = aead(s.backup.settings)
		}
		inv := &invoice{ID: id, Tenant: tenant, bank: seal(key, []byte("bank account of invoice "+id), id)}
		s.invoices[id] = inv
		s.backup.bank[id], s.backup.tenant[id] = inv.bank, tenant
	}
	s.routes()
	return s
}

// signIn gives a user an access token. Its life is the first control.
func (s *system) signIn(user string) string {
	life := 30 * 24 * time.Hour
	if s.on("expiry") {
		life = 15 * time.Minute
	}
	body, _ := json.Marshal(map[string]any{"sub": user, "exp": s.now.Add(life).Unix()})
	mac := hmac.New(sha256.New, s.signing)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// who checks a token's signature and its expiry and, if the design says so, the account.
func (s *system) who(r *http.Request) (string, *account) {
	body, sig, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	got, _ := base64.RawURLEncoding.DecodeString(sig)
	mac := hmac.New(sha256.New, s.signing)
	mac.Write(raw)
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if !hmac.Equal(got, mac.Sum(nil)) || json.Unmarshal(raw, &claims) != nil {
		return "", nil
	}
	if s.now.Unix() >= claims.Exp {
		s.stopped = "expiry"
		return "", nil
	}
	a := s.accounts[claims.Sub]
	if a == nil || (s.on("lookup") && a.disabled) {
		s.stopped = "lookup"
		return "", nil
	}
	return claims.Sub, a
}

// find loads an invoice for a caller: through the caller's tenant, and then past the policy.
func (s *system) find(a *account, id string) *invoice {
	inv := s.invoices[id]
	if inv != nil && s.on("scope") && inv.Tenant != a.tenant {
		s.stopped = "scope"
		return nil
	}
	if inv != nil && s.on("gate") && inv.Tenant != a.tenant {
		s.stopped = "gate"
		return nil
	}
	return inv
}

func internal(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// metadata is the cloud's metadata service: it gives the machine's own credential to
// requests from the machine. With the control, it wants a session token that a forged
// request cannot fetch.
func (s *system) metadata(hasSessionToken bool) (string, bool) {
	if s.on("imds") && !hasSessionToken {
		s.stopped = "imds"
		return "", false
	}
	return s.appRole, true
}

// readBackup is the cloud account's storage API. An administrator's credential may read
// anything. The service's own role may, unless the design has narrowed it.
func (s *system) readBackup(credential string) (dump, bool) {
	switch {
	case credential == s.pipeline && !s.on("least"):
		return s.backup, true
	case credential == s.appRole && !s.on("role"):
		return s.backup, true
	case credential == s.appRole:
		s.stopped = "role"
	case credential == s.pipeline:
		s.stopped = "least"
	}
	return dump{}, false
}

// unwrap is the key service. It serves the application's role, which has to be able to
// read bank details, and an administrator.
func (s *system) unwrap(credential, tenant string, wrapped []byte) ([]byte, bool) {
	if credential != s.appRole && !(credential == s.pipeline && !s.on("least")) {
		s.stopped = "envelope"
		return nil, false
	}
	key, err := open(s.kek, wrapped, tenant)
	return key, err == nil
}

// record adds a line to the audit log. Each line carries the hash of the one before.
func (s *system) record(line string) {
	before := ""
	if len(s.log) > 0 {
		before = digest(s.log[len(s.log)-1])
	}
	s.log = append(s.log, line+" |"+before)
	if s.on("audit") {
		s.anchor = digest(s.log[len(s.log)-1])
	}
}

func digest(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

func reply(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	if body != nil {
		json.NewEncoder(w).Encode(body)
	}
}

func (s *system) routes() {
	s.mux = http.NewServeMux()
	signedIn := func(h func(w http.ResponseWriter, r *http.Request, user string, a *account)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.stopped = ""
			user, a := s.who(r)
			if a == nil {
				reply(w, 401, nil)
				return
			}
			h(w, r, user, a)
		}
	}
	s.mux.HandleFunc("GET /invoices/{id}", signedIn(func(w http.ResponseWriter, r *http.Request, _ string, a *account) {
		inv := s.find(a, r.PathValue("id"))
		if inv == nil {
			reply(w, 404, nil)
			return
		}
		reply(w, 200, inv)
	}))
	s.mux.HandleFunc("PATCH /invoices/{id}", signedIn(func(w http.ResponseWriter, r *http.Request, _ string, a *account) {
		inv := s.find(a, r.PathValue("id"))
		if inv == nil {
			reply(w, 404, nil)
			return
		}
		if !s.on("bind") {
			json.NewDecoder(r.Body).Decode(inv) // whatever the request names is written
			reply(w, 200, inv)
			return
		}
		var edit struct {
			Note *string `json:"note"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&edit) != nil || edit.Note == nil {
			s.stopped = "bind"
			reply(w, 400, nil)
			return
		}
		inv.Note = *edit.Note
		reply(w, 200, inv)
	}))
	// "Import an invoice from a URL." The answer is shown to the caller, as the brief's design does.
	s.mux.HandleFunc("POST /import", signedIn(func(w http.ResponseWriter, r *http.Request, _ string, _ *account) {
		u, err := url.Parse(r.URL.Query().Get("url"))
		if err != nil {
			reply(w, 400, nil)
			return
		}
		ip := net.ParseIP(u.Hostname())
		if s.on("dial") && internal(ip) {
			s.stopped = "dial"
			reply(w, 400, nil)
			return
		}
		if ip.String() == "169.254.169.254" {
			if credential, ok := s.metadata(false); ok { // a plain GET, which is all that this feature can send
				reply(w, 200, credential)
				return
			}
		}
		reply(w, 502, nil)
	}))
	s.mux.HandleFunc("POST /verify-phone", func(w http.ResponseWriter, r *http.Request) {
		s.stopped = ""
		numbers := r.URL.Query().Get("range")
		switch {
		case s.on("range") && s.sent[numbers] >= 30:
			s.stopped = "range"
		case s.on("cap") && s.pence >= 1000:
			s.stopped = "cap"
		default:
			s.sent[numbers]++
			s.pence += 4
			reply(w, 200, nil)
			return
		}
		reply(w, 429, nil)
	})
	s.mux.HandleFunc("GET /admin/tenants/{tenant}/invoices", signedIn(func(w http.ResponseWriter, r *http.Request, user string, a *account) {
		tenant := r.PathValue("tenant")
		if a.role != "support" || (s.on("grant") && !s.grants[user+"/"+tenant]) {
			s.stopped = "grant"
			reply(w, 403, nil)
			return
		}
		s.record(fmt.Sprintf("%s %s viewed the invoices of tenant %s", s.now.Format("15:04"), user, tenant))
		var list []*invoice
		for _, inv := range s.invoices {
			if inv.Tenant == tenant {
				list = append(list, inv)
			}
		}
		reply(w, 200, list)
	}))
}

// do sends one request to the API and returns the status and the body. With an address it
// goes over HTTP. With none it is handed straight to the same handlers, which is how the
// search for the fewest controls runs some thousands of attacks in a second.
func (s *system) do(base, method, path, token, body string) (int, string) {
	if base == "" {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}
