// Chapter 1.5 — a lost response, a retry, and a double charge; then the fix.
//
// Run:
//
//	go run ./phase-1-foundations/code/ch05-idempotency
//
// A payment server charges customers. The client reaches it over a network
// that sometimes loses the request and sometimes loses the response. The
// program runs the same 1,000 payments four ways and counts what the server
// actually did, which is the one thing the client can never see for itself.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- server

type mode int

const (
	noKeys     mode = iota // ignore Idempotency-Key
	naiveKeys              // look the key up, do the work, then store it (racy)
	atomicKeys             // claim the key first, then do the work
)

// record is what the server remembers about one idempotency key.
type record struct {
	fingerprint [32]byte // hash of the request body the key was first used with
	done        bool     // false while the first request is still being processed
	status      int      // the stored response, replayed to every retry
	body        string
}

type server struct {
	mode mode
	work time.Duration // how long charging a card takes

	mu      sync.Mutex
	charges map[string]int // customer -> number of times charged
	keys    map[string]*record
	replays int // retries answered from a stored response
}

func newServer(m mode, work time.Duration) *server {
	return &server{mode: m, work: work, charges: map[string]int{}, keys: map[string]*record{}}
}

// charge is the side effect we must not repeat.
func (s *server) charge(customer string) (int, string) {
	time.Sleep(s.work)
	s.mu.Lock()
	s.charges[customer]++
	n := s.charges[customer]
	s.mu.Unlock()
	return http.StatusCreated, fmt.Sprintf(`{"customer":%q,"charge_number":%d}`, customer, n)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	customer := string(raw) // the body is just the customer id, to keep the demo small
	// In a real service the key is scoped to the authenticated caller, so
	// that two customers who pick the same key cannot see each other's result.
	key := r.Header.Get("Idempotency-Key")

	switch s.mode {
	case noKeys:
		status, body := s.charge(customer)
		w.WriteHeader(status)
		io.WriteString(w, body)

	case naiveKeys:
		// BUG: "have I seen this key?" and "remember this key" are two
		// separate steps with slow work in between. Two requests with the
		// same key can both get past the first step.
		s.mu.Lock()
		rec := s.keys[key]
		s.mu.Unlock()
		if rec != nil {
			w.WriteHeader(rec.status)
			io.WriteString(w, rec.body)
			return
		}
		status, body := s.charge(customer)
		s.mu.Lock()
		s.keys[key] = &record{done: true, status: status, body: body}
		s.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, body)

	case atomicKeys:
		fingerprint := sha256.Sum256(raw)

		// One atomic step: either we claim the key, or we learn that
		// somebody else already has. In a database this is an INSERT
		// into a table with a unique constraint on the key.
		s.mu.Lock()
		rec, seen := s.keys[key]
		if !seen {
			s.keys[key] = &record{fingerprint: fingerprint}
		}
		var prior record
		if seen {
			prior = *rec
			if prior.done && prior.fingerprint == fingerprint {
				s.replays++
			}
		}
		s.mu.Unlock()

		if seen {
			switch {
			case prior.fingerprint != fingerprint:
				// Same key, different request: a client bug. Refuse.
				http.Error(w, "this Idempotency-Key was used with a different request", http.StatusUnprocessableEntity) // 422
			case !prior.done:
				// The first attempt is still running. Do not start a second.
				w.Header().Set("Retry-After", "1")
				http.Error(w, "a request with this Idempotency-Key is in progress", http.StatusConflict) // 409
			default:
				// Finished earlier: send the same answer again, do no work.
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(prior.status)
				io.WriteString(w, prior.body)
			}
			return
		}

		status, body := s.charge(customer)

		// In a database, the charge and this update commit in the same
		// transaction, so a crash cannot leave one without the other.
		s.mu.Lock()
		rec = s.keys[key]
		rec.done, rec.status, rec.body = true, status, body
		s.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func (s *server) totals() (charges, chargedMoreThanOnce int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.charges {
		charges += n
		if n > 1 {
			chargedMoreThanOnce++
		}
	}
	return
}

// ---------------------------------------------------------------- network

var errLost = errors.New("network: no response")

// lossyNetwork loses 5% of requests before they reach the server and 10% of
// responses on the way back. The client sees the same error for both.
// Decisions are derived from the request, not from a shared random source,
// so the program prints the same numbers on every run.
type lossyNetwork struct {
	next http.RoundTripper
	mu   sync.Mutex
	seen map[string]int // request body -> how many times it has been sent
}

func (n *lossyNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(strings.NewReader(string(raw)))

	n.mu.Lock()
	n.seen[string(raw)]++
	attempt := n.seen[string(raw)]
	n.mu.Unlock()

	h := fnv.New32a()
	fmt.Fprintf(h, "%s#%d", raw, attempt)
	roll := h.Sum32() % 100

	if roll < 5 {
		return nil, errLost // the request never arrived; nothing happened
	}
	resp, err := n.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if roll < 15 {
		// The server did the work. The answer is lost on the way back.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, errLost
	}
	return resp, nil
}

// ---------------------------------------------------------------- client

type client struct {
	http        *http.Client
	url         string
	maxAttempts int  // 1 means "never retry"
	sendKey     bool // send an Idempotency-Key header
}

// backoff returns how long to wait before retry number `attempt` (1, 2, ...):
// a random time between zero and an exponentially growing, capped limit.
// This is the "full jitter" strategy.
func backoff(attempt int, rng *rand.Rand) time.Duration {
	const (
		base     = 5 * time.Millisecond
		maxDelay = 200 * time.Millisecond
	)
	limit := min(base<<attempt, maxDelay)
	return time.Duration(rng.Int64N(int64(limit) + 1))
}

// pay tries to charge one customer once. It returns the status codes it saw,
// one per attempt (0 means "no response").
func (c *client) pay(customer, key string, rng *rand.Rand) (statuses []int, ok bool) {
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff(attempt, rng))
		}
		// Every attempt gets its own deadline, so a server that never
		// answers cannot hold us for ever.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(customer))
		if c.sendKey {
			// The same key on every attempt: that is the whole point.
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			statuses = append(statuses, 0)
			continue // outcome unknown: maybe charged, maybe not
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		statuses = append(statuses, resp.StatusCode)

		switch {
		case resp.StatusCode < 300:
			return statuses, true
		case resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			continue // worth trying again
		default:
			return statuses, false // any other 4xx: the request is wrong, retrying cannot help
		}
	}
	return statuses, false
}

