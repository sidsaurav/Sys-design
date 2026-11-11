// Chapter 2.6 — the database is a shared resource: connection limits, the
// three timeouts, the lock queue, and a pool starved by slow queries.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/ch06-shared
//
// Use a scratch database: part 1 deliberately uses up every connection the
// server allows, for a moment. The whole run takes about 30 seconds.
//
// The connection string comes from DATABASE_URL and defaults to
// postgres://postgres:postgres@localhost:5432/postgres.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ctx = context.Background()

func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://postgres:postgres@localhost:5432/postgres"
}

// connect opens one connection and labels it, so that it can be recognised
// in pg_stat_activity.
func connect(label string) *pgx.Conn {
	conn, err := pgx.Connect(ctx, databaseURL())
	if err != nil {
		log.Fatalf("cannot connect to PostgreSQL (%v). See phase-2-data/README.md.", err)
	}
	mustExec(conn, "SELECT set_config('application_name', $1, false)", label)
	return conn
}

func mustExec(conn *pgx.Conn, sql string, args ...any) {
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

// describe formats a database error as "SQLSTATE: message".
func describe(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("%s %s: %s", pgErr.Severity, pgErr.Code, pgErr.Message)
	}
	return err.Error()
}

func main() {
	partConnections()
	partTimeouts()
	partLockQueue()
	partPoolStarvation()
}

// ------------------------------------------------------------ part 1

func partConnections() {
	fmt.Println("PART 1 — connections are a limited resource")
	admin := connect("admin")
	var limit, reserved string
	admin.QueryRow(ctx, "SHOW max_connections").Scan(&limit)
	admin.QueryRow(ctx, "SHOW superuser_reserved_connections").Scan(&reserved)
	fmt.Printf("  max_connections = %s, superuser_reserved_connections = %s\n", limit, reserved)

	// An application should connect as an ordinary role, not as a superuser.
	// Create one for the duration of this part.
	mustExec(admin, "DROP ROLE IF EXISTS sr_app")
	_, roleErr := admin.Exec(ctx, "CREATE ROLE sr_app LOGIN PASSWORD 'sr_app'")
	admin.Close(ctx)
	time.Sleep(200 * time.Millisecond) // let the server free this connection's slot

	var conns []*pgx.Conn
	if roleErr != nil {
		fmt.Printf("  (cannot create a test role, so skipping the ordinary-role half: %s)\n", describe(roleErr))
	} else {
		appConfig, err := pgx.ParseConfig(databaseURL())
		if err != nil {
			log.Fatal(err)
		}
		appConfig.User, appConfig.Password = "sr_app", "sr_app"
		for {
			conn, err := pgx.ConnectConfig(ctx, appConfig)
			if err != nil {
				fmt.Printf("  as an ordinary role, the application opened %d connections; the next one was refused:\n    %s\n",
					len(conns), describe(err))
				break
			}
			conns = append(conns, conn)
		}
	}

	// The slots that are left are reserved for superusers, so that an
	// administrator can still get in. Use them up too.
	before := len(conns)
	for {
		conn, err := pgx.Connect(ctx, databaseURL())
		if err != nil {
			fmt.Printf("  a superuser could still open %d more; then nobody can connect at all:\n    %s\n\n",
				len(conns)-before, describe(err))
			break
		}
		conns = append(conns, conn)
	}
	for _, c := range conns {
		c.Close(ctx)
	}
	time.Sleep(500 * time.Millisecond) // let the server finish closing them

	admin = connect("admin")
	mustExec(admin, "DROP ROLE IF EXISTS sr_app")
	admin.Close(ctx)
}

// ------------------------------------------------------------ part 2

