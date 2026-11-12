// Chapter 3.1 — what breaks when a second server is added.
//
// A small web application keeps three things in its own memory or on its
// own disk: who is logged in, how many requests each user has made, and the
// files users upload. With one instance nobody can tell. This program runs
// the application as several instances behind a round-robin load balancer —
// all inside this one process, on local ports — and reports what a user
// experiences, four ways:
//
//  1. state in each instance's own memory;
//  2. the same, with sticky sessions at the load balancer;
//  3. state in a store that every instance shares;
//  4. the session in a signed cookie, everything else in the shared store.
//
// Run:
//
//	go run ./phase-3-scaling-out/code/ch01-stateless
//	go run ./phase-3-scaling-out/code/ch01-stateless -instances 4
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const rateLimit = 5 // requests one API client may make to /limited

// ------------------------------------------------------------ state

// state is everything the application remembers between requests. Where
// it lives is the whole subject of the chapter.
type state interface {
	get(key string) (value string, ok bool)
	set(key, value string)
	del(key string)
	incr(key string) int // add one, return the new count
}

// memory keeps state in the process: one copy per instance.
type memory struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemory() *memory { return &memory{m: map[string]string{}} }

func (s *memory) get(k string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok
}
func (s *memory) set(k, v string) { s.mu.Lock(); s.m[k] = v; s.mu.Unlock() }
func (s *memory) del(k string)    { s.mu.Lock(); delete(s.m, k); s.mu.Unlock() }
func (s *memory) incr(k string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := strconv.Atoi(s.m[k])
	s.m[k] = strconv.Itoa(n + 1)
	return n + 1
}

// shared keeps state in a separate server that every instance talks to
// over the network. Here it is a few lines of Go standing in for Redis, a
// database, or an object store.
type shared struct{ base string }

func (s shared) call(op, k, v string) (string, int) {
	resp, err := http.PostForm(s.base+"/"+op, url.Values{"k": {k}, "v": {v}})
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode
}
func (s shared) get(k string) (string, bool) {
	v, code := s.call("get", k, "")
	return v, code == http.StatusOK
}
func (s shared) set(k, v string) { s.call("set", k, v) }
func (s shared) del(k string)    { s.call("del", k, "") }
func (s shared) incr(k string) int {
	v, _ := s.call("incr", k, "")
	n, _ := strconv.Atoi(v)
	return n
}

// startStore runs the shared store and returns its address.
func startStore() string {
	data := newMemory()
	mux := http.NewServeMux()
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		if v, ok := data.get(r.FormValue("k")); ok {
			io.WriteString(w, v)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) { data.set(r.FormValue("k"), r.FormValue("v")) })
	mux.HandleFunc("/del", func(w http.ResponseWriter, r *http.Request) { data.del(r.FormValue("k")) })
	mux.HandleFunc("/incr", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, data.incr(r.FormValue("k"))) })
	return "http://" + serve(mux).Addr().String()
}

func serve(h http.Handler) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go http.Serve(ln, h)
	return ln
}

// ------------------------------------------------------------ the application

