// Chapter 4.3 — every message may arrive twice. Four ways to cope, and one way to choose the ID.
//
// The program plays the part of a queue that delivers at least once: of
// every five messages, one is delivered twice — sometimes later, sometimes
// to two workers at the same moment — and now and then a worker dies half-way
// through a message, which is then delivered again. The consumers, and the
// database they write to, are real.
//
//	part 1  1,000 messages, each "add 10 points to an account": five consumers
//	part 2  which ID? the producer sends some messages twice
//	part 3  an effect outside the database: sending an email
//
// Needs the database from Phase 2. The connection string comes from
// DATABASE_URL and defaults to postgres://postgres:postgres@localhost:5432/postgres.
//
//	go run ./phase-4-async/code/ch03-idempotent
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ctx  = context.Background()
	pool *pgxpool.Pool
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

const schema = `
DROP TABLE IF EXISTS idem_accounts, idem_processed, idem_orders;
CREATE TABLE idem_accounts  (id int PRIMARY KEY, points int NOT NULL DEFAULT 0);
CREATE TABLE idem_processed (message_id text PRIMARY KEY, at timestamptz NOT NULL DEFAULT now());
CREATE TABLE idem_orders    (id int PRIMARY KEY, account int NOT NULL, points_awarded boolean NOT NULL DEFAULT false);
INSERT INTO idem_accounts SELECT g FROM generate_series(1, 50) g;
INSERT INTO idem_orders   SELECT g, 1 + g % 50 FROM generate_series(1, 1000) g;`

// A message says: order N has been paid; its buyer is owed 10 points.
type message struct {
	id      string // the ID the consumer is given for this delivery
	order   int
	account int
}

var errDied = errors.New("the worker died") // half-way through: nothing more is done, and nothing is acknowledged

// deliver hands every message to the consumer at least once, eight at a time.
// One message in ten is also handed, at the same moment, to a second worker.
// One in ten is delivered again later. One worker in a hundred dies half-way,
// and its message — never acknowledged — is delivered again.
func deliver(messages []message, consume func(m message, dies bool) error) (deliveries int) {
	rng := rand.New(rand.NewSource(3))
	var count atomic.Int64
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	var hand func(m message, dies bool)
	hand = func(m message, dies bool) {
		defer wg.Done()
		slots <- struct{}{}
		count.Add(1)
		err := consume(m, dies)
		<-slots
		if errors.Is(err, errDied) {
			wg.Add(1)
			go func() { time.Sleep(20 * time.Millisecond); hand(m, false) }()
			return
		}
		must(err)
	}
	var later []message
	for _, m := range messages {
		wg.Add(1)
		go hand(m, rng.Intn(100) == 0)
		switch rng.Intn(10) {
		case 0:
			wg.Add(1)
			go hand(m, false)
		case 1:
			later = append(later, m)
		}
	}
	wg.Wait()
	for _, m := range later {
		wg.Add(1)
		go hand(m, false)
	}
	wg.Wait()
	return int(count.Load())
}

func reset() {
	_, err := pool.Exec(ctx, schema)
	must(err)
}

func outcome() (total, wrong int) {
	must(pool.QueryRow(ctx, "SELECT sum(points), count(*) FILTER (WHERE points <> 200) FROM idem_accounts").Scan(&total, &wrong))
	return total, wrong
}

func orders(n int) []message {
	var list []message
	for i := 1; i <= n; i++ {
		list = append(list, message{id: fmt.Sprint("order-", i, "-points"), order: i, account: 1 + i%50})
	}
	return list
}

const addPoints = "UPDATE idem_accounts SET points = points + 10 WHERE id = $1"

// ------------------------------------------------------------ five consumers

// 1. Do the work for every delivery.
func everyTime(m message, dies bool) error {
	if dies {
		return errDied
	}
	_, err := pool.Exec(ctx, addPoints, m.account)
	return err
}

// 2. Look for the ID; if it is not there, do the work, then record the ID.
func lookThenDo(m message, dies bool) error {
	var seen bool
	must(pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM idem_processed WHERE message_id = $1)", m.id).Scan(&seen))
	if seen {
		return nil
	}
	if _, err := pool.Exec(ctx, addPoints, m.account); err != nil {
		return err
	}
	if dies {
		return errDied // the points were added; the ID was never recorded
	}
	_, err := pool.Exec(ctx, "INSERT INTO idem_processed (message_id) VALUES ($1) ON CONFLICT DO NOTHING", m.id)
	return err
}

// 3. Record the ID first; if it was new, do the work. Two separate statements.
func recordThenDo(m message, dies bool) error {
	tag, err := pool.Exec(ctx, "INSERT INTO idem_processed (message_id) VALUES ($1) ON CONFLICT DO NOTHING", m.id)
	if err != nil || tag.RowsAffected() == 0 {
		return err // seen before
	}
	if dies {
		return errDied // the ID is recorded; the points were never added
	}
	_, err = pool.Exec(ctx, addPoints, m.account)
	return err
}