// ---------------------------------------------------------------- scenarios

const payments = 1000

// run sends 1,000 payments, 50 at a time, and reports what the client
// believed and what the server did.
func run(title string, m mode, maxAttempts int, sendKey bool) {
	srv := newServer(m, time.Millisecond)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := &client{
		http:        &http.Client{Transport: &lossyNetwork{next: http.DefaultTransport, seen: map[string]int{}}},
		url:         ts.URL,
		maxAttempts: maxAttempts,
		sendKey:     sendKey,
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		failed    []string
		slots     = make(chan struct{}, 50)
	)
	for i := range payments {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			rng := rand.New(rand.NewPCG(uint64(i), 42)) // per-payment, so runs are repeatable
			customer := fmt.Sprintf("cust-%04d", i)
			key := fmt.Sprintf("key-%016x", rng.Uint64()) // real clients use a random UUID
			_, ok := c.pay(customer, key, rng)
			mu.Lock()
			if ok {
				succeeded++
			} else {
				failed = append(failed, customer)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	charges, multi := srv.totals()
	chargedAnyway := 0
	srv.mu.Lock()
	for _, customer := range failed {
		if srv.charges[customer] > 0 {
			chargedAnyway++
		}
	}
	replays := srv.replays
	srv.mu.Unlock()

	fmt.Println(title)
	fmt.Printf("  client was told \"paid\":             %4d\n", succeeded)
	fmt.Printf("  client was told nothing (error):    %4d", len(failed))
	if len(failed) > 0 {
		fmt.Printf("   <- %d of these customers were charged anyway", chargedAnyway)
	}
	fmt.Println()
	fmt.Printf("  charges the server actually made:   %4d\n", charges)
	fmt.Printf("  customers charged more than once:   %4d\n", multi)
	if sendKey {
		fmt.Printf("  retries answered from stored result:%4d\n", replays)
	}
	fmt.Println()
}

// overlap sends the same payment twice, 10 ms apart, while the server needs
// 50 ms to process it — a retry that arrives before the first attempt ends.
func overlap(title string, m mode) {
	srv := newServer(m, 50*time.Millisecond)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := &client{http: &http.Client{}, url: ts.URL, maxAttempts: 6, sendKey: true}

	var wg sync.WaitGroup
	var retryStatuses []int
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.pay("cust-race", "key-race", rand.New(rand.NewPCG(1, 1)))
	}()
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		retryStatuses, _ = c.pay("cust-race", "key-race", rand.New(rand.NewPCG(2, 2)))
	}()
	wg.Wait()

	charges, _ := srv.totals()
	fmt.Printf("  %-38s charges: %d   the retry saw: %v\n", title, charges, compact(retryStatuses))
}

// compact turns [409 409 409 201] into "409 x3, 201".
func compact(statuses []int) string {
	var parts []string
	for i := 0; i < len(statuses); {
		j := i
		for j < len(statuses) && statuses[j] == statuses[i] {
			j++
		}
		if j-i > 1 {
			parts = append(parts, fmt.Sprintf("%d x%d", statuses[i], j-i))
		} else {
			parts = append(parts, fmt.Sprint(statuses[i]))
		}
		i = j
	}
	return strings.Join(parts, ", ")
}

func main() {
	fmt.Printf("%d payments over a network that loses 5%% of requests and 10%% of responses.\n\n", payments)

	run("1. No retries", noKeys, 1, false)
	run("2. Retries, no idempotency key", noKeys, 6, false)
	run("3. Retries with an idempotency key", atomicKeys, 6, true)

	fmt.Println("4. A retry arrives while the first attempt is still running")
	overlap("look up, work, then store (racy):", naiveKeys)
	overlap("claim the key first (atomic):", atomicKeys)
}
