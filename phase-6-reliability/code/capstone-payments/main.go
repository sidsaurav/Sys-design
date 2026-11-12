// Capstone 6 — a payments API that keeps its promises while things fail around it.
//
// Six instances of a payments service, two in each of three zones, as real
// HTTP servers inside this process. They share a PostgreSQL database and call
// a bank, which is a stand-in inside the program: it charges a reference once
// however often it is asked, and it keeps a record of everything that it
// really charged. Merchants send payments, ask about their status, and run
// reports. In 26 seconds:
//
//	second  5 to 8    the bank takes 800 ms to answer, where it normally takes 30
//	second  8 to 10   the bank refuses everything
//	second 14         a zone is lost, with its two instances, for good
//	second 18 to 22   a sale: two and a half times the payments, twelve times the reports
//
// and throughout, one charge in a hundred is made by the bank and never
// answered. Afterwards the program compares three records: what merchants
// were told, what the database says, and what the bank charged.
//
//	go run ./phase-6-reliability/code/capstone-payments
//	go run ./phase-6-reliability/code/capstone-payments -without reconcile
//
// -without removes one safeguard: idempotency-key, deadline, breaker,
// reconcile, priority or spare.
//
// Needs the database from Phase 2 (DATABASE_URL, as before).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	without = map[string]bool{}
	pool    *pgxpool.Pool
	begin   time.Time
	client  = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2000}}
)

func since() time.Duration { return time.Since(begin) }

func between(from, to float64) bool {
	s := since().Seconds()
	return s >= from && s < to
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

const schema = `
DROP TABLE IF EXISTS pay_payments;
CREATE TABLE pay_payments (
    id         bigserial PRIMARY KEY,
    merchant   int  NOT NULL,
    key        text NOT NULL,                 -- chosen by the merchant: the same on every attempt
    amount     int  NOT NULL,
    intent     int  NOT NULL,                 -- which purchase this is. Only the checks use it
    state      text NOT NULL DEFAULT 'new',   -- new | calling | succeeded | declined | unknown
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant, key)
);
CREATE INDEX pay_unsettled ON pay_payments (updated_at) WHERE state IN ('calling', 'unknown');`

// ------------------------------------------------------------ the bank (a stand-in)

type bank struct {
	*httptest.Server
	mu        sync.Mutex
	byRef     map[string]string // reference → approved | declined
	approved  map[int]int       // intent → how many times it was really charged
	calls     atomic.Int64
	inTrouble atomic.Int64 // calls received while it was slow or refusing
}

func newBank() *bank {
	b := &bank{byRef: map[string]string{}, approved: map[int]int{}}
	rng := rand.New(rand.NewSource(1))
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.calls.Add(1)
		ref, intent := r.URL.Query().Get("ref"), 0
		fmt.Sscan(r.URL.Query().Get("intent"), &intent)
		if between(8, 10) { // refusing: nothing is charged
			b.inTrouble.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		takes := 30 * time.Millisecond
		if between(5, 8) {
			b.inTrouble.Add(1)
			takes = 800 * time.Millisecond
		}
		b.mu.Lock()
		result, seen := b.byRef[ref]
		lost := false
		if !seen { // a reference is charged once, however often it is sent
			result = "approved"
			if intent%20 == 0 {
				result = "declined"
			} else {
				b.approved[intent]++
			}
			b.byRef[ref] = result
			lost = rng.Intn(100) == 0 // charged, and the answer never arrives
		}
		b.mu.Unlock()
		if lost {
			takes = 5 * time.Second
		}
		select {
		case <-time.After(takes):
			fmt.Fprint(w, result)
		case <-r.Context().Done():
		}
	}))
	return b
}

// ------------------------------------------------------------ a breaker and a bulkhead for the bank (Chapter 6.3)

type guard struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	trial     bool
	places    chan struct{}
}

func (g *guard) allow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !without["breaker"] && g.failures >= 5 {
		if time.Now().Before(g.openUntil) || g.trial {
			return false
		}
		g.trial = true
	}
	select {
	case g.places <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *guard) done(ok bool) {
	<-g.places
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trial = false
	if ok {
		g.failures = 0
	} else if g.failures++; g.failures >= 5 {
		g.openUntil = time.Now().Add(500 * time.Millisecond)
	}
}

// ------------------------------------------------------------ one instance of the payments service

const timeLeft = "X-Time-Left-Ms" // the caller's deadline, as the time that is left (Chapter 6.2)