// 4. Record the ID and do the work in ONE transaction: both happen, or neither.
func oneTransaction(m message, dies bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // does nothing once the transaction is committed
	tag, err := tx.Exec(ctx, "INSERT INTO idem_processed (message_id) VALUES ($1) ON CONFLICT DO NOTHING", m.id)
	if err != nil || tag.RowsAffected() == 0 {
		return err // seen before — or a second copy waited for the first to commit, and then found it
	}
	if dies {
		return errDied // the transaction is rolled back: the ID is forgotten too
	}
	if _, err := tx.Exec(ctx, addPoints, m.account); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// 5. No table of IDs: the order itself records that its points have been awarded.
func stateInTheRow(m message, dies bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, "UPDATE idem_orders SET points_awarded = true WHERE id = $1 AND NOT points_awarded", m.order)
	if err != nil || tag.RowsAffected() == 0 {
		return err // already awarded
	}
	if dies {
		return errDied
	}
	if _, err := tx.Exec(ctx, addPoints, m.account); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — 1,000 orders, each worth 10 points to one of 50 accounts. Every account should end with 200,")
	fmt.Println("         and the total should be 10,000.")
	fmt.Println()
	fmt.Printf("  %-58s %12s %14s %18s\n", "the consumer...", "deliveries", "total points", "accounts wrong")
	for _, c := range []struct {
		name    string
		consume func(message, bool) error
	}{
		{"adds the points, every time", everyTime},
		{"looks for the ID; if new: adds the points, records the ID", lookThenDo},
		{"records the ID; if new: adds the points", recordThenDo},
		{"records the ID and adds the points, in one transaction", oneTransaction},
		{"marks the order as awarded and adds, in one transaction", stateInTheRow},
	} {
		reset()
		n := deliver(orders(1000), c.consume)
		total, wrong := outcome()
		fmt.Printf("  %-58s %12d %14d %12d of 50\n", c.name, n, total, wrong)
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — the producer is unsure whether 50 of its 1,000 sends arrived, and sends those again.")
	fmt.Println("         They had arrived. The broker gives every send an ID of its own.")
	fmt.Println()
	fmt.Printf("  %-58s %12s %14s\n", "duplicates are recognised by...", "messages", "total points")
	for _, byBroker := range []bool{true, false} {
		reset()
		var sent []message
		serial := 0
		send := func(m message) {
			serial++
			if byBroker {
				m.id = fmt.Sprint("broker-message-", serial) // a new ID for every send
			}
			sent = append(sent, m)
		}
		for i, m := range orders(1000) {
			send(m)
			if i%20 == 0 {
				send(m) // the same order, sent again
			}
		}
		deliver(sent, oneTransaction)
		total, _ := outcome()
		name := "an ID the producer chose: one per order"
		if byBroker {
			name = "the ID the broker gave each message"
		}
		fmt.Printf("  %-58s %12d %14d\n", name, len(sent), total)
	}
}

// ------------------------------------------------------------ part 3

// provider is somebody else's email service. If it is given a key, it
// remembers it, and does not send the same email twice.
type provider struct {
	mu   sync.Mutex
	keys map[string]bool
	sent int
}

func (p *provider) send(key string) {
	time.Sleep(time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	if key != "" {
		if p.keys[key] {
			return
		}
		p.keys[key] = true
	}
	p.sent++
}

func part3() {
	fmt.Println("PART 3 — 1,000 orders, each owed one confirmation email. The email is sent by an outside provider,")
	fmt.Println("         so sending it cannot be part of a database transaction.")
	fmt.Println()
	fmt.Printf("  %-58s %14s %16s\n", "the consumer...", "emails sent", "sent twice")
	for _, v := range []struct {
		name string
		look bool
		key  bool
	}{
		{"sends, every time", false, false},
		{"looks for the ID; if new: sends, then records the ID", true, false},
		{"sends every time, with the message's ID as a key", false, true},
	} {
		reset()
		p := &provider{keys: map[string]bool{}}
		deliver(orders(1000), func(m message, dies bool) error {
			if v.look {
				var seen bool
				must(pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM idem_processed WHERE message_id = $1)", m.id).Scan(&seen))
				if seen {
					return nil
				}
			}
			key := ""
			if v.key {
				key = m.id
			}
			p.send(key)
			if dies {
				return errDied // sent, and not recorded
			}
			if v.look {
				_, err := pool.Exec(ctx, "INSERT INTO idem_processed (message_id) VALUES ($1) ON CONFLICT DO NOTHING", m.id)
				return err
			}
			return nil
		})
		fmt.Printf("  %-58s %14d %16d\n", v.name, p.sent, p.sent-1000)
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 3)")
	flag.Parse()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	var err error
	pool, err = pgxpool.New(ctx, url+"?pool_max_conns=16")
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the database of Phase 2 first: see phase-2-data/README.md", url, err)
	}
	defer pool.Close()
	for i, run := range []func(){part1, part2, part3} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
	_, err = pool.Exec(ctx, "DROP TABLE IF EXISTS idem_accounts, idem_processed, idem_orders")
	must(err)
}
