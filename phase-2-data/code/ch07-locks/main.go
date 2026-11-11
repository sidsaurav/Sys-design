// Chapter 2.7 — what a schema change blocks, and for how long.
//
// Schema changes are applied to a table of three million rows while one
// connection keeps reading the table and another keeps writing to it. For
// each change the program prints how long the change took, and the longest
// time any single read or write had to wait while it ran.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/ch07-locks
//
// The last part shows a different kind of breakage: a change that blocks
// nothing, and still makes a running query fail.
//
// It takes about half a minute. The connection string comes from
// DATABASE_URL and defaults to
// postgres://postgres:postgres@localhost:5432/postgres.
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const rows = 3_000_000

var bg = context.Background()

func connect() *pgx.Conn {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	conn, err := pgx.Connect(bg, url)
	if err != nil {
		log.Fatalf("cannot connect to PostgreSQL (%v). See phase-2-data/README.md.", err)
	}
	return conn
}

func mustExec(conn *pgx.Conn, sql string) {
	if _, err := conn.Exec(bg, sql); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

// A probe repeats one small query and remembers the longest time a single
// execution took. That is how long the table was unavailable to it.
type probe struct {
	mu      sync.Mutex
	longest time.Duration
	failed  int
}

func (p *probe) run(ctx context.Context, wg *sync.WaitGroup, seed int64, query func(*pgx.Conn, int) error) {
	defer wg.Done()
	conn := connect()
	defer conn.Close(bg)
	rng := rand.New(rand.NewSource(seed))
	for ctx.Err() == nil {
		start := time.Now()
		err := query(conn, 1+rng.Intn(rows))
		took := time.Since(start)
		p.mu.Lock()
		p.longest = max(p.longest, took)
		if err != nil {
			p.failed++
		}
		p.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
}

func readOne(conn *pgx.Conn, id int) error {
	var customer int
	return conn.QueryRow(bg, "SELECT customer_id FROM sc_orders WHERE id = $1", id).Scan(&customer)
}

func writeOne(conn *pgx.Conn, id int) error {
	_, err := conn.Exec(bg, "UPDATE sc_orders SET note = 'seen' WHERE id = $1", id)
	return err
}

// A change is one way of making a schema change: the statements to run,
// and anything that must be undone first so that it can be tried.
type change struct {
	name    string
	prepare []string // not measured
	run     []string // measured, with the probes running
}

func measure(admin *pgx.Conn, c change) {
	for _, sql := range c.prepare {
		mustExec(admin, sql)
	}
	ctx, stop := context.WithCancel(bg)
	var wg sync.WaitGroup
	reads, writes := &probe{}, &probe{}
	wg.Add(2)
	go reads.run(ctx, &wg, 1, readOne)
	go writes.run(ctx, &wg, 2, writeOne)
	time.Sleep(200 * time.Millisecond) // let the probes get going

	start := time.Now()
	for _, sql := range c.run {
		mustExec(admin, sql)
	}
	took := time.Since(start)

	time.Sleep(100 * time.Millisecond)
	stop()
	wg.Wait()
	fmt.Printf("  %-58s %8s %12s %12s\n", c.name, short(took), short(reads.longest), short(writes.longest))
	if reads.failed+writes.failed > 0 {
		fmt.Printf("    (%d reads and %d writes failed)\n", reads.failed, writes.failed)
	}
}

func short(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d >= 10*time.Millisecond:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	}
}

func main() {
	admin := connect()
	defer admin.Close(bg)

	fmt.Printf("Building a table of %d orders and a table of 100,000 customers...\n", rows)
	for _, sql := range []string{
		"DROP TABLE IF EXISTS sc_orders, sc_customers",
		"CREATE TABLE sc_customers (id int PRIMARY KEY, name text NOT NULL)",
		"INSERT INTO sc_customers SELECT g, 'customer ' || g FROM generate_series(1, 100000) g",
		`CREATE TABLE sc_orders (
			id          bigint PRIMARY KEY,
			customer_id int,
			amount      int NOT NULL,
			note        text,
			created_at  timestamptz NOT NULL)`,
		fmt.Sprintf(`INSERT INTO sc_orders
			SELECT g, 1 + g %% 100000, 100 + g %% 9000, 'order note', timestamptz '2024-01-01' + g * interval '1 second'
			FROM generate_series(1, %d) g`, rows),
		"VACUUM ANALYZE sc_orders",
	} {
		mustExec(admin, sql)
	}

	fmt.Println()
	fmt.Println("  While each change runs, one connection reads the table and another writes to it.")
	fmt.Println()
	fmt.Printf("  %-58s %8s %12s %12s\n", "schema change", "took", "longest", "longest")
	fmt.Printf("  %-58s %8s %12s %12s\n", "", "", "read wait", "write wait")

	section := func(title string) { fmt.Printf("\n  %s\n", title) }

	section("Adding a column")
	measure(admin, change{
		name: "ADD COLUMN c1 text",
		run:  []string{"ALTER TABLE sc_orders ADD COLUMN c1 text"},
	})
	measure(admin, change{
		name: "ADD COLUMN c2 text NOT NULL DEFAULT 'new'",
		run:  []string{"ALTER TABLE sc_orders ADD COLUMN c2 text NOT NULL DEFAULT 'new'"},
	})
	measure(admin, change{
		name: "ADD COLUMN c3 timestamptz DEFAULT clock_timestamp()",
		run:  []string{"ALTER TABLE sc_orders ADD COLUMN c3 timestamptz DEFAULT clock_timestamp()"},
	})

	section("Changing a column's type")
	measure(admin, change{
		name: "ALTER COLUMN amount TYPE bigint",
		run:  []string{"ALTER TABLE sc_orders ALTER COLUMN amount TYPE bigint"},
	})

	section("Adding an index")
	measure(admin, change{
		name: "CREATE INDEX",
		run:  []string{"CREATE INDEX sc_orders_customer ON sc_orders (customer_id)"},
	})
	measure(admin, change{
		name:    "CREATE INDEX CONCURRENTLY",
		prepare: []string{"DROP INDEX sc_orders_customer"},
		run:     []string{"CREATE INDEX CONCURRENTLY sc_orders_customer ON sc_orders (customer_id)"},
	})

	section("Adding a CHECK constraint")
	measure(admin, change{
		name: "ADD CONSTRAINT ... CHECK (amount >= 0)",
		run:  []string{"ALTER TABLE sc_orders ADD CONSTRAINT amount_positive CHECK (amount >= 0)"},
	})
	measure(admin, change{
		name:    "ADD CONSTRAINT ... NOT VALID, then VALIDATE CONSTRAINT",
		prepare: []string{"ALTER TABLE sc_orders DROP CONSTRAINT amount_positive"},
		run: []string{
			"ALTER TABLE sc_orders ADD CONSTRAINT amount_positive CHECK (amount >= 0) NOT VALID",
			"ALTER TABLE sc_orders VALIDATE CONSTRAINT amount_positive",
		},
	})

	section("Making a column NOT NULL")
	measure(admin, change{
		name: "ALTER COLUMN customer_id SET NOT NULL",
		run:  []string{"ALTER TABLE sc_orders ALTER COLUMN customer_id SET NOT NULL"},
	})
	measure(admin, change{
		name:    "CHECK (... IS NOT NULL) NOT VALID, VALIDATE, SET NOT NULL",
		prepare: []string{"ALTER TABLE sc_orders ALTER COLUMN customer_id DROP NOT NULL"},
		run: []string{
			"ALTER TABLE sc_orders ADD CONSTRAINT customer_present CHECK (customer_id IS NOT NULL) NOT VALID",
			"ALTER TABLE sc_orders VALIDATE CONSTRAINT customer_present",
			"ALTER TABLE sc_orders ALTER COLUMN customer_id SET NOT NULL", // no scan: the constraint proves it
			"ALTER TABLE sc_orders DROP CONSTRAINT customer_present",
		},
	})

	section("Adding a foreign key")
	measure(admin, change{
		name: "ADD FOREIGN KEY (customer_id) REFERENCES sc_customers",
		run:  []string{"ALTER TABLE sc_orders ADD CONSTRAINT customer_fk FOREIGN KEY (customer_id) REFERENCES sc_customers"},
	})
	measure(admin, change{
		name:    "ADD FOREIGN KEY ... NOT VALID, then VALIDATE CONSTRAINT",
		prepare: []string{"ALTER TABLE sc_orders DROP CONSTRAINT customer_fk"},
		run: []string{
			"ALTER TABLE sc_orders ADD CONSTRAINT customer_fk FOREIGN KEY (customer_id) REFERENCES sc_customers NOT VALID",
			"ALTER TABLE sc_orders VALIDATE CONSTRAINT customer_fk",
		},
	})

	staleStatement(admin)

	mustExec(admin, "DROP TABLE sc_orders, sc_customers")
}

// staleStatement shows that a change which blocks nothing can still break
// running code. Like most drivers, pgx prepares a query the first time a
// connection runs it and reuses the prepared statement afterwards. The
// server refuses to reuse it once the query's result columns have changed.
func staleStatement(admin *pgx.Conn) {
	fmt.Println()
	fmt.Println("  A change that blocks nothing can still break a running query")
	app := connect()
	defer app.Close(bg)
	const query = "SELECT * FROM sc_orders WHERE id = $1"
	selectStar := func() string {
		rows, err := app.Query(bg, query, 1)
		if err != nil {
			return err.Error()
		}
		defer rows.Close()
		for rows.Next() {
		}
		if rows.Err() != nil {
			return rows.Err().Error()
		}
		return fmt.Sprintf("ok, %d columns", len(rows.FieldDescriptions()))
	}
	fmt.Println("    an application connection runs:   " + query)
	fmt.Println("    before the change:                " + selectStar())
	mustExec(admin, "ALTER TABLE sc_orders ADD COLUMN c4 text")
	fmt.Println("    ALTER TABLE sc_orders ADD COLUMN c4 text")
	fmt.Println("    the same query, first time after: " + selectStar())
	fmt.Println("    the same query, second time:      " + selectStar())
}
