// Chapter 2.7 — renaming a column while the application is running.
//
// Eight "instances" of an application read and write a users table the
// whole time. The column `name` has to become `full_name`. The program
// makes the change three ways and counts the requests that fail:
//
//  1. rename the column, then deploy the new code;
//  2. deploy the new code, then rename the column;
//  3. expand, migrate, contract: add the new column, write both, backfill,
//     switch reads, stop writing the old column, drop it.
//
// A deploy here is a rolling deploy: instances are replaced one at a time,
// so for a while old and new code run side by side.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/ch07-expand-contract
//	go run ./phase-2-data/code/ch07-expand-contract -backfill-only-empty   # a common mistake
//
// The connection string comes from DATABASE_URL and defaults to
// postgres://postgres:postgres@localhost:5432/postgres.
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	users     = 200_000
	hotUsers  = 500 // a few busy users get all the updates
	instances = 8
)

var bg = context.Background()

var onlyEmpty = flag.Bool("backfill-only-empty", false,
	"backfill only the rows where full_name is empty, instead of every row where it differs from name")

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

func mustExec(conn *pgx.Conn, sql string, args ...any) {
	if _, err := conn.Exec(bg, sql, args...); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

// ------------------------------------------------- the application

// A version of the application is, for our purposes, the SQL it runs.
type version struct {
	insert string // $1 = name, $2 = email
	update string // $1 = new name, $2 = id
	read   string // $1 = id; returns the name
}

var (
	// v1: the code before the change. It knows only `name`.
	v1 = &version{
		insert: "INSERT INTO sc_users (name, email) VALUES ($1, $2)",
		update: "UPDATE sc_users SET name = $1 WHERE id = $2",
		read:   "SELECT name FROM sc_users WHERE id = $1",
	}
	// v2: writes both columns, still reads the old one.
	v2 = &version{
		insert: "INSERT INTO sc_users (name, full_name, email) VALUES ($1, $1, $2)",
		update: "UPDATE sc_users SET name = $1, full_name = $1 WHERE id = $2",
		read:   "SELECT name FROM sc_users WHERE id = $1",
	}
	// v3: writes both columns, reads the new one.
	v3 = &version{
		insert: v2.insert,
		update: v2.update,
		read:   "SELECT full_name FROM sc_users WHERE id = $1",
	}
	// v4: the code after the change. It knows only `full_name`.
	v4 = &version{
		insert: "INSERT INTO sc_users (full_name, email) VALUES ($1, $2)",
		update: "UPDATE sc_users SET full_name = $1 WHERE id = $2",
		read:   "SELECT full_name FROM sc_users WHERE id = $1",
	}
)

// A fleet is the running application: several instances, each on some
// version of the code, serving requests until stopped.
type fleet struct {
	running  [instances]atomic.Pointer[version]
	requests atomic.Int64
	failed   atomic.Int64
	example  atomic.Value // one error message, as an example
	stop     chan struct{}
	wg       sync.WaitGroup
}

func startFleet(v *version) *fleet {
	f := &fleet{stop: make(chan struct{})}
	for i := range f.running {
		f.running[i].Store(v)
		f.wg.Add(1)
		go f.instance(i)
	}
	time.Sleep(300 * time.Millisecond)
	return f
}

func (f *fleet) instance(i int) {
	defer f.wg.Done()
	conn := connect()
	defer conn.Close(bg)
	rng := rand.New(rand.NewSource(int64(i)))
	for {
		select {
		case <-f.stop:
			return
		default:
		}
		v := f.running[i].Load()
		var err error
		switch n := rng.Intn(10); {
		case n == 0: // sign-up
			_, err = conn.Exec(bg, v.insert, fmt.Sprintf("new user %d", rng.Int63()), "new@example.com")
		case n <= 2: // a user edits their name
			_, err = conn.Exec(bg, v.update, fmt.Sprintf("renamed %d", rng.Int63()), 1+rng.Intn(hotUsers))
		default: // show a user's name
			var name string
			err = conn.QueryRow(bg, v.read, 1+rng.Intn(users)).Scan(&name)
		}
		f.requests.Add(1)
		if err != nil {
			f.failed.Add(1)
			f.example.CompareAndSwap(nil, err.Error())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// deploy replaces the running code one instance at a time.
func (f *fleet) deploy(v *version) {
	for i := range f.running {
		f.running[i].Store(v)
		time.Sleep(100 * time.Millisecond)
	}
}

func (f *fleet) shutdown() {
	time.Sleep(300 * time.Millisecond)
	close(f.stop)
	f.wg.Wait()
}

// step runs one step of a change and prints how long it took, and how many
// requests the application served, and failed, while it ran.
func (f *fleet) step(name string, do func()) {
	requests, failed := f.requests.Load(), f.failed.Load()
	start := time.Now()
	do()
	took := time.Since(start)
	fmt.Printf("  %-62s %7s %10d %8d\n", name, short(took), f.requests.Load()-requests, f.failed.Load()-failed)
}

func (f *fleet) summary() {
	fmt.Printf("  => %d of %d requests failed", f.failed.Load(), f.requests.Load())
	if msg, ok := f.example.Load().(string); ok {
		fmt.Printf(", for example:\n     %s", msg)
	}
	fmt.Print("\n\n")
}

// ------------------------------------------------- the migration tools

// migrate runs one schema change the way a migration tool should: the
// connection has a lock timeout (set in main), so the change can never sit
// in a table's lock queue blocking everyone behind it (Chapter 2.6); if it
// cannot get its lock in time, it backs off and tries again.
func migrate(conn *pgx.Conn, sql string) {
	for attempt := 1; ; attempt++ {
		_, err := conn.Exec(bg, sql)
		if err == nil {
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" && attempt < 10 { // lock_not_available
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
			continue
		}
		log.Fatalf("%s: %v", sql, err)
	}
}

// backfill copies name into full_name, 10,000 rows at a time. Each batch
// is its own short transaction: no lock is held for long, and a failure
// loses one batch, not the whole job. The condition is "differs", not "is
// empty": see the chapter for why, or run with -backfill-only-empty.
func backfill(conn *pgx.Conn) {
	const batch = 10_000
	condition := "full_name IS DISTINCT FROM name"
	if *onlyEmpty {
		condition = "full_name IS NULL"
	}
	var last int64
	if err := conn.QueryRow(bg, "SELECT max(id) FROM sc_users").Scan(&last); err != nil {
		log.Fatal(err)
	}
	for from := int64(1); from <= last; from += batch {
		mustExec(conn, "UPDATE sc_users SET full_name = name WHERE id >= $1 AND id < $2 AND "+condition, from, from+batch)
		time.Sleep(10 * time.Millisecond) // leave room for the application
	}
}

func count(conn *pgx.Conn, where string) (n int) {
	if err := conn.QueryRow(bg, "SELECT count(*) FROM sc_users WHERE "+where).Scan(&n); err != nil {
		log.Fatal(err)
	}
	return n
}

func resetTable(conn *pgx.Conn) {
	mustExec(conn, "DROP TABLE IF EXISTS sc_users")
	mustExec(conn, `CREATE TABLE sc_users (
		id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		name  text NOT NULL,
		email text NOT NULL)`)
	mustExec(conn, fmt.Sprintf(`INSERT INTO sc_users (name, email)
		SELECT 'user ' || g, 'user' || g || '@example.com' FROM generate_series(1, %d) g`, users))
	mustExec(conn, "VACUUM ANALYZE sc_users")
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

func header(title string) {
	fmt.Println(title)
	fmt.Printf("  %-62s %7s %10s %8s\n", "step", "took", "requests", "failed")
}

// ------------------------------------------------- three ways

func main() {
	flag.Parse()
	admin := connect()
	defer admin.Close(bg)
	mustExec(admin, "SET lock_timeout = '2s'")

	fmt.Printf("Renaming users.name to full_name. %d users; %d instances of the application serve requests throughout.\n\n",
		users, instances)

	// Way 1.
	resetTable(admin)
	f := startFleet(v1)
	header("WAY 1 — rename the column, then deploy the new code")
	f.step("ALTER TABLE ... RENAME COLUMN name TO full_name", func() {
		migrate(admin, "ALTER TABLE sc_users RENAME COLUMN name TO full_name")
	})
	f.step("rolling deploy: v1 (uses name) -> v4 (uses full_name)", func() { f.deploy(v4) })
	f.shutdown()
	f.summary()

	// Way 2.
	resetTable(admin)
	f = startFleet(v1)
	header("WAY 2 — deploy the new code, then rename the column")
	f.step("rolling deploy: v1 (uses name) -> v4 (uses full_name)", func() { f.deploy(v4) })
	f.step("ALTER TABLE ... RENAME COLUMN name TO full_name", func() {
		migrate(admin, "ALTER TABLE sc_users RENAME COLUMN name TO full_name")
	})
	f.shutdown()
	f.summary()

	// Way 3.
	resetTable(admin)
	f = startFleet(v1)
	header("WAY 3 — expand, migrate, contract")
	f.step("expand:   ADD COLUMN full_name text", func() {
		migrate(admin, "ALTER TABLE sc_users ADD COLUMN full_name text")
	})
	f.step("deploy:   v2 — write both columns, read name", func() { f.deploy(v2) })
	missing := count(admin, "full_name IS NULL")
	stale := count(admin, "full_name IS NOT NULL AND full_name <> name")
	fmt.Printf("            (full_name is missing in %d rows and out of date in %d)\n", missing, stale)
	f.step("migrate:  backfill full_name in batches of 10,000 rows", func() { backfill(admin) })
	fmt.Printf("            (rows where full_name differs from name: %d)\n", count(admin, "full_name IS DISTINCT FROM name"))
	f.step("migrate:  make full_name NOT NULL, without a long lock", func() {
		migrate(admin, "ALTER TABLE sc_users ADD CONSTRAINT full_name_present CHECK (full_name IS NOT NULL) NOT VALID")
		migrate(admin, "ALTER TABLE sc_users VALIDATE CONSTRAINT full_name_present")
		migrate(admin, "ALTER TABLE sc_users ALTER COLUMN full_name SET NOT NULL")
		migrate(admin, "ALTER TABLE sc_users DROP CONSTRAINT full_name_present")
	})
	f.step("deploy:   v3 — write both columns, read full_name", func() { f.deploy(v3) })
	f.step("contract: ALTER COLUMN name DROP NOT NULL", func() {
		migrate(admin, "ALTER TABLE sc_users ALTER COLUMN name DROP NOT NULL")
	})
	f.step("deploy:   v4 — write and read full_name only", func() { f.deploy(v4) })
	f.step("contract: DROP COLUMN name", func() {
		migrate(admin, "ALTER TABLE sc_users DROP COLUMN name")
	})
	f.shutdown()
	f.summary()

	mustExec(admin, "DROP TABLE sc_users")
}
