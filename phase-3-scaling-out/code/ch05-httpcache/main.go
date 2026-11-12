// Chapter 3.5 — an HTTP cache, and what the origin's headers make it do.
//
// A small cache that follows the rules of RFC 9111 stands between two
// "browsers" and an origin server 80 ms away. Nothing in it is special to
// this program: it stores and reuses responses according to the
// Cache-Control, ETag and Vary headers that the origin sends, as a CDN or
// a browser does.
//
//	part 1  what a cache near the user saves: a miss, a hit, a revalidation
//	part 2  one page, four sets of headers: who sees whose account
//	part 3  one URL, two languages: Vary
//	part 4  a new version of a script: a fixed URL against a versioned one
//	part 5  a signed URL: a private file without a login
//
// Run:
//
//	go run ./phase-3-scaling-out/code/ch05-httpcache
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ the cache

// A stored response, and the request headers it depends on.
type stored struct {
	header   http.Header
	body     []byte
	storedAt time.Time
	lifetime time.Duration     // how long it stays fresh
	varies   map[string]string // the request's value for each header named in Vary
}

// A cache receives requests, and forwards to `next` those it cannot answer
// itself. shared says which kind it is: a shared cache serves many users
// (a CDN, a proxy); a private cache serves one (a browser's).
type cache struct {
	next    string
	shared  bool
	mu      sync.Mutex
	entries map[string][]*stored // by URL; one per variant
	did     string               // what it did with the last request
}

func newCache(next string, shared bool) *cache {
	return &cache{next: next, shared: shared, entries: map[string][]*stored{}}
}

// directives parses a Cache-Control header into a map.
func directives(h http.Header) map[string]string {
	d := map[string]string{}
	for _, part := range strings.Split(h.Get("Cache-Control"), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		if name != "" {
			d[strings.ToLower(name)] = strings.Trim(value, `"`)
		}
	}
	return d
}

// mayStore applies the rules of RFC 9111, section 3. It returns how long
// the response stays fresh.
func (c *cache) mayStore(req *http.Request, resp *http.Response) (lifetime time.Duration, ok bool) {
	d := directives(resp.Header)
	if _, no := d["no-store"]; no || resp.StatusCode != http.StatusOK {
		return 0, false
	}
	if _, private := d["private"]; private && c.shared {
		return 0, false
	}
	_, sharedAge := d["s-maxage"]
	_, public := d["public"]
	if c.shared && req.Header.Get("Authorization") != "" && !sharedAge && !public {
		return 0, false // section 3.5: requests with credentials
	}
	if _, revalidate := d["no-cache"]; revalidate {
		return 0, resp.Header.Get("ETag") != "" // stored, but never reused without asking
	}
	age := d["max-age"]
	if c.shared && sharedAge {
		age = d["s-maxage"]
	}
	seconds, err := strconv.Atoi(age)
	if err != nil {
		return 0, false // no explicit lifetime: this cache does not guess
	}
	return time.Duration(seconds) * time.Second, true
}

// find returns the stored response that matches the request, taking Vary
// into account.
func (c *cache) find(req *http.Request) *stored {
	for _, s := range c.entries[req.URL.String()] {
		matches := true
		for name, value := range s.varies {
			matches = matches && req.Header.Get(name) == value
		}
		if matches {
			return s
		}
	}
	return nil
}

func (c *cache) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	send := func(s *stored, did string) {
		c.did = did
		for name, values := range s.header {
			w.Header()[name] = values
		}
		w.Header().Set("Age", strconv.Itoa(int(time.Since(s.storedAt).Seconds())))
		w.Write(s.body)
	}

	old := c.find(req)
	if req.Method == http.MethodGet && old != nil && time.Since(old.storedAt) < old.lifetime {
		send(old, "HIT: served from the cache")
		return
	}

	// Ask the next hop: the origin, or the cache in front of it.
	forward, _ := http.NewRequest(req.Method, c.next+req.URL.String(), nil)
	forward.Header = req.Header.Clone()
	if old != nil && old.header.Get("ETag") != "" {
		forward.Header.Set("If-None-Match", old.header.Get("ETag")) // "has it changed?"
	}
	resp, err := http.DefaultClient.Do(forward)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotModified && old != nil {
		old.storedAt = time.Now() // still good: fresh again
		send(old, "REVALIDATED: unchanged, served from the cache")
		return
	}
	c.did = "MISS: fetched, not stored"
	if lifetime, ok := c.mayStore(req, resp); ok && req.Method == http.MethodGet {
		s := &stored{header: resp.Header.Clone(), body: body, storedAt: time.Now(), lifetime: lifetime, varies: map[string]string{}}
		for _, name := range strings.Split(resp.Header.Get("Vary"), ",") {
			if name = strings.TrimSpace(name); name != "" {
				s.varies[name] = req.Header.Get(name)
			}
		}
		kept := c.entries[req.URL.String()][:0]
		for _, e := range c.entries[req.URL.String()] {
			if e != old {
				kept = append(kept, e)
			}
		}
		c.entries[req.URL.String()] = append(kept, s)
		c.did = "MISS: fetched and stored"
	}
	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// ------------------------------------------------------------ the origin