type instance struct {
	*httptest.Server
	zone       int
	slots      int64 // requests it will have in progress at once
	inProgress atomic.Int64
	bankGuard  *guard
	stop       chan struct{}
}

var (
	bankURL                                           string
	replayed, refusedFast, unknowns, settled, overran atomic.Int64
	refused                                           [3]atomic.Int64 // by kind of request, during the sale
)

const (
	payment = iota
	status
	report
)

// admit is the limit on requests in progress, with priorities (Chapter 6.4).
func (in *instance) admit(kind int, w http.ResponseWriter) bool {
	limit := in.slots
	if kind == report && !without["priority"] {
		limit = max(1, in.slots/4) // a report is let in only while the instance is less than a quarter busy
	}
	if in.inProgress.Add(1) > limit {
		in.inProgress.Add(-1)
		if between(18, 22) {
			refused[kind].Add(1)
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		return false
	}
	return true
}

// charge asks the bank to charge a payment. The bank charges a reference only once.
func charge(ctx context.Context, id int64, amount, intent int) string {
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/?ref=p%d&amount=%d&intent=%d", bankURL, id, amount, intent), nil)
	resp, err := client.Do(req)
	if err != nil {
		return "unknown" // no answer: it may or may not have been charged
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "new" // the bank said no at once: nothing was charged
	}
	var result string
	fmt.Fscan(resp.Body, &result)
	if result == "approved" {
		return "succeeded"
	}
	return "declined"
}

func (in *instance) pay(w http.ResponseWriter, r *http.Request) {
	if !in.admit(payment, w) {
		return
	}
	defer in.inProgress.Add(-1)
	start, left := time.Now(), 1500*time.Millisecond
	if ms, err := strconv.Atoi(r.Header.Get(timeLeft)); err == nil {
		left = time.Duration(ms) * time.Millisecond
	}
	defer func() {
		if time.Since(start) > left+50*time.Millisecond {
			overran.Add(1) // still working after the caller had gone
		}
	}()
	q := r.URL.Query()
	merchant, _ := strconv.Atoi(q.Get("merchant"))
	amount, _ := strconv.Atoi(q.Get("amount"))
	intent, _ := strconv.Atoi(q.Get("intent"))
	ctx, cancel := context.WithTimeout(r.Context(), left)
	defer cancel()

	// 1. Record the request, or find the one that was recorded before (Chapter 1.5).
	var id int64
	var state string
	err := pool.QueryRow(ctx, `INSERT INTO pay_payments (merchant, key, amount, intent) VALUES ($1, $2, $3, $4)
	                           ON CONFLICT (merchant, key) DO UPDATE SET merchant = pay_payments.merchant
	                           RETURNING id, state`, merchant, q.Get("key"), amount, intent).Scan(&id, &state)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if state == "new" { // nobody is dealing with it: take it (Chapter 2.4)
		tag, err := pool.Exec(ctx, `UPDATE pay_payments SET state = 'calling', updated_at = now() WHERE id = $1 AND state = 'new'`, id)
		if err != nil || tag.RowsAffected() == 0 {
			state = "calling"
		} else {
			state = "mine"
		}
	} else {
		replayed.Add(1)
	}
	answer := func(state string) {
		switch state {
		case "succeeded":
			w.WriteHeader(http.StatusOK)
		case "declined":
			w.WriteHeader(http.StatusPaymentRequired)
		case "new": // not charged. The same request may be sent again later
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		default: // calling, unknown: not known yet. Ask again, with the same key
			w.WriteHeader(http.StatusAccepted)
		}
	}
	if state != "mine" {
		answer(state)
		return
	}

	// 2. The bank: behind a breaker and a bulkhead, and for no longer than the time that is left.
	set := func(state string) { // the outcome is recorded even if the caller has gone
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		pool.Exec(ctx, `UPDATE pay_payments SET state = $2, updated_at = now() WHERE id = $1`, id, state)
	}
	if !in.bankGuard.allow() {
		refusedFast.Add(1)
		set("new")
		answer("new")
		return
	}
	bankCtx, bankCancel := context.WithTimeout(ctx, min(400*time.Millisecond, left-time.Since(start)-100*time.Millisecond))
	if without["deadline"] {
		bankCtx, bankCancel = context.WithTimeout(context.Background(), 10*time.Second) // "the bank will answer in the end"
	}
	outcome := charge(bankCtx, id, amount, intent)
	bankCancel()
	in.bankGuard.done(outcome == "succeeded" || outcome == "declined")
	if outcome == "unknown" {
		unknowns.Add(1)
	}
	set(outcome)
	answer(outcome)
}

func (in *instance) status(w http.ResponseWriter, r *http.Request) {
	if !in.admit(status, w) {
		return
	}
	defer in.inProgress.Add(-1)
	var state string
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if pool.QueryRow(ctx, `SELECT state FROM pay_payments WHERE merchant = $1 AND key = $2`, r.URL.Query().Get("merchant"), r.URL.Query().Get("key")).Scan(&state) != nil {
		state = "none"
	}
	fmt.Fprint(w, state)
}

func (in *instance) report(w http.ResponseWriter, r *http.Request) {
	if !in.admit(report, w) {
		return
	}
	defer in.inProgress.Add(-1)
	time.Sleep(100 * time.Millisecond) // a report is slow, and can wait
}

// reconcile finds payments whose outcome is not known, and finds it out (Chapter 5.7: finish what was started).
// A payment is left alone for a second, so that the request which is dealing with it can finish.
func (in *instance) reconcile() {
	for {
		select {
		case <-in.stop:
			return
		case <-time.After(250 * time.Millisecond):
		}
		rows, err := pool.Query(context.Background(), `
			UPDATE pay_payments SET updated_at = now()
			WHERE id IN (SELECT id FROM pay_payments WHERE state IN ('calling', 'unknown') AND updated_at < now() - interval '1 second'
			             ORDER BY id LIMIT 20 FOR UPDATE SKIP LOCKED)
			RETURNING id, amount, intent`)
		if err != nil {
			continue
		}
		type unsettled struct {
			id             int64
			amount, intent int
		}
		var batch []unsettled
		for rows.Next() {
			var u unsettled
			rows.Scan(&u.id, &u.amount, &u.intent)
			batch = append(batch, u)
		}
		rows.Close()
		for _, u := range batch {
			if !in.bankGuard.allow() {
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			outcome := charge(ctx, u.id, u.amount, u.intent) // the same reference: the bank answers as it did the first time
			cancel()
			in.bankGuard.done(outcome == "succeeded" || outcome == "declined")
			if outcome == "succeeded" || outcome == "declined" {
				pool.Exec(context.Background(), `UPDATE pay_payments SET state = $2, updated_at = now() WHERE id = $1 AND state IN ('calling', 'unknown')`, u.id, outcome)
				settled.Add(1)
			}
		}
	}
}

func newInstance(zone int, slots int64) *instance {
	// At most three quarters of an instance's places may be waiting for the bank (Chapter 6.3).
	in := &instance{zone: zone, slots: slots, bankGuard: &guard{places: make(chan struct{}, slots*3/4)}, stop: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/payments", in.pay)
	mux.HandleFunc("/status", in.status)
	mux.HandleFunc("/reports", in.report)
	in.Server = httptest.NewServer(mux)
	if !without["reconcile"] {
		go in.reconcile()
	}
	return in
}

// ------------------------------------------------------------ the balancer, and the merchants

type fleet struct {
	instances []*instance
	next      atomic.Int64
	downUntil []atomic.Int64 // by instance: not to be used before this moment
}

// send makes a request to some instance. If an instance cannot be reached at all, it tries another.
func (f *fleet) send(method, path string, left time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), left)
	defer cancel()
	for range f.instances {
		i := int(f.next.Add(1)) % len(f.instances)
		if time.Now().UnixNano() < f.downUntil[i].Load() {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, method, f.instances[i].URL+path, nil)
		req.Header.Set(timeLeft, strconv.FormatInt(left.Milliseconds(), 10))
		resp, err := client.Do(req)
		if err != nil {
			var op *net.OpError
			if errors.As(err, &op) && op.Op == "dial" { // nothing was sent: it is safe to go elsewhere
				f.downUntil[i].Store(time.Now().Add(time.Second).UnixNano())
				continue
			}
			return 0 // no answer
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	return 0
}

type told int

const (
	nothing told = iota // the merchant gave up without a final answer
	paid
	declined
)

type purchase struct {
	at, took time.Duration
	outcome  told
}

func (f *fleet) buy(intent int, rng *rand.Rand) told {
	merchant, amount := intent%50, 100+intent%900
	for attempt, asks := 1, 0; attempt <= 6 && asks < 20; {
		key := fmt.Sprint("k", intent)
		if without["idempotency-key"] {
			key = fmt.Sprint("k", intent, "-", attempt, "-", asks) // nothing connects this attempt with the last
		}
		switch f.send("POST", fmt.Sprintf("/payments?merchant=%d&key=%s&amount=%d&intent=%d", merchant, key, amount, intent), 1500*time.Millisecond) {
		case http.StatusOK:
			return paid
		case http.StatusPaymentRequired:
			return declined
		case http.StatusAccepted: // not known yet: ask again soon
			asks++
			time.Sleep(300 * time.Millisecond)
		default: // refused, or no answer: wait, longer each time, and send it again
			time.Sleep(time.Duration(50<<attempt+rng.Intn(100)) * time.Millisecond)
			attempt++
		}
	}
	return nothing
}

// ------------------------------------------------------------ the run

func main() {
	off := flag.String("without", "", "remove one safeguard: idempotency-key, deadline, breaker, reconcile, priority, spare")
	flag.Parse()
	if *off != "" {
		valid := []string{"idempotency-key", "deadline", "breaker", "reconcile", "priority", "spare"}
		if !slices.Contains(valid, *off) {
			log.Fatalf("-without %s: choose one of: %s", *off, strings.Join(valid, ", "))
		}
		without[*off] = true
		fmt.Printf("WITHOUT %s\n", *off)
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	var err error
	pool, err = pgxpool.New(context.Background(), url+"?pool_max_conns=60")
	if err == nil {
		err = pool.Ping(context.Background())
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the database of Phase 2 first: see phase-2-data/README.md", url, err)
	}
	defer pool.Close()
	_, err = pool.Exec(context.Background(), schema)
	must(err)

	b := newBank()
	bankURL = b.URL
	slots := int64(8) // for each instance: enough for the sale's payments with a zone missing
	if without["spare"] {
		slots = 3 // enough for the sale's payments, as long as no zone is lost
	}
	f := &fleet{downUntil: make([]atomic.Int64, 6)}
	for i := 0; i < 6; i++ {
		f.instances = append(f.instances, newInstance(i/2, slots))
	}
	begin = time.Now()

	var (
		wg                         sync.WaitGroup
		mu                         sync.Mutex
		purchases                  = map[int]purchase{}
		statusAsked, statusLate    int
		reportsAsked, reportsDone  int
		saleReports, saleReportsOK int
	)
	every := func(perSecond float64, during func() float64, do func(n int)) { // a stream of requests
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n, due := 0, 0.0; since() < 26*time.Second; time.Sleep(time.Millisecond) {
				for due += perSecond * during() / 1000; due >= 1; due-- {
					n++
					wg.Add(1)
					go func(n int) { defer wg.Done(); do(n) }(n)
				}
			}
		}()
	}
	sale := func(factor float64) func() float64 {
		return func() float64 {
			if between(18, 22) {
				return factor
			}
			return 1
		}
	}
	every(120, sale(2.5), func(n int) { // payments
		rng := rand.New(rand.NewSource(int64(n)))
		at := since()
		outcome := f.buy(n, rng)
		mu.Lock()
		purchases[n] = purchase{at, since() - at, outcome}
		mu.Unlock()
	})
	every(80, sale(1), func(n int) { // "what happened to my payment?"
		start := time.Now()
		code := f.send("GET", fmt.Sprintf("/status?merchant=%d&key=k%d", n%50, n), 500*time.Millisecond)
		mu.Lock()
		if between(5, 10.5) {
			statusAsked++
			if code != http.StatusOK || time.Since(start) > 200*time.Millisecond {
				statusLate++
			}
		}
		mu.Unlock()
	})
	every(40, sale(12), func(int) { // reports
		inSale := between(18, 22)
		code := f.send("GET", "/reports", 2*time.Second)
		mu.Lock()
		reportsAsked++
		if code == http.StatusOK {
			reportsDone++
		}
		if inSale {
			saleReports++
			if code == http.StatusOK {
				saleReportsOK++
			}
		}
		mu.Unlock()
	})
	go func() { // second 14: zone C is lost
		time.Sleep(14 * time.Second)
		for _, in := range f.instances[4:] {
			close(in.stop)
			in.CloseClientConnections()
			go in.Close()
		}
	}()
	wg.Wait()
	time.Sleep(4 * time.Second) // time for what is unsettled to be settled
	for _, in := range f.instances[:4] {
		close(in.stop)
	}

	// ---------------------------------------------------------------- the checks
	type row struct {
		intent int
		state  string
	}
	var ledger []row
	rows, err := pool.Query(context.Background(), `SELECT intent, state FROM pay_payments`)
	must(err)
	for rows.Next() {
		var r row
		must(rows.Scan(&r.intent, &r.state))
		ledger = append(ledger, r)
	}
	rows.Close()
	succeeded, unsettled := map[int]int{}, 0
	for _, r := range ledger {
		switch r.state {
		case "succeeded":
			succeeded[r.intent]++
		case "calling", "unknown":
			unsettled++
		}
	}
	var twice, toldPaidNotCharged, chargedNotRecorded, toldDeclinedCharged, gaveUpCharged, gaveUpAfterZone, gaveUpInSale int
	var saleTook []time.Duration // how long a purchase made during the sale took to reach its final answer
	counts := map[told]int{}
	b.mu.Lock()
	for intent, n := range b.approved {
		if n > 1 {
			twice++
		}
		if succeeded[intent] == 0 {
			chargedNotRecorded++
		}
	}
	for intent, p := range purchases {
		counts[p.outcome]++
		switch {
		case p.outcome == paid && b.approved[intent] == 0:
			toldPaidNotCharged++
		case p.outcome == declined && b.approved[intent] > 0:
			toldDeclinedCharged++
		case p.outcome == nothing:
			if b.approved[intent] > 0 {
				gaveUpCharged++
			}
			if p.at >= 14*time.Second && p.at < 18*time.Second {
				gaveUpAfterZone++
			}
			if p.at >= 18*time.Second && p.at < 22*time.Second {
				gaveUpInSale++
			}
		}
		if p.outcome != nothing && p.at >= 18*time.Second && p.at < 22*time.Second {
			saleTook = append(saleTook, p.took)
		}
	}
	b.mu.Unlock()
	sort.Slice(saleTook, func(i, j int) bool { return saleTook[i] < saleTook[j] })

	fmt.Printf("26 seconds: %d purchases. Merchants were told that %d were paid and %d declined, and gave up on %d,\n",
		len(purchases), counts[paid], counts[declined], counts[nothing])
	fmt.Printf("  of which the bank had charged %d.\n", gaveUpCharged)
	fmt.Printf("  the bank was called %d times, %d of them while it was slow or refusing\n", b.calls.Load(), b.inTrouble.Load())
	fmt.Printf("  reports: %d asked for, %d produced; during the sale, %d of %d\n", reportsAsked, reportsDone, saleReportsOK, saleReports)
	if len(saleTook) > 0 {
		fmt.Printf("  a purchase made during the sale had its answer in %d ms (median) and %d ms (99th percentile)\n",
			saleTook[len(saleTook)/2].Milliseconds(), saleTook[len(saleTook)*99/100].Milliseconds())
	}
	fmt.Println()
	fmt.Println("WHAT THE SAFEGUARDS DID")
	for _, c := range []struct {
		name string
		n    int64
	}{
		{"requests answered from the record of an earlier attempt", replayed.Load()},
		{"payments refused at once, uncharged, by the breaker or the bulkhead", refusedFast.Load()},
		{"payments whose outcome was not known when the request ended", unknowns.Load()},
		{"…and was found out afterwards", settled.Load()},
		{"reports refused for lack of room, during the sale", refused[report].Load()},
	} {
		fmt.Printf("  %-70s %6d\n", c.name, c.n)
	}
	fmt.Println("\nCHECKS")
	for _, c := range []struct {
		name string
		n    int
	}{
		{"purchases charged twice by the bank", twice},
		{"purchases the merchant was told were paid, that the bank never charged", toldPaidNotCharged},
		{"purchases the merchant was told were declined, that the bank charged", toldDeclinedCharged},
		{"purchases charged by the bank and not recorded as paid", chargedNotRecorded},
		{"payments left with their outcome unknown", unsettled},
		{"requests still being worked on after their caller's deadline", int(overran.Load())},
		{fmt.Sprintf("status queries late or refused while the bank was in trouble (of %d)", statusAsked), statusLate},
		{"purchases given up on, in the four seconds after the zone was lost", gaveUpAfterZone},
		{"payment requests refused for lack of room, during the sale", int(refused[payment].Load())},
		{"purchases given up on, during the sale", gaveUpInSale},
	} {
		fmt.Printf("  %-74s %6d\n", c.name, c.n)
	}
}
