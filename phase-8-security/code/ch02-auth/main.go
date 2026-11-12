// Chapter 8.2 — passwords that leak, sessions that end, and tokens that are checked.
//
//	part 1  a table of 1,000 passwords is stolen. It was stored one of four ways. How long does the attacker need?
//	part 2  one account, signed in three ways. An hour later it is disabled. When does each stop working?
//	part 3  four tokens that should be refused, shown to a careless verifier and to a careful one
//
// Parts 2 and 3 use a real HTTP server inside this process. The attacks are made only on it.
//
//	go run ./phase-8-security/code/ch02-auth
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// ------------------------------------------------------------ part 1: stored passwords

// The attacker's wordlist: 10,000 passwords of the kind that people choose.
func wordlist() (list []string) {
	for _, word := range []string{"password", "qwerty", "letmein", "welcome", "dragon", "monkey", "football", "sunshine", "princess", "shadow",
		"master", "summer", "winter", "liverpool", "charlie", "freedom", "whatever", "trustno", "iloveyou", "starwars"} {
		for n := 0; n < 500; n++ {
			list = append(list, fmt.Sprintf("%s%d", word, n))
		}
	}
	return list
}

type user struct {
	password string // what the person typed. The service never stores this
	salt     []byte
}

// 1,000 users. Four in ten chose a password that is on the wordlist. The rest use sixteen random characters.
func users(words []string) (list []user) {
	for i := 0; i < 1000; i++ {
		u := user{salt: make([]byte, 16)}
		rand.Read(u.salt)
		if i%10 < 4 {
			u.password = words[(i*7919)%len(words)]
		} else {
			random := make([]byte, 12)
			rand.Read(random)
			u.password = base64.RawURLEncoding.EncodeToString(random)
		}
		list = append(list, u)
	}
	return list
}

func fast(password string, salt []byte) string {
	sum := sha256.Sum256(append(salt, password...))
	return hex.EncodeToString(sum[:])
}

// OWASP's minimum for Argon2id: 19 MiB of memory, 2 passes, 1 thread.
func slow(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
}

func part1() {
	words := wordlist()
	people := users(words)
	fmt.Printf("PART 1 — a stolen table of %s passwords, and a wordlist of %s. One core of this machine.\n", commas(len(people)), commas(len(words)))
	fmt.Println()
	fmt.Printf("  %-34s %14s %18s %30s\n", "how the passwords were stored", "guesses", "passwords found", "time to try the whole")
	fmt.Printf("  %-34s %14s %18s %30s\n", "", "a second", "", "wordlist on every user")

	// 1. SHA-256 with no salt: equal passwords have equal hashes, so the wordlist is hashed once.
	begin := time.Now()
	table := map[string]bool{}
	for _, w := range words {
		table[fast(w, nil)] = true
	}
	found := 0
	for _, u := range people {
		if table[fast(u.password, nil)] { // what is stored for this user is in the attacker's table
			found++
		}
	}
	took := time.Since(begin)
	fmt.Printf("  %-34s %14s %18d %30s\n", "SHA-256, no salt", "one pass", found, took.Round(time.Millisecond))

	// 2. SHA-256 with a salt for each user: the wordlist has to be hashed again for each one.
	begin = time.Now()
	found, guesses := 0, 0
	for _, u := range people {
		stored := fast(u.password, u.salt)
		for _, w := range words {
			guesses++
			if fast(w, u.salt) == stored {
				found++
				break
			}
		}
	}
	took = time.Since(begin)
	fmt.Printf("  %-34s %14s %18d %30s\n", "SHA-256, a salt for each user", commas(int(float64(guesses)/took.Seconds())), found, took.Round(time.Millisecond))

	// 3 and 4. Functions that are slow on purpose. The same attack, for one second each, and then arithmetic.
	for _, scheme := range []struct {
		name  string
		guess func(u user, word string, stored []byte) bool
		store func(u user) []byte
	}{
		{"bcrypt, cost 10", func(u user, w string, stored []byte) bool {
			return bcrypt.CompareHashAndPassword(stored, []byte(w)) == nil
		},
			func(u user) []byte { h, _ := bcrypt.GenerateFromPassword([]byte(u.password), 10); return h }},
		{"Argon2id, 19 MiB and 2 passes", func(u user, w string, stored []byte) bool {
			return subtle.ConstantTimeCompare(slow(w, u.salt), stored) == 1
		},
			func(u user) []byte { return slow(u.password, u.salt) }},
	} {
		u := people[5] // one of the six in ten whose password is not on the wordlist
		stored := scheme.store(u)
		begin, tried, found := time.Now(), 0, 0
		for _, w := range words {
			if time.Since(begin) > time.Second {
				break
			}
			tried++
			if scheme.guess(u, w, stored) {
				found++
				break
			}
		}
		rate := float64(tried) / time.Since(begin).Seconds()
		whole := time.Duration(float64(guesses) / rate * float64(time.Second))
		fmt.Printf("  %-34s %14.0f %18s %30s\n", scheme.name, rate, fmt.Sprintf("%d, in one second", found), fmt.Sprintf("%.1f days", whole.Hours()/24))
	}
}