func partTimeouts() {
	fmt.Println("PART 2 — three timeouts, each set to 500 ms")

	// statement_timeout: no statement may run longer than this.
	a := connect("timeouts")
	mustExec(a, "SET statement_timeout = '500ms'")
	start := time.Now()
	_, err := a.Exec(ctx, "SELECT pg_sleep(5)")
	fmt.Printf("  statement_timeout: a 5-second query was stopped after %v\n    %s\n",
		time.Since(start).Round(10*time.Millisecond), describe(err))
	a.Close(ctx)

	// lock_timeout: no statement may wait longer than this for a lock.
	admin := connect("admin")
	mustExec(admin, "DROP TABLE IF EXISTS sr_items")
	mustExec(admin, "CREATE TABLE sr_items (id int PRIMARY KEY, stock int NOT NULL)")
	mustExec(admin, "INSERT INTO sr_items SELECT g, 10 FROM generate_series(1, 1000) g")
	holder := connect("lock holder")
	mustExec(holder, "BEGIN")
	mustExec(holder, "UPDATE sr_items SET stock = 9 WHERE id = 1") // holds the row lock
	b := connect("timeouts")
	mustExec(b, "SET lock_timeout = '500ms'")
	start = time.Now()
	_, err = b.Exec(ctx, "UPDATE sr_items SET stock = 8 WHERE id = 1")
	fmt.Printf("  lock_timeout: an update waiting for a locked row gave up after %v\n    %s\n",
		time.Since(start).Round(10*time.Millisecond), describe(err))
	b.Close(ctx)
	mustExec(holder, "ROLLBACK")
	holder.Close(ctx)

	// idle_in_transaction_session_timeout: a session may not sit inside an
	// open transaction, doing nothing, for longer than this.
	c := connect("timeouts")
	mustExec(c, "SET idle_in_transaction_session_timeout = '500ms'")
	mustExec(c, "BEGIN")
	mustExec(c, "SELECT count(*) FROM sr_items")
	time.Sleep(time.Second) // the application "forgets" the transaction
	_, err = c.Exec(ctx, "SELECT 1")
	fmt.Printf("  idle_in_transaction_session_timeout: a transaction left open for 1 s was terminated by the server\n    %s\n\n",
		describe(err))
	c.Close(ctx)
	admin.Close(ctx)
}

// ------------------------------------------------------------ part 3