// origin is the application. What it sends for each page can be changed
// between experiments.
type origin struct {
	mu            sync.Mutex
	logo          []byte
	accountPolicy http.Header // the caching headers sent with /account
	welcomeVary   bool        // whether /welcome sends Vary: Accept-Language
	scriptVersion int
	requests      atomic.Int64
	bytesSent     atomic.Int64
	key           []byte // for signing URLs
}

func etag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:6]) + `"`
}

// reply sends a body with an ETag, or 304 if the client already has it.
func (o *origin) reply(w http.ResponseWriter, r *http.Request, body []byte) {
	tag := etag(body)
	w.Header().Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	o.bytesSent.Add(int64(len(body)))
	w.Write(body)
}

func (o *origin) sign(path string, expires int64) string {
	mac := hmac.New(sha256.New, o.key)
	fmt.Fprintf(mac, "%s\n%d", path, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// signedURL returns a link to a private file that works, for anyone who
// holds it, until it expires.
func (o *origin) signedURL(path string, validFor time.Duration) string {
	expires := time.Now().Add(validFor).Unix()
	return fmt.Sprintf("%s?expires=%d&signature=%s", path, expires, o.sign(path, expires))
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	time.Sleep(80 * time.Millisecond) // the origin is far away: 80 ms there and back
	o.requests.Add(1)
	o.mu.Lock()
	defer o.mu.Unlock()
	switch path := r.URL.Path; {
	case path == "/logo.png":
		w.Header().Set("Cache-Control", "public, max-age=1")
		o.reply(w, r, o.logo)

	case path == "/account": // a page that is different for every user
		for name, values := range o.accountPolicy {
			w.Header()[name] = values
		}
		user := "nobody"
		if c, err := r.Cookie("user"); err == nil {
			user = c.Value
		}
		fmt.Fprintf(w, "%s's account", user)

	case path == "/welcome": // a page that depends on the reader's language
		w.Header().Set("Cache-Control", "max-age=60")
		if o.welcomeVary {
			w.Header().Set("Vary", "Accept-Language")
		}
		if strings.HasPrefix(r.Header.Get("Accept-Language"), "fr") {
			fmt.Fprint(w, "Bienvenue")
		} else {
			fmt.Fprint(w, "Welcome")
		}

	case path == "/app.js": // a script at a fixed address
		w.Header().Set("Cache-Control", "max-age=3600")
		o.reply(w, r, []byte(fmt.Sprintf("script, version %d", o.scriptVersion)))

	case strings.HasPrefix(path, "/app.v"): // the same script, with its version in the address
		w.Header().Set("Cache-Control", "max-age=31536000, immutable")
		o.reply(w, r, []byte("script, version "+strings.TrimSuffix(strings.TrimPrefix(path, "/app.v"), ".js")))

	case path == "/index.html": // the page that says which script to load
		w.Header().Set("Cache-Control", "no-cache")
		o.reply(w, r, []byte(fmt.Sprintf("/app.v%d.js", o.scriptVersion)))

	case strings.HasPrefix(path, "/private/"):
		expires, _ := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
		valid := hmac.Equal([]byte(r.URL.Query().Get("signature")), []byte(o.sign(path, expires)))
		if !valid || time.Now().Unix() > expires {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		fmt.Fprint(w, "the private report")

	default:
		http.NotFound(w, r)
	}
}

// ------------------------------------------------------------ helpers

func serve(h http.Handler) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go http.Serve(ln, h)
	return "http://" + ln.Addr().String()
}

// get fetches a URL with the given request headers and reports the body,
// the status and how long it took.
func get(address string, headers ...string) (body string, status int, took time.Duration) {
	req, _ := http.NewRequest("GET", address, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	return string(text), resp.StatusCode, time.Since(start)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

// ------------------------------------------------------------ the experiments

func main() {
	o := &origin{logo: bytes.Repeat([]byte("logo"), 50_000), scriptVersion: 1, key: []byte("a secret known only to the origin")}
	originURL := serve(o)

	// Part 1.
	fmt.Println("PART 1 — a 200 KB logo. The origin is 80 ms away; the cache is next to the user. The logo is fresh for 1 second.")
	fmt.Println()
	fmt.Printf("  %-36s %-46s %9s %18s\n", "", "what the cache did", "took", "bytes from origin")
	edge := newCache(originURL, true)
	edgeURL := serve(edge)
	step := func(name string) {
		before := o.bytesSent.Load()
		_, _, took := get(edgeURL + "/logo.png")
		fmt.Printf("  %-36s %-46s %9s %18d\n", name, edge.did, ms(took), o.bytesSent.Load()-before)
	}
	step("the first request")
	step("the same again")
	time.Sleep(1100 * time.Millisecond)
	step("again, after it has expired")
	o.mu.Lock()
	o.logo = bytes.Repeat([]byte("LOGO"), 50_000)
	o.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	step("again, after the logo has changed")

	// Part 2.
	fmt.Println()
	fmt.Println("PART 2 — /account shows the account of whoever is logged in. Ana loads it, then Ben, through one shared cache.")
	fmt.Println()
	fmt.Printf("  %-44s %-16s %-16s %s\n", "the origin sends", "Ana sees", "Ben sees", "what the cache did for Ben")
	for _, policy := range []http.Header{
		{"Cache-Control": {"max-age=60"}},
		{"Cache-Control": {"private, max-age=60"}},
		{"Cache-Control": {"max-age=60"}, "Vary": {"Cookie"}},
		{"Cache-Control": {"no-store"}},
	} {
		o.mu.Lock()
		o.accountPolicy = policy
		o.mu.Unlock()
		edge := newCache(originURL, true)
		edgeURL := serve(edge)
		ana, _, _ := get(edgeURL+"/account", "Cookie", "user=Ana")
		ben, _, _ := get(edgeURL+"/account", "Cookie", "user=Ben")
		sent := "Cache-Control: " + policy.Get("Cache-Control")
		if v := policy.Get("Vary"); v != "" {
			sent += "; Vary: " + v
		}
		if ben != "Ben's account" {
			ben = strings.ToUpper(ben) + " !"
		}
		fmt.Printf("  %-44s %-16s %-16s %s\n", sent, ana, ben, edge.did)
	}

	// Part 3.
	fmt.Println()
	fmt.Println("PART 3 — /welcome is in the reader's language. An English reader loads it, then a French one.")
	fmt.Println()
	fmt.Printf("  %-52s %-22s %s\n", "the origin sends", "the English reader", "the French reader")
	for _, vary := range []bool{false, true} {
		o.mu.Lock()
		o.welcomeVary = vary
		o.mu.Unlock()
		edgeURL := serve(newCache(originURL, true))
		english, _, _ := get(edgeURL+"/welcome", "Accept-Language", "en")
		french, _, _ := get(edgeURL+"/welcome", "Accept-Language", "fr")
		sent := "Cache-Control: max-age=60"
		if vary {
			sent += "; Vary: Accept-Language"
		}
		fmt.Printf("  %-52s %-22s %s\n", sent, english, french)
	}

	// Part 4. A browser's own cache, behind the shared one.
	fmt.Println()
	fmt.Println("PART 4 — the site ships version 2 of its script. A returning visitor's browser has version 1 in its cache.")
	fmt.Println()
	browser := serve(newCache(serve(newCache(originURL, true)), false))
	loadPage := func() (fixed, versioned string) {
		fixed, _, _ = get(browser + "/app.js")
		which, _, _ := get(browser + "/index.html") // the page names the script it wants
		versioned, _, _ = get(browser + which)
		return fixed, versioned
	}
	fixedBefore, versionedBefore := loadPage()
	o.mu.Lock()
	o.scriptVersion = 2 // the deploy
	o.mu.Unlock()
	fixedAfter, versionedAfter := loadPage()
	fmt.Printf("  %-64s %-22s %s\n", "how the script is served", "before the deploy", "after the deploy")
	fmt.Printf("  %-64s %-22s %s\n", "/app.js, max-age=3600", fixedBefore, fixedAfter+"   <- for up to an hour")
	fmt.Printf("  %-64s %-22s %s\n", "/app.v<N>.js, max-age=31536000, immutable; the page is no-cache", versionedBefore, versionedAfter)

	// Part 5.
	fmt.Println()
	fmt.Println("PART 5 — a private file, fetched with a signed URL: no cookie, no login")
	fmt.Println()
	link := o.signedURL("/private/report.pdf", 2*time.Second)
	parsed, _ := url.Parse(link)
	fmt.Printf("  the link:  %s?expires=%s&signature=%s...\n\n", parsed.Path, parsed.Query().Get("expires"), parsed.Query().Get("signature")[:16])
	_, status, _ := get(originURL + link)
	fmt.Printf("  %-52s %d\n", "the link as issued", status)
	_, status, _ = get(originURL + strings.Replace(link, "report.pdf", "salaries.pdf", 1))
	fmt.Printf("  %-52s %d\n", "the same signature on a different file", status)
	time.Sleep(3100 * time.Millisecond) // past the 2 seconds, and past the end of the second in which they ran out
	_, status, _ = get(originURL + link)
	fmt.Printf("  %-52s %d\n", "the link as issued, after it has expired", status)
}