// ------------------------------------------------------------ parts 2 and 3: a server that signs people in

type claims struct {
	Sub  string `json:"sub"`            // who
	Role string `json:"role,omitempty"` // what they may do
	Aud  string `json:"aud"`            // which service the token is for
	Exp  int64  `json:"exp"`            // when it stops being valid
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// token makes a JSON Web Token: header, claims and signature, each in base64, joined by dots.
func token(key []byte, alg string, c claims) string {
	body, _ := json.Marshal(c)
	signed := b64([]byte(`{"alg":"`+alg+`","typ":"JWT"}`)) + "." + b64(body)
	if alg == "none" {
		return signed + "."
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	return signed + "." + b64(mac.Sum(nil))
}

// verify checks a token. The careless version is what a hurried implementation looks like.
func verify(t string, key []byte, audience string, now time.Time, careful bool) (c claims, err error) {
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return c, errors.New("not a token")
	}
	var header struct{ Alg string }
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	json.Unmarshal(raw, &header)
	switch {
	case header.Alg == "HS256":
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(parts[0] + "." + parts[1]))
		if !hmac.Equal([]byte(b64(mac.Sum(nil))), []byte(parts[2])) {
			return c, errors.New("the signature is wrong")
		}
	case header.Alg == "none" && !careful:
		// The token says that it needs no signature, and the careless verifier believes it.
	default:
		return c, errors.New("the algorithm is not one that this service accepts")
	}
	raw, _ = base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(raw, &c) != nil {
		return c, errors.New("the claims cannot be read")
	}
	if careful && now.Unix() >= c.Exp {
		return c, errors.New("the token has expired")
	}
	if careful && c.Aud != audience {
		return c, errors.New("the token is for another service")
	}
	return c, nil
}

type server struct {
	mu       sync.Mutex
	key      []byte
	now      time.Time         // the server's clock, which the program moves
	sessions map[string]string // session ID, or refresh token → user
	disabled map[string]bool
	lookups  int // how often a request needed the store
}

