// Parts 3 and 4: encryption at rest, and where the key is.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

func newAEAD(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key) // 32 bytes of key: AES-256
	check(err)
	aead, err := cipher.NewGCM(block)
	check(err)
	return aead
}

// seal encrypts with a fresh random nonce, which it stores in front of the ciphertext.
// The result is bound to the associated data: the same data must be given to open it.
func seal(key cipher.AEAD, plaintext, associated []byte) []byte {
	nonce := randomBytes(key.NonceSize())
	return key.Seal(nonce, nonce, plaintext, associated)
}

func open(key cipher.AEAD, sealed, associated []byte) ([]byte, error) {
	n := key.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("too short to be ciphertext")
	}
	return key.Open(nil, sealed[:n], sealed[n:], associated)
}

// A keyService stands in for a cloud key management service. It holds one key-encryption
// key, which never leaves it. It wraps and unwraps data keys for the callers that it
// knows, and it writes down every request.
type keyService struct {
	*httptest.Server
	kek     cipher.AEAD
	callers map[string]string // credential → the caller's name
	mu      sync.Mutex
	log     []string
}

func newKeyService() *keyService {
	ks := &keyService{kek: newAEAD(randomBytes(32)), callers: map[string]string{}}
	ks.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/")
		context := []byte(r.URL.Query().Get("context")) // what the key is for; it is not secret
		body, _ := io.ReadAll(r.Body)
		who, known := ks.callers[r.Header.Get("Authorization")]
		var out []byte
		err := errors.New("refused")
		switch {
		case !known:
			who = "an unknown caller"
		case op == "wrap":
			out, err = seal(ks.kek, body, context), nil
		case op == "unwrap":
			out, err = open(ks.kek, body, context)
		}
		entry := op + " for " + who
		if err != nil {
			entry = op + " REFUSED to " + who
		}
		ks.mu.Lock()
		ks.log = append(ks.log, entry)
		ks.mu.Unlock()
		if err != nil {
			http.Error(w, "refused", 403)
			return
		}
		w.Write(out)
	}))
	return ks
}

// enrol gives a caller a credential for the key service.
func (ks *keyService) enrol(name string) (credential string) {
	credential = hex.EncodeToString(randomBytes(16))
	ks.callers[credential] = name
	return credential
}

// ask is the client side: wrap or unwrap one data key, for one context.
func (ks *keyService) ask(op, credential, context string, key []byte) ([]byte, error) {
	req, err := http.NewRequest("POST", ks.URL+"/"+op+"?context="+context, bytes.NewReader(key))
	check(err)
	req.Header.Set("Authorization", credential)
	resp, err := http.DefaultClient.Do(req)
	check(err)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, errors.New("the key service refused")
	}
	return out, nil
}

type payee struct {
	tenant  string
	id      int
	account []byte // the bank account that invoices are paid into, as it is stored
}

// A database is everything that a snapshot of it contains.
type database struct {
	payees   []payee
	settings map[string][]byte // design 2 keeps its key here
	wrapped  map[string][]byte // design 3 keeps each tenant's data key here, wrapped
}

const (
	tenants   = 50
	perTenant = 40
)

func tenantName(i int) string { return fmt.Sprintf("T%02d", i+1) }

func accountNumber(t, id int) string { return fmt.Sprintf("account %04d-%04d", t+1, id) }

// bound is the associated data for one cell: which tenant, which column, which row.
func bound(tenant string, id int) []byte {
	return []byte(fmt.Sprintf("tenant=%s|payees.account|id=%d", tenant, id))
}

// build fills a database, storing each account as the design says.
func build(store func(tenant string, id int, account string) []byte) *database {
	db := &database{settings: map[string][]byte{}, wrapped: map[string][]byte{}}
	for t := 0; t < tenants; t++ {
		for id := 1; id <= perTenant; id++ {
			db.payees = append(db.payees, payee{tenantName(t), id, store(tenantName(t), id, accountNumber(t, id))})
		}
	}
	return db
}

// count says how many accounts a reader gets back as they were written.
func count(db *database, read func(p payee) (string, error)) int {
	n := 0
	for i, p := range db.payees {
		if got, err := read(p); err == nil && got == accountNumber(i/perTenant, p.id) {
			n++
		}
	}
	return n
}