// An instance is one copy of the application.
type instance struct {
	id      int
	state   state  // sessions (unless signed), counters and uploaded files
	signKey []byte // if set, sessions are signed cookies and need no state
	ln      net.Listener
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// A signed session cookie is "user|expiry.signature". Any instance that
// knows the key can check it, without looking anything up.
func (in *instance) sign(payload string) string {
	mac := hmac.New(sha256.New, in.signKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (in *instance) verify(cookie string) (user string, ok bool) {
	encoded, signature, found := strings.Cut(cookie, ".")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if !found || err != nil || !hmac.Equal([]byte(in.sign(string(payload))), []byte(encoded+"."+signature)) {
		return "", false
	}
	user, expiry, _ := strings.Cut(string(payload), "|")
	unix, _ := strconv.ParseInt(expiry, 10, 64)
	return user, time.Now().Unix() < unix
}

// user returns who is making the request, or "" if nobody is logged in.
func (in *instance) user(r *http.Request) string {
	c, err := r.Cookie("session")
	if err != nil {
		return ""
	}
	if in.signKey != nil {
		user, _ := in.verify(c.Value)
		return user
	}
	user, _ := in.state.get("session:" + c.Value)
	return user
}

func (in *instance) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		user := r.FormValue("user")
		var value string
		if in.signKey != nil {
			value = in.sign(user + "|" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		} else {
			value = randomID()
			in.state.set("session:"+value, user)
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: value, Path: "/", HttpOnly: true})
		fmt.Fprintf(w, "logged in by instance %d", in.id)
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err == nil && in.signKey == nil {
			in.state.del("session:" + c.Value)
		}
		// A signed cookie cannot be deleted from the server: all it can do
		// is ask the browser to forget it.
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1})
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		if user := in.user(r); user != "" {
			fmt.Fprintf(w, "hello %s, from instance %d", user, in.id)
		} else {
			http.Error(w, "not logged in", http.StatusUnauthorized)
		}
	})
	mux.HandleFunc("GET /limited", func(w http.ResponseWriter, r *http.Request) {
		// A rate limit per API client: no login involved, only a counter.
		if in.state.incr("count:"+r.URL.Query().Get("client")) > rateLimit {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
		}
	})
	mux.HandleFunc("POST /avatar", func(w http.ResponseWriter, r *http.Request) {
		if user := in.user(r); user != "" {
			in.state.set("avatar:"+user, r.FormValue("image")) // stands in for a file on this instance's disk
		}
	})
	mux.HandleFunc("GET /avatar/{user}", func(w http.ResponseWriter, r *http.Request) {
		if image, ok := in.state.get("avatar:" + r.PathValue("user")); ok {
			io.WriteString(w, image)
		} else {
			http.NotFound(w, r)
		}
	})
	return mux
}

// ------------------------------------------------------------ the load balancer

// A balancer forwards each request to one of the instances: the next one
// in turn, or — with sticky sessions — the one this browser was given
// first, remembered in a cookie that the balancer sets.
type balancer struct {
	proxies []*httputil.ReverseProxy
	alive   []bool
	next    atomic.Int64
	sticky  bool
}

func (b *balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	chosen := -1
	if b.sticky {
		if c, err := r.Cookie("lb"); err == nil {
			if i, err := strconv.Atoi(c.Value); err == nil && i < len(b.alive) && b.alive[i] {
				chosen = i
			}
		}
	}
	for chosen < 0 {
		if i := int(b.next.Add(1)-1) % len(b.alive); b.alive[i] {
			chosen = i
		}
	}
	if b.sticky {
		http.SetCookie(w, &http.Cookie{Name: "lb", Value: strconv.Itoa(chosen), Path: "/"})
	}
	b.proxies[chosen].ServeHTTP(w, r)
}

// ------------------------------------------------------------ a deployment, and a user

type deployment struct {
	instances []*instance
	balancer  *balancer
	url       string
}

func deploy(n int, sticky bool, where func() state, signKey []byte) *deployment {
	d := &deployment{balancer: &balancer{sticky: sticky}}
	for i := 0; i < n; i++ {
		in := &instance{id: i + 1, state: where(), signKey: signKey}
		in.ln = serve(in.handler())
		d.instances = append(d.instances, in)
		target, _ := url.Parse("http://" + in.ln.Addr().String())
		d.balancer.proxies = append(d.balancer.proxies, httputil.NewSingleHostReverseProxy(target))
		d.balancer.alive = append(d.balancer.alive, true)
	}
	d.url = "http://" + serve(d.balancer).Addr().String()
	return d
}

// kill stops an instance, as a crash or a deploy would, and the balancer
// stops sending requests to it.
func (d *deployment) kill(i int) {
	d.instances[i].ln.Close()
	d.balancer.alive[i] = false
}

// A browser is one user's HTTP client: it keeps cookies between requests.
type browser struct {
	client *http.Client
	base   string
}

func newBrowser(base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{client: &http.Client{Jar: jar}, base: base}
}

