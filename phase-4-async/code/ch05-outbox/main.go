// Chapter 4.5 — saving an order and announcing it are two writes.
//
// An order is saved in PostgreSQL, and an "order placed" event is published
// to a broker. The database is real. The broker is a stand-in inside this
// program which, like a real one, is sometimes unreachable. Now and then the
// process dies between one step and the next.
//
//	part 1  1,000 orders: three ways of doing the two writes
//	part 2  no second write at all: reading the changes from the database's own log
//
// Needs the database from Phase 2 (DATABASE_URL, as before). Part 2 needs
// one setting, wal_level = logical; the program says how if it is missing.
//
//	go run ./phase-4-async/code/ch05-outbox
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
	"strings"
	"sync"
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
DROP TABLE IF EXISTS dw_orders, dw_outbox;
CREATE TABLE dw_orders (id int PRIMARY KEY, total_cents int NOT NULL CHECK (total_cents > 0));
CREATE TABLE dw_outbox (
    id           bigserial PRIMARY KEY,
    event        text NOT NULL,         -- what happened, for whoever reads it
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_at timestamptz            -- null until the relay has delivered it
);`

// ------------------------------------------------------------ the broker (a stand-in)

var errUnreachable = errors.New("the broker did not answer")

type broker struct {
	mu     sync.Mutex
	rng    *rand.Rand
	events map[string]int // event → how many times it was published
}

func newBroker() *broker { return &broker{rng: rand.New(rand.NewSource(5)), events: map[string]int{}} }

func (b *broker) publish(event string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rng.Intn(50) == 0 { // one call in fifty finds it unreachable
		return errUnreachable
	}
	b.events[event]++
	return nil
}

// ------------------------------------------------------------ three ways to do two writes

// An order in this run. One in fifty cannot be saved (its total is not
// allowed); and during one in a hundred, the process dies half-way.
type order struct {
	id    int
	total int
	dies  bool
}

func event(o order) string { return fmt.Sprint("order-placed:", o.id) }

func save(o order) error {
	_, err := pool.Exec(ctx, "INSERT INTO dw_orders (id, total_cents) VALUES ($1, $2)", o.id, o.total)
	return err
}

// 1. Save the order, then publish the event.
func saveThenPublish(o order, b *broker) {
	if save(o) != nil {
		return // the customer is told that it failed
	}
	if o.dies {
		return
	}
	b.publish(event(o)) // if this fails, the order is already saved. There is nothing to undo
}

// 2. Publish the event, then save the order.
func publishThenSave(o order, b *broker) {
	if b.publish(event(o)) != nil {
		return // the customer is told that it failed
	}
	if o.dies {
		return
	}
	save(o) // if this fails, the event has already gone. It cannot be taken back
}

// 3. Save the order and a note of the event, in one transaction. The broker
// is not involved at all. A separate relay publishes the notes.
func saveWithOutbox(o order, _ *broker) {
	tx, err := pool.Begin(ctx)
	must(err)
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "INSERT INTO dw_orders (id, total_cents) VALUES ($1, $2)", o.id, o.total); err != nil {
		return
	}
	if o.dies {
		return // nothing was committed: no order, and no note
	}
	if _, err := tx.Exec(ctx, "INSERT INTO dw_outbox (event) VALUES ($1)", event(o)); err != nil {
		return
	}
	must(tx.Commit(ctx))
}

// relay publishes the notes that have not been published, oldest first,
// and marks each one once the broker has accepted it. It is the only thing
// that talks to the broker, and it can try as often as it needs to.
func relay(b *broker, stop <-chan struct{}) (delays []time.Duration) {
	rng := rand.New(rand.NewSource(6))
	stopping := false
	for {
		rows, err := pool.Query(ctx, "SELECT id, event, created_at FROM dw_outbox WHERE published_at IS NULL ORDER BY id LIMIT 50")
		must(err)
		type note struct {
			id      int64
			event   string
			created time.Time
		}
		var notes []note
		for rows.Next() {
			var n note
			must(rows.Scan(&n.id, &n.event, &n.created))
			notes = append(notes, n)
		}
		rows.Close()
		for _, n := range notes {
			if b.publish(n.event) != nil {
				break // unreachable: leave it, and everything after it, for the next round
			}
			delays = append(delays, time.Since(n.created))
			if rng.Intn(100) == 0 {
				break // the relay dies here: published, and not yet marked. Its replacement starts the round again
			}
			_, err := pool.Exec(ctx, "UPDATE dw_outbox SET published_at = now() WHERE id = $1", n.id)
			must(err)
		}
		if len(notes) == 0 {
			if stopping {
				return delays // asked to stop, and nothing is left to send
			}
			select {
			case <-stop:
				stopping = true // look once more before stopping
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

func part1() {
	fmt.Println("PART 1 — 1,000 orders. One in fifty cannot be saved. One call in fifty finds the broker unreachable.")
	fmt.Println("         During one order in a hundred, the process dies between one step and the next.")
	fmt.Println()
	fmt.Printf("  %-44s %8s %8s %18s %18s %16s\n", "", "orders", "events", "orders with", "events with", "events")
	fmt.Printf("  %-44s %8s %8s %18s %18s %16s\n", "", "saved", "sent", "no event", "no order", "sent twice")

	for _, v := range []struct {
		name  string
		place func(order, *broker)
		relay bool
	}{
		{"save the order, then publish", saveThenPublish, false},
		{"publish, then save the order", publishThenSave, false},
		{"save order and note together; a relay sends", saveWithOutbox, true},
	} {
		_, err := pool.Exec(ctx, schema)
		must(err)
		b := newBroker()
		stop := make(chan struct{})
		relayed := make(chan []time.Duration, 1)
		if v.relay {
			go func() { relayed <- relay(b, stop) }()
		}
		rng := rand.New(rand.NewSource(7))
		var wg sync.WaitGroup
		slots := make(chan struct{}, 8)
		for id := 1; id <= 1000; id++ {
			o := order{id: id, total: 1000 + id, dies: rng.Intn(100) == 0}
			if rng.Intn(50) == 0 {
				o.total = 0 // the database will refuse it
			}
			wg.Add(1)
			slots <- struct{}{}
			go func() { defer wg.Done(); v.place(o, b); <-slots }()
			time.Sleep(2 * time.Millisecond) // about 500 orders a second
		}
		wg.Wait()
		var delays []time.Duration
		if v.relay {
			close(stop)
			delays = <-relayed
		}

		saved := map[string]bool{}
		rows, err := pool.Query(ctx, "SELECT id FROM dw_orders")
		must(err)
		for rows.Next() {
			var id int
			must(rows.Scan(&id))
			saved[fmt.Sprint("order-placed:", id)] = true
		}
		var noEvent, noOrder, twice int
		for e := range saved {
			if b.events[e] == 0 {
				noEvent++
			}
		}
		for e, n := range b.events {
			if !saved[e] {
				noOrder++
			}
			if n > 1 {
				twice++
			}
		}
		fmt.Printf("  %-44s %8d %8d %18d %18d %16d\n", v.name, len(saved), len(b.events), noEvent, noOrder, twice)
		if v.relay {
			sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
			fmt.Printf("\n  The relay looks for new notes every 20 ms. From saving an order to publishing its event took\n")
			fmt.Printf("  %d ms (median) and %d ms (longest).\n", delays[len(delays)/2].Milliseconds(), delays[len(delays)-1].Milliseconds())
		}
	}
}

// ------------------------------------------------------------ part 2: the database's own log

func part2() {
	fmt.Println("PART 2 — no note, and no second write: the changes are read from the database's own log.")
	fmt.Println()
	var level string
	must(pool.QueryRow(ctx, "SHOW wal_level").Scan(&level))
	if level != "logical" {
		fmt.Println("  This part needs the database to keep enough in its log to reconstruct each change. Run")
		fmt.Println("      ALTER SYSTEM SET wal_level = 'logical';")
		fmt.Println("  as a superuser, restart PostgreSQL, and run this again. (With Docker: add  -c wal_level=logical")
		fmt.Println("  to the end of the docker run command.)")
		return
	}
	_, err := pool.Exec(ctx, schema)
	must(err)
	pool.Exec(ctx, "SELECT pg_drop_replication_slot('course_orders')") // left over from an interrupted run, perhaps
	// A slot is a reader's place in the log. From now on the database keeps
	// every change until this reader has taken it.
	_, err = pool.Exec(ctx, "SELECT pg_create_logical_replication_slot('course_orders', 'test_decoding')")
	must(err)
	defer pool.Exec(ctx, "SELECT pg_drop_replication_slot('course_orders')")

	// The application only saves orders. It knows nothing about events.
	rng := rand.New(rand.NewSource(7))
	committed := 0
	for id := 1; id <= 1000; id++ {
		tx, err := pool.Begin(ctx)
		must(err)
		_, err = tx.Exec(ctx, "INSERT INTO dw_orders (id, total_cents) VALUES ($1, $2)", id, 1000+id)
		must(err)
		if rng.Intn(50) == 0 {
			tx.Rollback(ctx) // the customer changed their mind
			continue
		}
		must(tx.Commit(ctx))
		committed++
	}
	_, err = pool.Exec(ctx, "DELETE FROM dw_orders WHERE id = 7")
	must(err)

	rows, err := pool.Query(ctx, "SELECT data FROM pg_logical_slot_get_changes('course_orders', NULL, NULL)")
	must(err)
	var inserts, deletes int
	var sample []string
	for rows.Next() {
		var line string
		must(rows.Scan(&line))
		switch {
		case strings.Contains(line, "dw_orders: INSERT"):
			inserts++
			if inserts <= 2 {
				sample = append(sample, line)
			}
		case strings.Contains(line, "dw_orders: DELETE"):
			deletes++
			sample = append(sample, line)
		}
	}
	fmt.Printf("  1,000 orders were inserted; %d of those transactions were committed, and one order was then deleted.\n", committed)
	fmt.Printf("  Changes to the orders table found in the log: %d inserts, %d delete.\n\n", inserts, deletes)
	for _, line := range sample {
		fmt.Println("    " + line)
	}

	// A reader that goes away: the database keeps the log for it.
	_, err = pool.Exec(ctx, "INSERT INTO dw_orders SELECT g, 5000 FROM generate_series(2001, 202000) g")
	must(err)
	var kept string
	must(pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn))
	                         FROM pg_replication_slots WHERE slot_name = 'course_orders'`).Scan(&kept))
	fmt.Printf("\n  The reader now stops, and 200,000 more orders are saved. Log that the database must keep until\n")
	fmt.Printf("  the reader returns: %s — and growing with every write.\n", kept)
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 or 2)")
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
	for i, run := range []func(){part1, part2} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
	_, err = pool.Exec(ctx, "DROP TABLE IF EXISTS dw_orders, dw_outbox")
	must(err)
}