func part3() {
	ks := newKeyService()
	defer ks.Close()
	app := ks.enrol("invoices-app")

	// Design 1: the accounts as they are. The disk under the database is encrypted, which a
	// snapshot never notices: it is read through the database, above the disk.
	d1 := build(func(_ string, _ int, account string) []byte { return []byte(account) })
	plain := func(p payee) (string, error) { return string(p.account), nil }

	// Design 2: encrypted by the application, with one key, kept in a table of the same database.
	d2 := build(func(string, int, string) []byte { return nil })
	d2.settings["account_key"] = randomBytes(32)
	for i := range d2.payees {
		p := &d2.payees[i]
		p.account = seal(newAEAD(d2.settings["account_key"]), []byte(accountNumber(i/perTenant, p.id)), bound(p.tenant, p.id))
	}
	withTableKey := func(db *database) func(p payee) (string, error) {
		key := newAEAD(db.settings["account_key"]) // whoever holds the database holds this
		return func(p payee) (string, error) {
			got, err := open(key, p.account, bound(p.tenant, p.id))
			return string(got), err
		}
	}

	// Design 3: envelope encryption. A data key for each tenant is made here, used here, and
	// stored only after the key service has wrapped it.
	d3 := build(func(string, int, string) []byte { return nil })
	for t := 0; t < tenants; t++ {
		dataKey := randomBytes(32)
		wrapped, err := ks.ask("wrap", app, "tenant="+tenantName(t), dataKey)
		check(err)
		d3.wrapped[tenantName(t)] = wrapped
		for id := 1; id <= perTenant; id++ {
			p := &d3.payees[t*perTenant+id-1]
			p.account = seal(newAEAD(dataKey), []byte(accountNumber(t, id)), bound(p.tenant, id))
		}
	}
	// To read, somebody has to get each tenant's data key unwrapped, and so has to be
	// somebody that the key service will answer.
	withKeyService := func(db *database, credential string) func(p payee) (string, error) {
		keys := map[string]cipher.AEAD{}
		return func(p payee) (string, error) {
			if _, tried := keys[p.tenant]; !tried {
				keys[p.tenant] = nil
				if dataKey, err := ks.ask("unwrap", credential, "tenant="+p.tenant, db.wrapped[p.tenant]); err == nil {
					keys[p.tenant] = newAEAD(dataKey)
				}
			}
			if keys[p.tenant] == nil {
				return "", errors.New("no key")
			}
			got, err := open(keys[p.tenant], p.account, bound(p.tenant, p.id))
			return string(got), err
		}
	}

	n := tenants * perTenant
	fmt.Printf("PART 3 — a snapshot of the database is copied to a place that anyone can read. It holds %s bank\n", commas(n))
	fmt.Println("accounts. The finder has the snapshot, can reach the key service, and has no credential for it.")
	fmt.Println()
	fmt.Printf("  %-64s %16s %16s\n", "how the account numbers were stored", "the application", "the finder")
	fmt.Printf("  %-64s %16s %16s\n", "", "reads", "reads")
	line := func(what string, app, finder int) {
		fmt.Printf("  %-64s %16s %16s\n", what, commas(app), commas(finder))
	}
	line("1  as they are, on an encrypted disk", count(d1, plain), count(d1, plain))
	line("2  encrypted, with the key in a table of the same database", count(d2, withTableKey(d2)), count(d2, withTableKey(d2)))
	line("3  encrypted with a key for each tenant, and those keys", count(d3, withKeyService(d3, app)), count(d3, withKeyService(d3, "")))
	fmt.Println("     wrapped by a key that never leaves the key service")
	fmt.Println()
	fmt.Printf("  A thief who has the application's own credential for the key service reads %s of design 3.\n", commas(count(d3, withKeyService(d3, app))))
	fmt.Println()
	fmt.Println("  The key service's log, in order:")
	for i := 0; i < len(ks.log); {
		j := i
		for j < len(ks.log) && ks.log[j] == ks.log[i] {
			j++
		}
		fmt.Printf("    %3d × %s\n", j-i, ks.log[i])
		i = j
	}
}

func part4() {
	fmt.Println("PART 4 — somebody can write to the table and has no key. They copy the ciphertext of payee 2's")
	fmt.Println("account, which is their own, over payee 1's. Then the application pays an invoice of payee 1.")
	fmt.Println()
	fmt.Printf("  %-36s %s\n", "what each ciphertext was bound to", "the application decrypts payee 1's account, and gets")
	key := newAEAD(randomBytes(32)) // tenant T01's data key, as the application holds it
	for _, binds := range []bool{false, true} {
		to := func(id int) []byte {
			if !binds {
				return nil
			}
			return bound("T01", id)
		}
		row1 := seal(key, []byte(accountNumber(0, 1)), to(1))
		row2 := seal(key, []byte(accountNumber(0, 2)), to(2))
		row1 = row2 // the write: no key is needed to copy bytes
		what, result := "nothing", ""
		if binds {
			what = "its tenant, its column and its row"
		}
		if got, err := open(key, row1, to(1)); err != nil {
			result = "an error: " + err.Error()
		} else {
			result = fmt.Sprintf("%q, which is payee 2's: the money goes there", got)
		}
		fmt.Printf("  %-36s %s\n", what, result)
	}
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