func (b *browser) do(method, path string, form url.Values, cookie string) (status int, body string) {
	req, _ := http.NewRequest(method, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := b.client
	if cookie != "" { // present exactly this cookie, as someone who had copied it would
		req.Header.Set("Cookie", cookie)
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(text)
}

func (b *browser) sessionCookie() string {
	u, _ := url.Parse(b.base)
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == "session" {
			return "session=" + c.Value
		}
	}
	return ""
}

// count makes n requests and returns how many got the wanted status.
func (b *browser) count(n int, path string, want int) (got int) {
	for i := 0; i < n; i++ {
		if status, _ := b.do("GET", path, nil, ""); status == want {
			got++
		}
	}
	return got
}

// ------------------------------------------------------------ the experiment

// try plays the same story against one deployment and returns one result
// for each part of it.
func try(d *deployment) []string {
	ana, ben := newBrowser(d.url), newBrowser(d.url)
	_, greeting := ana.do("POST", "/login", url.Values{"user": {"ana"}}, "")
	ben.do("POST", "/login", url.Values{"user": {"ben"}}, "")
	loggedInBy, _ := strconv.Atoi(greeting[strings.LastIndex(greeting, " ")+1:])

	var out []string
	result := func(got, of int) { out = append(out, fmt.Sprintf("%d of %d", got, of)) }

	// 1. Ana browses.
	result(ana.count(12, "/me", http.StatusOK), 12)

	// 2. An API client with a limit of five requests makes twenty.
	result(ben.count(20, "/limited?client=acme", http.StatusOK), 20)

	// 3. Ana uploads a picture; Ben looks at it.
	ana.do("POST", "/avatar", url.Values{"image": {"(a picture of Ana)"}}, "")
	result(ben.count(12, "/avatar/ana", http.StatusOK), 12)

	// 4. Ben logs out. Does a copy of his old cookie still work?
	old := ben.sessionCookie()
	ben.do("POST", "/logout", nil, "")
	accepted := 0
	for i := 0; i < 12; i++ {
		if status, _ := ben.do("GET", "/me", nil, old); status == http.StatusOK {
			accepted++
		}
	}
	result(accepted, 12)

	// 5. The instance that logged Ana in stops: a crash, or a deploy.
	d.kill(loggedInBy - 1)
	result(ana.count(12, "/me", http.StatusOK), 12)
	return out
}

func main() {
	n := flag.Int("instances", 2, "how many instances of the application to run")
	flag.Parse()

	ownMemory := func() state { return newMemory() } // a separate copy for each instance
	sharedStore := func() func() state {             // one store for all the instances of a deployment
		store := shared{startStore()}
		return func() state { return store }
	}
	key := []byte(randomID())

	columns := []struct {
		name string
		d    *deployment
	}{
		{"in each instance", deploy(*n, false, ownMemory, nil)},
		{"in each instance, sticky", deploy(*n, true, ownMemory, nil)},
		{"in a shared store", deploy(*n, false, sharedStore(), nil)},
		{"signed cookie + store", deploy(*n, false, sharedStore(), key)},
	}
	rows := []string{
		"Ana logs in, then views 12 pages: pages that know her",
		fmt.Sprintf("A client limited to %d requests makes 20: allowed", rateLimit),
		"Ana uploads a picture; Ben loads it 12 times: found",
		"Ben logs out; his old cookie is replayed 12 times: accepted",
		"The instance that logged Ana in stops; 12 pages: know her",
	}
	results := make([][]string, len(columns))
	for i, c := range columns {
		results[i] = try(c.d)
	}

	fmt.Printf("%d instances of the application behind a load balancer. Where the state is kept:\n\n", *n)
	line := func(first string, cells func(i int) string) {
		var b strings.Builder
		fmt.Fprintf(&b, "%-62s", first)
		for i := range columns {
			fmt.Fprintf(&b, "%-26s", cells(i))
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
	line("", func(i int) string { return columns[i].name })
	for r, row := range rows {
		line(row, func(i int) string { return results[i][r] })
	}
}
