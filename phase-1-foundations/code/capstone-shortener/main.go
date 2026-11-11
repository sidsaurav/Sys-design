// Phase 1 capstone — a URL shortener on one server.
//
// Run:
//
//	go run ./phase-1-foundations/code/capstone-shortener             # scripted walk-through of the API
//	go run ./phase-1-foundations/code/capstone-shortener serve       # serve on localhost:8080, try it with curl
//	go run ./phase-1-foundations/code/capstone-shortener loadtest    # find the server's capacity (about 40 s)
//
// This is a reference implementation of the design in capstone-url-shortener.md.
// It uses every idea from Phase 1 and nothing from later phases: storage is an
// in-memory stand-in for a database, with a connection pool and a per-query
// delay so that it behaves like one under load.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------ config

type config struct {
	poolSize       int           // "database" connections
	dbLatency      time.Duration // how long one query holds a connection
	maxInFlight    int           // requests allowed inside the server at once
	requestTimeout time.Duration // deadline for one request, queueing included
}

var defaults = config{
	poolSize:       10,
	dbLatency:      2 * time.Millisecond,
	maxInFlight:    100,
	requestTimeout: 200 * time.Millisecond,
}

// ------------------------------------------------------------------ storage

type link struct {
	Code      string    `json:"code"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	Clicks    int64     `json:"clicks"`
}

var (
	errNotFound  = errors.New("not found")
	errDuplicate = errors.New("code already exists")
	errKeyReused = errors.New("idempotency key reused with a different request")
)

// store stands in for a database: a table of links with a unique key on the
// code, and a table of idempotency keys. Every operation takes a connection
// from a fixed pool and holds it for dbLatency, like a real query would.
type store struct {
	pool    chan struct{}
	latency time.Duration

	mu    sync.RWMutex
	links map[string]*link
	keys  map[string]idemRecord // idempotency key -> what it created
}

type idemRecord struct {
	fingerprint [32]byte
	code        string
}

func newStore(cfg config) *store {
	s := &store{
		pool:    make(chan struct{}, cfg.poolSize),
		latency: cfg.dbLatency,
		links:   map[string]*link{},
		keys:    map[string]idemRecord{},
	}
	for range cfg.poolSize {
		s.pool <- struct{}{}
	}
	return s
}

// query takes a connection, or gives up when the request's deadline passes
// (Chapter 1.6: never wait for a pool without a limit). It holds the
// connection for one query's worth of time and returns it.
func (s *store) query(ctx context.Context) error {
	select {
	case <-s.pool:
	case <-ctx.Done():
		return ctx.Err()
	}
	time.Sleep(s.latency)
	s.pool <- struct{}{}
	return nil
}

func (s *store) get(ctx context.Context, code string) (link, error) {
	if err := s.query(ctx); err != nil {
		return link{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.links[code]
	if !ok {
		return link{}, errNotFound
	}
	return *l, nil
}

// create inserts a link. If idemKey is set and has been seen before, it
// returns the link created the first time instead (replayed = true).
// The key and the link are written under one lock — the stand-in for
// "in the same database transaction" (Chapter 1.5).
func (s *store) create(ctx context.Context, l link, idemKey string, fingerprint [32]byte) (created link, replayed bool, err error) {
	if err := s.query(ctx); err != nil {
		return link{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if idemKey != "" {
		if rec, seen := s.keys[idemKey]; seen {
			if rec.fingerprint != fingerprint {
				return link{}, false, errKeyReused
			}
			return *s.links[rec.code], true, nil
		}
	}
	if _, exists := s.links[l.Code]; exists { // the unique constraint
		return link{}, false, errDuplicate
	}
	s.links[l.Code] = &l
	if idemKey != "" {
		s.keys[idemKey] = idemRecord{fingerprint: fingerprint, code: l.Code}
	}
	return l, false, nil
}

func (s *store) remove(ctx context.Context, code string) error {
	if err := s.query(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.links[code]; !ok {
		return errNotFound
	}
	delete(s.links, code)
	return nil
}

// addClicks writes a whole batch of click counts in one query.
func (s *store) addClicks(ctx context.Context, counts map[string]int64) error {
	if err := s.query(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for code, n := range counts {
		if l, ok := s.links[code]; ok {
			l.Clicks += n
		}
	}
	return nil
}

// ------------------------------------------------------------------ service

type service struct {
	cfg   config
	store *store
	slots chan struct{} // one token per request allowed in flight

	clickMu sync.Mutex
	pending map[string]int64 // clicks counted but not yet written
}

func newService(cfg config) *service {
	svc := &service{cfg: cfg, store: newStore(cfg), slots: make(chan struct{}, cfg.maxInFlight), pending: map[string]int64{}}
	go svc.flushClicksForever()
	return svc
}

// Counting a click must not cost a database write per redirect, so clicks
// are added up in memory and written in one batch every second. The price:
// a crash loses up to one second of counts. (Phase 4 does this durably.)
func (svc *service) countClick(code string) {
	svc.clickMu.Lock()
	svc.pending[code]++
	svc.clickMu.Unlock()
}

func (svc *service) flushClicks() {
	svc.clickMu.Lock()
	batch := svc.pending
	svc.pending = map[string]int64{}
	svc.clickMu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.store.addClicks(ctx, batch); err != nil {
		// Could not write: put the counts back and try again next time.
		svc.clickMu.Lock()
		for code, n := range batch {
			svc.pending[code] += n
		}
		svc.clickMu.Unlock()
	}
}

func (svc *service) flushClicksForever() {
	for range time.Tick(time.Second) {
		svc.flushClicks()
	}
}

// handler wires the routes and wraps them in the two protections every
// request path needs: a limit on requests in flight, and a deadline.
func (svc *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/links", svc.createLink)
	mux.HandleFunc("GET /v1/links/{code}", svc.describeLink)
	mux.HandleFunc("DELETE /v1/links/{code}", svc.deleteLink)
	mux.HandleFunc("GET /{code}", svc.redirect)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bounded queue (Chapter 1.6): if the server is full, say so at
		// once instead of making the caller wait in an ever-longer line.
		select {
		case svc.slots <- struct{}{}:
			defer func() { <-svc.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			problem(w, http.StatusServiceUnavailable, "overloaded", "Server is at capacity", "Try again shortly.")
			return
		}
		// Deadline (Chapter 1.5): no request may hold resources for ever.
		ctx, cancel := context.WithTimeout(r.Context(), svc.cfg.requestTimeout)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

var aliasPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{3,32}$`)

