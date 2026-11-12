// Capstone 4 — a notification platform.
//
// Other teams ask it to tell a user something. It decides which channels to
// use, queues one delivery per channel, and sends each through an outside
// provider — within that provider's rate limit, retrying when the provider
// fails, never sending marketing to someone who has opted out, and never
// sending a login code that has expired.
//
// The database is PostgreSQL and is real; the deliveries table is also the
// queue. The three providers (email, SMS, push) are stand-ins inside this
// program: they have rate limits, they go down, and only two of them accept
// an idempotency key.
//
//	go run ./phase-4-async/code/capstone-notify                      # about 30 seconds, then the checks
//	go run ./phase-4-async/code/capstone-notify -without check-at-send
//
// -without removes one safeguard: check-at-send, reserved, ttl or request-key.
// Needs the database from Phase 2 (DATABASE_URL, as before).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ctx     = context.Background()
	pool    *pgxpool.Pool
	without = map[string]bool{}
	began   time.Time
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// The run is the design scaled down: a login code is good for 3 seconds
// here (10 minutes in the design), and providers accept hundreds a second.
const (
	codeLife  = 3 * time.Second
	lease     = 1.0 // seconds a delivery stays hidden once a sender has taken it
	maxTries  = 12
	emailRate = 300 // what each provider accepts per second
	smsRate   = 60
	pushRate  = 1000
)

const schema = `
DROP TABLE IF EXISTS nt_deliveries, nt_notifications, nt_users;
CREATE TABLE nt_users (id int PRIMARY KEY, marketing_ok boolean NOT NULL DEFAULT true);
CREATE TABLE nt_notifications (
    id          bigserial PRIMARY KEY,
    request_key text NOT NULL,          -- chosen by the caller: the same request, sent twice, is one notification
    user_id     int NOT NULL,
    class       text NOT NULL,          -- 'code', 'transactional' or 'marketing'
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at  timestamptz             -- after this it is no longer worth sending
);
CREATE UNIQUE INDEX nt_one_per_request ON nt_notifications (request_key);
-- One row for each notification and channel. This table is the queue.
CREATE TABLE nt_deliveries (
    id              bigserial PRIMARY KEY,
    notification_id bigint NOT NULL REFERENCES nt_notifications,
    channel         text NOT NULL,      -- 'email', 'sms' or 'push'
    class           text NOT NULL,
    state           text NOT NULL DEFAULT 'waiting',  -- then: sent, suppressed, expired or dead
    attempts        int NOT NULL DEFAULT 0,
    visible_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    sent_at         timestamptz,
    UNIQUE (notification_id, channel)
);
CREATE INDEX nt_waiting ON nt_deliveries (channel, class, visible_at) WHERE state = 'waiting';
INSERT INTO nt_users SELECT g FROM generate_series(1, 20000) g;`

// ------------------------------------------------------------ accepting a request

var channelsFor = map[string][]string{
	"code":          {"sms"},
	"transactional": {"email", "push"},
	"marketing":     {"email"},
}

// accept records a notification and its deliveries in one statement: both
// exist or neither does, and a request that was already accepted changes nothing.
func accept(key string, user int, class string) (isNew bool) {
	var expires *time.Time
	if class == "code" {
		t := time.Now().Add(codeLife)
		expires = &t
	}
	if class == "marketing" && without["check-at-send"] {
		// The alternative to checking when sending: check the preference now, once.
		var ok bool
		must(pool.QueryRow(ctx, "SELECT marketing_ok FROM nt_users WHERE id = $1", user).Scan(&ok))
		if !ok {
			return false
		}
	}
	conflict := "ON CONFLICT (request_key) DO NOTHING"
	if without["request-key"] {
		conflict, key = "", fmt.Sprint(key, "-", rand.Int63()) // every call is taken to be a new request
	}
	tag, err := pool.Exec(ctx, `
		WITH n AS (INSERT INTO nt_notifications (request_key, user_id, class, expires_at)
		           VALUES ($1, $2, $3, $4) `+conflict+` RETURNING id)
		INSERT INTO nt_deliveries (notification_id, channel, class)
		SELECT n.id, c, $3 FROM n, unnest($5::text[]) AS c`, key, user, class, expires, channelsFor[class])
	must(err)
	return tag.RowsAffected() > 0
}

// ------------------------------------------------------------ the providers (stand-ins)

var (
	errBusy = errors.New("429: too many requests")
	errDown = errors.New("the provider did not answer")
)