func random() string {
	b := make([]byte, 16) // 128 bits from the system's random source
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *server) lookup(id string) (string, bool) {
	s.lookups++
	user, ok := s.sessions[id]
	return user, ok && !s.disabled[user]
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch r.URL.Path {
	case "/login": // the password has been checked. What is the client given to show next time?
		user := r.URL.Query().Get("user")
		switch r.URL.Query().Get("as") {
		case "session":
			id := random()
			s.sessions[id] = user
			fmt.Fprint(w, id)
		case "token":
			fmt.Fprint(w, token(s.key, "HS256", claims{Sub: user, Aud: "shop", Exp: s.now.Add(30 * 24 * time.Hour).Unix()}))
		case "pair":
			refresh := random()
			s.sessions[refresh] = user
			fmt.Fprint(w, token(s.key, "HS256", claims{Sub: user, Aud: "shop", Exp: s.now.Add(5 * time.Minute).Unix()})+" "+refresh)
		}
	case "/refresh": // a refresh token is looked up, like a session, and buys five more minutes
		user, ok := s.lookup(credential)
		if !ok {
			http.Error(w, "sign in again", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, token(s.key, "HS256", claims{Sub: user, Aud: "shop", Exp: s.now.Add(5 * time.Minute).Unix()}))
	case "/me":
		if strings.Count(credential, ".") == 2 { // a signed token: checked without asking anybody
			c, err := verify(credential, s.key, "shop", s.now, true)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, c.Sub)
			return
		}
		user, ok := s.lookup(credential) // a session ID: looked up
		if !ok {
			http.Error(w, "sign in again", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, user)
	}
}

func call(url, credential string) (int, string) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// A client holds what it was given at login, and uses it as a real client would.
type client struct {
	url, kind, credential, refresh string
}

func signIn(url, user, kind string) *client {
	_, body := call(url+"/login?user="+user+"&as="+kind, "")
	c := &client{url: url, kind: kind, credential: body}
	if kind == "pair" {
		c.credential, c.refresh, _ = strings.Cut(body, " ")
	}
	return c
}

// get asks who it is signed in as. With a pair, an expired access token is replaced once.
func (c *client) get() int {
	status, _ := call(c.url+"/me", c.credential)
	if status == http.StatusUnauthorized && c.kind == "pair" {
		if status, fresh := call(c.url+"/refresh", c.refresh); status == http.StatusOK {
			c.credential = fresh
			status, _ := call(c.url+"/me", c.credential)
			return status
		}
	}
	return status
}

func part2() {
	began := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	s := &server{key: []byte("a key that only this service knows"), now: began, sessions: map[string]string{}, disabled: map[string]bool{}}
	web := httptest.NewServer(s)
	defer web.Close()
	kinds := []string{"session", "token", "pair"}
	at := func(d time.Duration) {
		s.mu.Lock()
		s.now = began.Add(d)
		s.mu.Unlock()
	}

	fmt.Println("PART 2 — one account, signed in three ways at 09:00. At 09:02 the account is disabled.")
	fmt.Println()
	fmt.Printf("  %-38s %16s %20s %24s\n", "", "a session ID,", "a signed token,", "a token for 5 minutes,")
	fmt.Printf("  %-38s %16s %20s %24s\n", "time since signing in", "looked up", "good for 30 days", "and a refresh token")
	clients := map[string]*client{}
	for _, k := range kinds {
		clients[k] = signIn(web.URL, "maya", k)
	}
	row := func(name string, d time.Duration) {
		at(d)
		fmt.Printf("  %-38s %16d %20d %24d\n", name, clients["session"].get(), clients["token"].get(), clients["pair"].get())
	}
	row("1 minute", time.Minute)
	s.mu.Lock()
	s.disabled["maya"] = true
	s.mu.Unlock()
	fmt.Println("  — the account is disabled —")
	row("3 minutes", 3*time.Minute)
	row("6 minutes", 6*time.Minute)
	row("1 day", 24*time.Hour)
	row("29 days", 29*24*time.Hour)
	row("31 days", 31*24*time.Hour)

	// What the difference costs: another account makes 1,000 requests in an hour, each way.
	fmt.Println()
	line := fmt.Sprintf("  %-38s", "lookups in the store, 1,000 requests")
	for i, k := range kinds {
		at(0)
		c := signIn(web.URL, "omar", k)
		s.lookups = 0
		for n := 1; n <= 1000; n++ {
			at(time.Duration(n) * 3600 * time.Millisecond)
			c.get()
		}
		line += fmt.Sprintf(" %*s", []int{16, 20, 24}[i], commas(s.lookups))
	}
	fmt.Println(line)
}

func part3() {
	key := []byte("a key that only this service knows")
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	good := claims{Sub: "maya", Role: "customer", Aud: "shop", Exp: now.Add(5 * time.Minute).Unix()}
	changed := token(key, "HS256", good)
	parts := strings.Split(changed, ".")
	body, _ := json.Marshal(claims{Sub: "maya", Role: "admin", Aud: "shop", Exp: good.Exp})
	changed = parts[0] + "." + b64(body) + "." + parts[2] // the claims are altered, and the old signature kept

	fmt.Println("PART 3 — four tokens that the shop should refuse.")
	fmt.Println()
	fmt.Printf("  %-58s %-22s %s\n", "the token", "a careless verifier", "a careful one")
	for _, c := range []struct{ name, token string }{
		{"claims changed to role admin, with the old signature", changed},
		{"role admin, and a header that says: no signature needed", token(nil, "none", claims{Sub: "maya", Role: "admin", Aud: "shop", Exp: good.Exp})},
		{"properly signed, and expired yesterday", token(key, "HS256", claims{Sub: "maya", Aud: "shop", Exp: now.Add(-24 * time.Hour).Unix()})},
		{"properly signed, by the same issuer, for another service", token(key, "HS256", claims{Sub: "maya", Role: "admin", Aud: "reports", Exp: good.Exp})},
	} {
		line := fmt.Sprintf("  %-58s", c.name)
		for _, careful := range []bool{false, true} {
			got, err := verify(c.token, key, "shop", now, careful)
			verdict := "refused: " + fmt.Sprint(err)
			if err == nil {
				verdict = "ACCEPTED"
				if got.Role == "admin" {
					verdict = "ACCEPTED, as admin"
				}
			}
			if careful {
				line += " " + verdict
			} else {
				line += fmt.Sprintf(" %-22s", strings.SplitN(verdict, ":", 2)[0])
			}
		}
		fmt.Println(line)
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
	fmt.Println()
	part3()
}