const maxURLLength = 2048

func (svc *service) createLink(w http.ResponseWriter, r *http.Request) {
	// Limit on every input (Chapter 1.4).
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		problem(w, http.StatusRequestEntityTooLarge, "body-too-large", "Request body too large", "The body may be at most 4096 bytes.")
		return
	}
	var req struct {
		URL   string `json:"url"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		problem(w, http.StatusBadRequest, "invalid-json", "Body is not valid JSON", err.Error())
		return
	}
	if msg := validateTarget(req.URL); msg != "" {
		problem(w, http.StatusUnprocessableEntity, "invalid-url", "The url field is not acceptable", msg)
		return
	}
	if req.Alias != "" && (!aliasPattern.MatchString(req.Alias) || req.Alias == "v1") {
		problem(w, http.StatusUnprocessableEntity, "invalid-alias", "The alias field is not acceptable",
			"An alias is 3 to 32 letters, digits, '-' or '_'.")
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	fingerprint := sha256.Sum256(raw)
	l := link{URL: req.URL, CreatedAt: time.Now().UTC().Truncate(time.Second)}

	// A chosen alias gets one attempt. A random code is retried on the
	// (very rare) collision; the unique constraint is what detects it.
	attempts := 5
	if req.Alias != "" {
		attempts = 1
	}
	for range attempts {
		l.Code = req.Alias
		if l.Code == "" {
			l.Code = randomCode()
		}
		created, replayed, err := svc.store.create(r.Context(), l, idemKey, fingerprint)
		switch {
		case err == nil:
			if replayed {
				w.Header().Set("Idempotent-Replayed", "true")
			}
			w.Header().Set("Location", "/"+created.Code)
			writeJSON(w, http.StatusCreated, created)
			return
		case errors.Is(err, errDuplicate):
			continue
		case errors.Is(err, errKeyReused):
			problem(w, http.StatusUnprocessableEntity, "idempotency-key-reused", "Idempotency-Key was used with a different request",
				"Use a new key for a new request.")
			return
		default:
			svc.unavailable(w)
			return
		}
	}
	if req.Alias != "" {
		problem(w, http.StatusConflict, "alias-taken", "That alias is already in use", "Choose another alias.")
		return
	}
	svc.unavailable(w)
}

func (svc *service) redirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, err := svc.store.get(r.Context(), code)
	switch {
	case errors.Is(err, errNotFound):
		problem(w, http.StatusNotFound, "not-found", "No such link", "There is no link with this code.")
	case err != nil:
		svc.unavailable(w)
	default:
		svc.countClick(code)
		// 302, not 301: a 301 may be cached by browsers for a long time,
		// which would hide clicks from us and make deletion ineffective.
		http.Redirect(w, r, l.URL, http.StatusFound)
	}
}

func (svc *service) describeLink(w http.ResponseWriter, r *http.Request) {
	l, err := svc.store.get(r.Context(), r.PathValue("code"))
	switch {
	case errors.Is(err, errNotFound):
		problem(w, http.StatusNotFound, "not-found", "No such link", "There is no link with this code.")
	case err != nil:
		svc.unavailable(w)
	default:
		svc.clickMu.Lock()
		l.Clicks += svc.pending[l.Code] // include clicks not yet flushed
		svc.clickMu.Unlock()
		writeJSON(w, http.StatusOK, l)
	}
}

func (svc *service) deleteLink(w http.ResponseWriter, r *http.Request) {
	err := svc.store.remove(r.Context(), r.PathValue("code"))
	switch {
	case errors.Is(err, errNotFound):
		problem(w, http.StatusNotFound, "not-found", "No such link", "There is no link with this code.")
	case err != nil:
		svc.unavailable(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// unavailable is the answer when we ran out of time or capacity: a 503 that
// tells the client it may retry, not a 500 that says we have a bug.
func (svc *service) unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	problem(w, http.StatusServiceUnavailable, "unavailable", "Temporarily unable to handle the request", "Try again shortly.")
}

// validateTarget returns an explanation if the URL must be refused.
func validateTarget(target string) string {
	if len(target) > maxURLLength {
		return fmt.Sprintf("The URL may be at most %d characters.", maxURLLength)
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return "The URL must be absolute, for example https://example.com/page."
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "Only http and https URLs can be shortened."
	}
	return ""
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" // base 62

// randomCode returns 7 random base-62 characters: 62^7 is about 3.5 trillion
// possibilities. It uses crypto/rand so that codes cannot be predicted.
func randomCode() string {
	const length = 7
	// 248 is the largest multiple of 62 below 256; bytes above it are
	// discarded so that every character is equally likely.
	out := make([]byte, 0, length)
	buf := make([]byte, 16)
	for len(out) < length {
		rand.Read(buf)
		for _, b := range buf {
			if b < 248 && len(out) < length {
				out = append(out, alphabet[b%62])
			}
		}
	}
	return string(out)
}

// problem writes an RFC 9457 "problem details" error (Chapter 1.4).
func problem(w http.ResponseWriter, status int, kind, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":   "https://api.example.com/problems/" + kind,
		"title":  title,
		"status": status,
		"detail": detail,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ------------------------------------------------------------------ main

func main() {
	mode := "demo"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		mode = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	cfg := defaults
	flag.IntVar(&cfg.poolSize, "pool", cfg.poolSize, "database connection pool size")
	flag.DurationVar(&cfg.dbLatency, "db-latency", cfg.dbLatency, "time one database query takes")
	flag.IntVar(&cfg.maxInFlight, "max-in-flight", cfg.maxInFlight, "requests allowed in the server at once")
	addr := flag.String("addr", "localhost:8080", "serve: address to listen on")
	rates := flag.String("rates", "1000,2000,3000,4000,4500,5000,6000", "loadtest: arrival rates to try, requests per second")
	step := flag.Duration("step", 4*time.Second, "loadtest: how long to hold each rate")
	flag.Parse()

	switch mode {
	case "demo":
		demo(cfg)
	case "serve":
		serve(cfg, *addr)
	case "loadtest":
		loadtest(cfg, *rates, *step)
	default:
		fmt.Fprintln(os.Stderr, "usage: capstone-shortener [demo|serve|loadtest] [flags]")
		os.Exit(2)
	}
}

// serve runs the service with the server-side timeouts from Chapter 1.5.
func serve(cfg config, addr string) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           newService(cfg).handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("listening on http://%s", addr)
	log.Fatal(srv.ListenAndServe())
}

// ------------------------------------------------------------------ demo

func demo(cfg config) {
	svc := newService(cfg)
	ts := httptest.NewServer(svc.handler())
	defer ts.Close()
	// Do not follow redirects: we want to see the 302 itself.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	call := func(method, path, body string, headers ...string) (int, http.Header, map[string]any) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()
		var parsed map[string]any
		json.NewDecoder(resp.Body).Decode(&parsed)
		return resp.StatusCode, resp.Header, parsed
	}
	show := func(what string, status int, extra string) {
		fmt.Printf("  %-52s -> %d %s  %s\n", what, status, http.StatusText(status), extra)
	}

	fmt.Println("1. Create a link, then follow it")
	status, hdr, body := call("POST", "/v1/links", `{"url":"https://example.com/a/very/long/path?with=parameters"}`, "Idempotency-Key", "key-1")
	code := body["code"].(string)
	show("POST /v1/links  (Idempotency-Key: key-1)", status, "Location: "+hdr.Get("Location"))
	status, hdr, _ = call("GET", "/"+code, "")
	show("GET /"+code, status, "Location: "+hdr.Get("Location"))

	fmt.Println("\n2. The response to the create was lost, so the client retries with the same key")
	status, hdr, body = call("POST", "/v1/links", `{"url":"https://example.com/a/very/long/path?with=parameters"}`, "Idempotency-Key", "key-1")
	show("POST /v1/links  (Idempotency-Key: key-1)", status,
		fmt.Sprintf("same code: %v, Idempotent-Replayed: %s", body["code"] == code, hdr.Get("Idempotent-Replayed")))
	status, _, body = call("POST", "/v1/links", `{"url":"https://example.com/something-else"}`, "Idempotency-Key", "key-1")
	show("POST /v1/links  (key-1 again, different body)", status, fmt.Sprint(body["type"]))

	fmt.Println("\n3. Requests the server must refuse")
	status, _, body = call("POST", "/v1/links", `{"url":"javascript:alert(1)"}`)
	show(`POST /v1/links  {"url":"javascript:alert(1)"}`, status, fmt.Sprint(body["type"]))
	call("POST", "/v1/links", `{"url":"https://example.com/1","alias":"docs"}`)
	status, _, body = call("POST", "/v1/links", `{"url":"https://example.com/2","alias":"docs"}`)
	show(`POST /v1/links  {"alias":"docs"} a second time`, status, fmt.Sprint(body["type"]))
	status, _, body = call("GET", "/nosuchcode", "")
	show("GET /nosuchcode", status, fmt.Sprint(body["type"]))

	fmt.Println("\n4. Clicks are counted in memory and written in batches")
	for range 5 {
		call("GET", "/"+code, "")
	}
	_, _, body = call("GET", "/v1/links/"+code, "")
	show("GET /v1/links/"+code+"  (after 6 redirects)", http.StatusOK, fmt.Sprintf("clicks: %v", body["clicks"]))

	fmt.Println("\n5. Delete, and the link stops working")
	status, _, _ = call("DELETE", "/v1/links/"+code, "")
	show("DELETE /v1/links/"+code, status, "")
	status, _, _ = call("GET", "/"+code, "")
	show("GET /"+code, status, "")
	status, _, _ = call("DELETE", "/v1/links/"+code, "")
	show("DELETE /v1/links/"+code+"  (again)", status, "idempotent: the link is gone either way")
}

// ------------------------------------------------------------------ loadtest

// loadtest finds the server's capacity with a breakpoint test: an open-loop
// load (Chapter 1.7) at one arrival rate after another.
func loadtest(cfg config, rateList string, step time.Duration) {
	svc := newService(cfg)
	ts := httptest.NewServer(svc.handler())
	defer ts.Close()

	// 10,000 links to spread the requests over.
	codes := make([]string, 10_000)
	svc.store.mu.Lock()
	for i := range codes {
		codes[i] = randomCode()
		svc.store.links[codes[i]] = &link{Code: codes[i], URL: "https://example.com/" + strconv.Itoa(i)}
	}
	svc.store.mu.Unlock()

	client := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{MaxIdleConns: 5000, MaxIdleConnsPerHost: 5000},
	}

	fmt.Printf("Pool of %d connections, %v per query -> predicted capacity %.0f redirects per second.\n",
		cfg.poolSize, cfg.dbLatency, float64(cfg.poolSize)/cfg.dbLatency.Seconds())
	fmt.Printf("At most %d requests in flight; each rate is held for %v.\n\n", cfg.maxInFlight, step)
	fmt.Println("  target/s   redirected/s   302     503   other    p50      p99      max")

	for _, field := range strings.Split(rateList, ",") {
		rate, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || rate <= 0 {
			log.Fatalf("bad rate %q", field)
		}
		var (
			wg            sync.WaitGroup
			mu            sync.Mutex
			latencies     []time.Duration // of successful redirects
			ok, shed, bad atomic.Int64
		)
		interval := time.Second / time.Duration(rate)
		begin := time.Now()
		for i := 0; time.Duration(i)*interval < step; i++ {
			scheduled := begin.Add(time.Duration(i) * interval)
			time.Sleep(time.Until(scheduled))
			target := ts.URL + "/" + codes[mrand.IntN(len(codes))]
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Get(target)
				if err != nil {
					bad.Add(1)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusFound:
					ok.Add(1)
					took := time.Since(scheduled)
					mu.Lock()
					latencies = append(latencies, took)
					mu.Unlock()
				case http.StatusServiceUnavailable:
					shed.Add(1)
				default:
					bad.Add(1)
				}
			}()
		}
		wg.Wait()
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		total := float64(ok.Load() + shed.Load() + bad.Load())
		fmt.Printf("  %6d      %6.0f       %5.1f%%  %5.1f%%  %5.1f%%  %6s   %6s   %6s\n",
			rate, float64(ok.Load())/step.Seconds(),
			float64(ok.Load())/total*100, float64(shed.Load())/total*100, float64(bad.Load())/total*100,
			ms(pct(latencies, 50)), ms(pct(latencies, 99)), ms(pct(latencies, 100)))
		time.Sleep(500 * time.Millisecond) // let the server drain between steps
	}
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}
