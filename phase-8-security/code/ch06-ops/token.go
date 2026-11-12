// Part 2: what a leaked token can do, and until when.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

type grant struct {
	Holder string   `json:"sub"`
	May    []string `json:"scope"` // what the holder may do; "*" is everything
	Until  int64    `json:"exp"`   // 0: for ever
}

func sign(key []byte, g grant) string {
	body, _ := json.Marshal(g)
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verify(key []byte, token string, now time.Time) (grant, error) {
	var g grant
	body, sig, ok := strings.Cut(token, ".")
	raw, err1 := base64.RawURLEncoding.DecodeString(body)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	if !ok || err1 != nil || err2 != nil || !hmac.Equal(got, mac.Sum(nil)) || json.Unmarshal(raw, &g) != nil {
		return g, errors.New("not a token of ours")
	}
	if g.Until != 0 && now.Unix() >= g.Until {
		return g, errors.New("expired")
	}
	return g, nil
}

// Everything that the cloud account's API can do, and the permission that each needs.
var operations = []struct{ method, path, needs string }{
	{"POST", "/deploy/invoices", "deploy:invoices"},
	{"POST", "/deploy/ledger", "deploy:ledger"},
	{"GET", "/secrets/production", "secrets:read"},
	{"POST", "/users/admins", "users:admin"},
	{"DELETE", "/databases/production", "databases:delete"},
}

func part2() {
	key := make([]byte, 32)
	rand.Read(key)
	now := time.Date(2026, 3, 2, 14, 0, 0, 0, time.UTC) // the moment of the leak; the server's clock is moved below
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, err := verify(key, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), now)
		if err != nil {
			http.Error(w, err.Error(), 401)
			return
		}
		for _, op := range operations {
			if op.method == r.Method && op.path == r.URL.Path {
				for _, may := range g.May {
					if may == "*" || may == op.needs {
						fmt.Fprint(w, "done")
						return
					}
				}
			}
		}
		http.Error(w, "not allowed", 403)
	}))
	defer account.Close()

	// try uses a token on every operation, and says which succeeded.
	try := func(token string) string {
		var done []string
		for _, op := range operations {
			req, _ := http.NewRequest(op.method, account.URL+op.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp.StatusCode == 200 {
				done = append(done, strings.TrimPrefix(op.path, "/"))
			}
			if err == nil {
				resp.Body.Close()
			}
		}
		switch len(done) {
		case 0:
			return "nothing"
		case len(operations):
			return fmt.Sprintf("all %d operations", len(done))
		}
		return fmt.Sprintf("%d of %d: %s", len(done), len(operations), strings.Join(done, ", "))
	}

	pipelines := []struct {
		what  string
		token string
	}{
		{"a stored token that may do anything", sign(key, grant{"pipeline", []string{"*"}, 0})},
		{"a stored token for its one job", sign(key, grant{"pipeline", []string{"deploy:invoices"}, 0})},
		{"a token made for this run, for ten minutes", sign(key, grant{"pipeline", []string{"deploy:invoices"}, now.Add(10 * time.Minute).Unix()})},
	}
	fmt.Println("PART 2 — at 14:00 a build step that nobody on the team wrote prints the pipeline's token. The cloud")
	fmt.Println("account has five operations. This is what the token does for whoever reads it.")
	fmt.Println()
	fmt.Printf("  %-46s %-28s %s\n", "the pipeline holds", "at 14:05", "three days later")
	leak := now
	for _, p := range pipelines {
		now = leak.Add(5 * time.Minute)
		soon := try(p.token)
		now = leak.Add(72 * time.Hour)
		later := try(p.token)
		fmt.Printf("  %-46s %-28s %s\n", p.what, soon, later)
	}
}