type sent struct {
	user  int
	class string
	at    time.Time
}

type provider struct {
	name     string
	perSec   float64
	takesKey bool // does it recognise an idempotency key and refuse to send twice?
	downFrom time.Duration
	downTo   time.Duration

	mu       sync.Mutex
	allowed  float64
	lastSeen time.Time
	keys     map[int64]int // delivery → how many times it was really sent
	log      []sent
	refused  int // times we went over its limit
}

func (p *provider) send(delivery int64, user int, class string) error {
	since := time.Since(began)
	if since >= p.downFrom && since < p.downTo {
		time.Sleep(20 * time.Millisecond)
		return errDown
	}
	p.mu.Lock()
	now := time.Now()
	p.allowed = min(p.perSec/5, p.allowed+now.Sub(p.lastSeen).Seconds()*p.perSec)
	p.lastSeen = now
	if p.allowed < 1 {
		p.refused++
		p.mu.Unlock()
		return errBusy
	}
	p.allowed--
	if p.takesKey && p.keys[delivery] > 0 {
		p.mu.Unlock()
		return nil // seen before: accepted, and not sent again
	}
	p.keys[delivery]++
	p.log = append(p.log, sent{user, class, now})
	p.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	return nil
}

// ------------------------------------------------------------ sending

// pace hands out permission to call a provider, at a steady rate.
type pace struct {
	mu   sync.Mutex
	gap  time.Duration
	next time.Time
}

func (p *pace) wait() {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	at := p.next
	p.next = p.next.Add(p.gap)
	p.mu.Unlock()
	time.Sleep(time.Until(at))
}

const takeSQL = `
UPDATE nt_deliveries SET attempts = attempts + 1, visible_at = clock_timestamp() + make_interval(secs => $3)
WHERE id = (SELECT id FROM nt_deliveries
            WHERE state = 'waiting' AND channel = $1 AND class = ANY ($2) AND visible_at <= clock_timestamp()
            ORDER BY visible_at, id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id, notification_id, class, attempts`

func finish(id int64, state string) {
	_, err := pool.Exec(ctx, "UPDATE nt_deliveries SET state = $2, sent_at = clock_timestamp() WHERE id = $1", id, state)
	must(err)
}

// sender takes deliveries of the given classes for one channel, and sends them.
func sender(p *provider, classes []string, limit *pace, stop *atomic.Bool, died *atomic.Int64) {
	rng := rand.New(rand.NewSource(rand.Int63()))
	for !stop.Load() {
		var id, notification int64
		var class string
		var attempts int
		if pool.QueryRow(ctx, takeSQL, p.name, classes, lease).Scan(&id, &notification, &class, &attempts) != nil {
			time.Sleep(20 * time.Millisecond) // nothing waiting
			continue
		}
		// Everything that decides whether to send is read now, not when the request was accepted.
		var user int
		var expired, marketingOK bool
		must(pool.QueryRow(ctx, `SELECT n.user_id, coalesce(n.expires_at < clock_timestamp(), false), u.marketing_ok
		                         FROM nt_notifications n JOIN nt_users u ON u.id = n.user_id WHERE n.id = $1`, notification).
			Scan(&user, &expired, &marketingOK))
		switch {
		case expired && !without["ttl"]:
			finish(id, "expired")
			continue
		case class == "marketing" && !marketingOK && !without["check-at-send"]:
			finish(id, "suppressed")
			continue
		}
		limit.wait()
		err := p.send(id, user, class)
		switch {
		case err == nil && rng.Intn(200) == 0:
			died.Add(1) // the sender dies here: sent, and not recorded. The delivery reappears when its lease runs out
		case err == nil:
			finish(id, "sent")
		case attempts >= maxTries:
			finish(id, "dead")
		default: // try again later, waiting longer each time: 0.2 s, 0.4 s, 0.8 s… up to 3 s
			wait := min(3.0, 0.2*float64(int(1)<<min(attempts-1, 10)))
			_, err := pool.Exec(ctx, "UPDATE nt_deliveries SET visible_at = clock_timestamp() + make_interval(secs => $2) WHERE id = $1", id, wait)
			must(err)
		}
	}
}

// ------------------------------------------------------------ the run