// showQueue prints who is waiting for whom among our labelled sessions.
func showQueue(admin *pgx.Conn) {
	rows, err := admin.Query(ctx, `
		SELECT application_name, state, coalesce(wait_event_type, '-'),
		       coalesce((SELECT string_agg(b.application_name, ', ')
		                 FROM pg_stat_activity b
		                 WHERE b.pid = ANY (pg_blocking_pids(a.pid))), '-'),
		       left(query, 44)
		FROM pg_stat_activity a
		WHERE application_name LIKE '_: %'
		ORDER BY application_name`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	fmt.Println("    session                     state                  waiting for   blocked by                   last statement")
	for rows.Next() {
		var name, state, wait, blockedBy, query string
		rows.Scan(&name, &state, &wait, &blockedBy, &query)
		fmt.Printf("    %-27s %-22s %-13s %-28s %s\n", name, state, wait, blockedBy, query)
	}
}

func partLockQueue() {
	fmt.Println("PART 3 — the lock queue: one forgotten transaction, one schema change, and a table nobody can read")
	admin := connect("admin")
	defer admin.Close(ctx)

	for _, useTimeout := range []bool{false, true} {
		a := connect("A: forgotten transaction")
		b := connect("B: schema change")
		c := connect("C: ordinary query")

		mustExec(a, "BEGIN")
		mustExec(a, "SELECT count(*) FROM sr_items") // ...and then nothing: the transaction stays open

		if useTimeout {
			fmt.Println("\n  The same again, but the schema change first runs: SET lock_timeout = '500ms'")
			mustExec(b, "SET lock_timeout = '500ms'")
		}

		bDone := make(chan error, 1)
		go func() {
			_, err := b.Exec(ctx, "ALTER TABLE sr_items ADD COLUMN IF NOT EXISTS note text")
			bDone <- err
		}()
		time.Sleep(200 * time.Millisecond)

		cStart := time.Now()
		cDone := make(chan error, 1)
		go func() {
			var n int
			cDone <- c.QueryRow(ctx, "SELECT count(*) FROM sr_items").Scan(&n)
		}()
		time.Sleep(200 * time.Millisecond)

		if !useTimeout {
			showQueue(admin)
			fmt.Println("  C is a plain SELECT. It is stuck behind B, which is stuck behind A.")
			time.Sleep(time.Second)
			mustExec(a, "COMMIT") // somebody finally notices
			<-bDone
			<-cDone
			fmt.Printf("  A commits at last. C's simple SELECT took %v.\n", time.Since(cStart).Round(100*time.Millisecond))
		} else {
			err := <-bDone
			<-cDone
			fmt.Printf("  B gave up: %s\n", describe(err))
			fmt.Printf("  C's SELECT took %v. The schema change can be tried again later.\n", time.Since(cStart).Round(100*time.Millisecond))
			mustExec(a, "COMMIT")
		}
		a.Close(ctx)
		b.Close(ctx)
		c.Close(ctx)
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 4

func newPool(maxConns int32) *pgxpool.Pool {
	cfg, err := pgxpool.ParseConfig(databaseURL())
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	return pool
}

// runReport runs one slow report (a query that takes a second). If limit
// is set, the report is given that statement_timeout and no more.
func runReport(pool *pgxpool.Pool, limit string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if limit != "" {
		// SET LOCAL lasts until the end of this transaction only.
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+limit+"'"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "SELECT pg_sleep(1)"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// measure keeps ten report loops running and, at the same time, sends 50
// fast "checkout" queries per second for four seconds. It returns how long
// the checkouts took, and how many reports finished or were cut off.
func measure(checkoutPool, reportPool *pgxpool.Pool, reportLimit string) (p50, p99 time.Duration, finished, cutOff int) {
	stop := make(chan struct{})
	var reportWG sync.WaitGroup
	var mu sync.Mutex
	for range 10 {
		reportWG.Add(1)
		go func() {
			defer reportWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := runReport(reportPool, reportLimit)
				mu.Lock()
				if err != nil {
					cutOff++
				} else {
					finished++
				}
				mu.Unlock()
			}
		}()
	}
	time.Sleep(300 * time.Millisecond) // let the reports get going

	var wg sync.WaitGroup
	var latencies []time.Duration
	const rate, seconds = 50, 4
	begin := time.Now()
	for i := 0; i < rate*seconds; i++ {
		scheduled := begin.Add(time.Duration(i) * time.Second / rate)
		time.Sleep(time.Until(scheduled))
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stock int
			checkoutPool.QueryRow(ctx, "SELECT stock FROM sr_items WHERE id = 7").Scan(&stock)
			took := time.Since(scheduled)
			mu.Lock()
			latencies = append(latencies, took)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(stop)
	reportWG.Wait()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		return latencies[max(int(math.Ceil(p/100*float64(len(latencies)))), 1)-1]
	}
	return pct(50), pct(99), finished, cutOff
}

func partPoolStarvation() {
	fmt.Println("PART 4 — ten slow reports and a fast checkout, in one application")
	fmt.Println("  Each report query takes 1 s. The checkout query takes well under 1 ms. 50 checkouts per second.")
	fmt.Println()
	fmt.Println("  setup                                              checkout p50   checkout p99   reports finished / cut off")
	row := func(name string, p50, p99 time.Duration, finished, cutOff int) {
		fmt.Printf("  %-50s %9s %14s %14d / %d\n", name, ms(p50), ms(p99), finished, cutOff)
	}

	// 1. Everything shares one pool of ten connections.
	shared := newPool(10)
	p50, p99, finished, cutOff := measure(shared, shared, "")
	row("one pool of 10 for everything", p50, p99, finished, cutOff)

	// 2. The same pool, but a report may run for at most 100 ms.
	p50, p99, finished, cutOff = measure(shared, shared, "100ms")
	row("one pool of 10; reports limited to 100 ms each", p50, p99, finished, cutOff)
	shared.Close()

	// 3. Reports get their own small pool and cannot touch checkout's.
	checkout, reports := newPool(8), newPool(2)
	p50, p99, finished, cutOff = measure(checkout, reports, "")
	row("checkout pool of 8, separate report pool of 2", p50, p99, finished, cutOff)
	checkout.Close()
	reports.Close()

	admin := connect("admin")
	mustExec(admin, "DROP TABLE IF EXISTS sr_items")
	admin.Close(ctx)
}

func ms(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}