func main() {
	off := flag.String("without", "", "remove one safeguard: check-at-send, reserved, ttl, request-key")
	flag.Parse()
	if *off != "" {
		without[*off] = true
		fmt.Printf("WITHOUT %s\n", *off)
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	var err error
	pool, err = pgxpool.New(ctx, url+"?pool_max_conns=40")
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the database of Phase 2 first: see phase-2-data/README.md", url, err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, schema)
	must(err)

	began = time.Now()
	email := &provider{name: "email", perSec: emailRate, takesKey: true, downFrom: 9 * time.Second, downTo: 13 * time.Second, keys: map[int64]int{}}
	sms := &provider{name: "sms", perSec: smsRate, takesKey: false, downFrom: 17 * time.Second, downTo: 22 * time.Second, keys: map[int64]int{}}
	push := &provider{name: "push", perSec: pushRate, takesKey: true, downFrom: -1, downTo: -1, keys: map[int64]int{}}

	// Senders. Email is shared by two classes, so a third of its rate — and
	// some senders — are kept for transactional mail, whatever else is waiting.
	var stop atomic.Bool
	var died atomic.Int64
	var senders sync.WaitGroup
	start := func(n int, p *provider, classes []string, limit *pace) {
		for i := 0; i < n; i++ {
			senders.Add(1)
			go func() { defer senders.Done(); sender(p, classes, limit, &stop, &died) }()
		}
	}
	perSecond := func(n float64) *pace { return &pace{gap: time.Duration(float64(time.Second) / (n * 0.9))} } // 10% under the limit
	if without["reserved"] {
		start(10, email, []string{"transactional", "marketing"}, perSecond(emailRate))
	} else {
		start(4, email, []string{"transactional"}, perSecond(emailRate/3))
		start(6, email, []string{"marketing"}, perSecond(emailRate*2/3))
	}
	start(4, sms, []string{"code"}, perSecond(smsRate))
	start(6, push, []string{"transactional"}, perSecond(pushRate))

	// Traffic, for 25 seconds: login codes and order updates all the time;
	// at second 4, a campaign of 4,000 marketing emails; at second 6, 400 of
	// the campaign's recipients opt out of marketing.
	fmt.Println("25 seconds of requests: 20 login codes and 40 order updates a second; one request in twenty is sent twice.")
	fmt.Println("  second  4: a campaign of 4,000 marketing emails is queued")
	fmt.Println("  second  6: 400 of its recipients opt out of marketing")
	fmt.Println("  seconds 9 to 13: the email provider is down          seconds 17 to 22: the SMS provider is down")
	var optedOut sync.Map // user → when
	var accepted, repeats atomic.Int64
	var traffic sync.WaitGroup
	rng := rand.New(rand.NewSource(42))
	request := func(key string, user int, class string) {
		traffic.Add(1)
		twice := rng.Intn(20) == 0
		go func() {
			defer traffic.Done()
			if accept(key, user, class) {
				accepted.Add(1)
			}
			if twice { // the caller was not sure, and asks again with the same key
				repeats.Add(1)
				accept(key, user, class)
			}
		}()
	}
	for tick := 0; tick < 250; tick++ { // a tenth of a second each
		second := tick / 10
		for i := 0; i < 2; i++ {
			request(fmt.Sprint("code-", tick, "-", i), 10001+rng.Intn(10000), "code")
		}
		for i := 0; i < 4; i++ {
			request(fmt.Sprint("order-", tick, "-", i), 10001+rng.Intn(10000), "transactional")
		}
		if tick == 40 {
			for user := 1; user <= 4000; user++ {
				request(fmt.Sprint("spring-sale-", user), user, "marketing")
			}
		}
		if tick == 60 {
			traffic.Wait()
			_, err := pool.Exec(ctx, "UPDATE nt_users SET marketing_ok = false WHERE id BETWEEN 3601 AND 4000")
			must(err)
			for user := 3601; user <= 4000; user++ {
				optedOut.Store(user, time.Now())
			}
		}
		time.Sleep(time.Until(began.Add(time.Duration(tick+1) * 100 * time.Millisecond)))
		_ = second
	}
	traffic.Wait()

	// Let the queue drain, then stop the senders.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var waiting int
		must(pool.QueryRow(ctx, "SELECT count(*) FROM nt_deliveries WHERE state = 'waiting'").Scan(&waiting))
		if waiting == 0 {
			break
		}
	}
	stop.Store(true)
	senders.Wait()
	report(email, sms, push, &optedOut, accepted.Load(), repeats.Load(), died.Load())
	_, err = pool.Exec(ctx, "DROP TABLE IF EXISTS nt_deliveries, nt_notifications, nt_users")
	must(err)
}

func report(email, sms, push *provider, optedOut *sync.Map, accepted, repeats, died int64) {
	fmt.Printf("\nWHAT HAPPENED\n  requests accepted %d, of which the caller sent %d twice; senders that died after sending: %d\n\n", accepted, repeats, died)
	fmt.Printf("  %-9s %-15s %9s %9s %12s %9s %7s %9s\n", "channel", "class", "queued", "sent", "suppressed", "expired", "dead", "waiting")
	rows, err := pool.Query(ctx, `SELECT channel, class, count(*), count(*) FILTER (WHERE state = 'sent'),
	        count(*) FILTER (WHERE state = 'suppressed'), count(*) FILTER (WHERE state = 'expired'),
	        count(*) FILTER (WHERE state = 'dead'), count(*) FILTER (WHERE state = 'waiting')
	        FROM nt_deliveries GROUP BY 1, 2 ORDER BY 1, 2`)
	must(err)
	unaccounted := 0
	for rows.Next() {
		var channel, class string
		var n, sentN, suppressed, expired, dead, waiting int
		must(rows.Scan(&channel, &class, &n, &sentN, &suppressed, &expired, &dead, &waiting))
		fmt.Printf("  %-9s %-15s %9d %9d %12d %9d %7d %9d\n", channel, class, n, sentN, suppressed, expired, dead, waiting)
		unaccounted += waiting
	}

	// How long things took, from the request being accepted to the delivery being marked sent.
	delay := func(channel, class string) (median, p99 float64) {
		must(pool.QueryRow(ctx, `SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM d.sent_at - n.created_at)), 0),
		                                coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch FROM d.sent_at - n.created_at)), 0)
		                         FROM nt_deliveries d JOIN nt_notifications n ON n.id = d.notification_id
		                         WHERE d.state = 'sent' AND d.channel = $1 AND d.class = $2`, channel, class).Scan(&median, &p99))
		return median, p99
	}
	fmt.Printf("\n  %-34s %12s %18s\n", "from request to sent", "median", "99th percentile")
	for _, c := range [][2]string{{"sms", "code"}, {"email", "transactional"}, {"push", "transactional"}, {"email", "marketing"}} {
		median, p99 := delay(c[0], c[1])
		fmt.Printf("  %-34s %10.2f s %16.2f s\n", c[1]+", by "+c[0], median, p99)
	}

	// The checks. Each compares what the providers really sent with what should have been.
	var twiceRequested int
	must(pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM nt_notifications
	                         GROUP BY split_part(request_key, '-', 1), split_part(request_key, '-', 2), split_part(request_key, '-', 3)
	                         HAVING count(*) > 1) x`).Scan(&twiceRequested))
	more := func(p *provider) (n int) {
		for _, times := range p.keys {
			if times > 1 {
				n++
			}
		}
		return n
	}
	var afterOptOut, lateCodes int
	for _, s := range email.log {
		if at, ok := optedOut.Load(s.user); ok && s.class == "marketing" && s.at.After(at.(time.Time)) {
			afterOptOut++
		}
	}
	// A code is late if the provider sent it after it had expired.
	var created []time.Time
	rows, err = pool.Query(ctx, "SELECT user_id, created_at FROM nt_notifications WHERE class = 'code' ORDER BY created_at")
	must(err)
	byUser := map[int][]time.Time{}
	for rows.Next() {
		var user int
		var at time.Time
		must(rows.Scan(&user, &at))
		byUser[user] = append(byUser[user], at)
		created = append(created, at)
	}
	sort.Slice(sms.log, func(i, j int) bool { return sms.log[i].at.Before(sms.log[j].at) })
	for _, s := range sms.log {
		// The newest code requested for this user before the send is the one that was sent.
		var newest time.Time
		for _, at := range byUser[s.user] {
			if at.Before(s.at) {
				newest = at
			}
		}
		if s.at.Sub(newest) > codeLife {
			lateCodes++
		}
	}

	fmt.Println("\nCHECKS")
	for _, c := range []struct {
		name string
		n    int
	}{
		{"requests that became two notifications", twiceRequested},
		{"emails sent more than once", more(email)},
		{"push messages sent more than once", more(push)},
		{"text messages sent more than once", more(sms)},
		{"marketing emails sent after the user opted out", afterOptOut},
		{"login codes sent after they had expired", lateCodes},
		{"calls refused by a provider for exceeding its limit", email.refused + sms.refused + push.refused},
		{"deliveries still waiting, unaccounted for", unaccounted},
	} {
		fmt.Printf("  %-54s %6d\n", c.name, c.n)
	}
}
