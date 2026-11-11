// Chapter 2.3 — what two transactions can do to each other at each
// isolation level, shown on a real PostgreSQL.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/ch03-isolation
//
// The program opens two connections, A and B, and makes them take turns in
// a fixed order, printing what each one sees. Nothing is simulated: the
// results and the errors come from the database.
//
// The connection string comes from DATABASE_URL and defaults to
// postgres://postgres:postgres@localhost:5432/postgres.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ctx = context.Background()

// session is one connection to the database, with a name for the printout.
type session struct {
	name string
	conn *pgx.Conn
}

// run executes a statement and prints one line about it. For a SELECT it
// prints the single value returned; otherwise the number of rows changed.
// It returns the value (if any) and the error (if any).
func (s *session) run(note, sql string, args ...any) (int, error) {
	value, rows, err := s.do(sql, args...)
	s.print(note, sql, value, rows, err, "")
	return value, err
}

func (s *session) do(sql string, args ...any) (value int, rows int64, err error) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
		err = s.conn.QueryRow(ctx, sql, args...).Scan(&value)
		return value, 0, err
	}
	tag, err := s.conn.Exec(ctx, sql, args...)
	return 0, tag.RowsAffected(), err
}

func (s *session) print(note, sql string, value int, rows int64, err error, suffix string) {
	var outcome string
	upper := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case err != nil:
		outcome = "ERROR " + describe(err)
	case strings.HasPrefix(upper, "SELECT"):
		outcome = fmt.Sprintf("%d", value)
	case strings.HasPrefix(upper, "UPDATE") && rows == 1:
		outcome = "1 row updated"
	case strings.HasPrefix(upper, "UPDATE"):
		outcome = fmt.Sprintf("%d rows updated", rows)
	default:
		outcome = "ok"
	}
	if note == "" {
		note = sql
	}
	fmt.Printf("   %s: %-50s -> %s%s\n", s.name, note, outcome, suffix)
}

// runBlocked starts a statement that is expected to wait for a lock held by
// the other session. It returns a function that, once the other session has
// let go, collects and prints the result.
func (s *session) runBlocked(note, sql string, args ...any) (finish func() error) {
	type result struct {
		value int
		rows  int64
		err   error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		v, r, err := s.do(sql, args...)
		done <- result{v, r, err}
	}()
	select {
	case res := <-done: // did not block after all
		s.print(note, sql, res.value, res.rows, res.err, "")
		return func() error { return res.err }
	case <-time.After(300 * time.Millisecond):
		fmt.Printf("   %s: %-50s ... waiting for the other transaction\n", s.name, note)
	}
	return func() error {
		res := <-done
		waited := time.Since(start).Round(100 * time.Millisecond)
		s.print("   (the statement above, once unblocked)", sql, res.value, res.rows, res.err,
			fmt.Sprintf("   [waited %v]", waited))
		return res.err
	}
}

// describe turns a database error into its SQLSTATE code and message.
func describe(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("%s: %s", pgErr.Code, pgErr.Message)
	}
	return err.Error()
}

func connect(name string) *session {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatalf("cannot connect to PostgreSQL (%v). See phase-2-data/README.md.", err)
	}
	return &session{name: name, conn: conn}
}

// reset puts the tables back to their starting state.
func reset(s *session) {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS iso_accounts, iso_doctors`,
		`CREATE TABLE iso_accounts (id int PRIMARY KEY, balance int NOT NULL)`,
		`INSERT INTO iso_accounts VALUES (1, 100)`,
		`CREATE TABLE iso_doctors (name text PRIMARY KEY, on_call boolean NOT NULL)`,
		`INSERT INTO iso_doctors VALUES ('alice', true), ('bob', true)`,
	} {
		if _, err := s.conn.Exec(ctx, stmt); err != nil {
			log.Fatal(err)
		}
	}
}

func final(s *session, label, sql string) {
	var v int
	s.conn.QueryRow(ctx, sql).Scan(&v)
	fmt.Printf("   %s: %d\n", label, v)
}

const (
	readBalance = `SELECT balance FROM iso_accounts WHERE id = 1`
	countOnCall = `SELECT count(*) FROM iso_doctors WHERE on_call`
)

func main() {
	a, b := connect("A"), connect("B")
	defer a.conn.Close(ctx)
	defer b.conn.Close(ctx)

	// ------------------------------------------------------------------
	fmt.Println("1. Reading the same row twice in one transaction")
	for _, level := range []string{"READ COMMITTED", "REPEATABLE READ"} {
		reset(a)
		fmt.Println("   -- at " + level)
		a.run("BEGIN", "BEGIN ISOLATION LEVEL "+level)
		a.run("SELECT balance", readBalance)
		b.run("UPDATE balance = 20; COMMIT (on its own)", `UPDATE iso_accounts SET balance = 20 WHERE id = 1`)
		a.run("SELECT balance   (same query again)", readBalance)
		a.run("COMMIT", "COMMIT")
	}

	// ------------------------------------------------------------------
	fmt.Println("\n2. Two withdrawals of 80 from a balance of 100, at READ COMMITTED (the default)")
	fmt.Println("   Each transaction reads the balance, checks it in application code, and writes the new value.")
	reset(a)
	a.run("BEGIN", "BEGIN")
	b.run("BEGIN", "BEGIN")
	balA, _ := a.run("SELECT balance", readBalance)
	balB, _ := b.run("SELECT balance", readBalance)
	a.run(fmt.Sprintf("UPDATE balance = %d   (%d - 80)", balA-80, balA), `UPDATE iso_accounts SET balance = $1 WHERE id = 1`, balA-80)
	finish := b.runBlocked(fmt.Sprintf("UPDATE balance = %d   (%d - 80)", balB-80, balB), `UPDATE iso_accounts SET balance = $1 WHERE id = 1`, balB-80)
	a.run("COMMIT", "COMMIT")
	finish()
	b.run("COMMIT", "COMMIT")
	final(a, "final balance", readBalance)
	fmt.Println("   Both withdrawals succeeded: 160 was paid out, and only 80 left the account.")

	// ------------------------------------------------------------------
	fmt.Println("\n3. The same two withdrawals at REPEATABLE READ")
	reset(a)
	a.run("BEGIN", "BEGIN ISOLATION LEVEL REPEATABLE READ")
	b.run("BEGIN", "BEGIN ISOLATION LEVEL REPEATABLE READ")
	balA, _ = a.run("SELECT balance", readBalance)
	balB, _ = b.run("SELECT balance", readBalance)
	a.run(fmt.Sprintf("UPDATE balance = %d   (%d - 80)", balA-80, balA), `UPDATE iso_accounts SET balance = $1 WHERE id = 1`, balA-80)
	finish = b.runBlocked(fmt.Sprintf("UPDATE balance = %d   (%d - 80)", balB-80, balB), `UPDATE iso_accounts SET balance = $1 WHERE id = 1`, balB-80)
	a.run("COMMIT", "COMMIT")
	if err := finish(); err != nil {
		b.run("ROLLBACK", "ROLLBACK")
		fmt.Println("   B must start again from the beginning:")
		b.run("BEGIN", "BEGIN ISOLATION LEVEL REPEATABLE READ")
		balB, _ = b.run("SELECT balance", readBalance)
		fmt.Printf("   B: balance %d is less than 80: refuse the withdrawal\n", balB)
		b.run("ROLLBACK", "ROLLBACK")
	}
	final(a, "final balance", readBalance)

	// ------------------------------------------------------------------
	fmt.Println("\n4. Write skew: at least one doctor must stay on call")
	fmt.Println("   Alice and Bob are both on call. Each asks to go off call at the same moment.")
	for _, level := range []string{"REPEATABLE READ", "SERIALIZABLE"} {
		reset(a)
		fmt.Println("   -- at " + level)
		a.run("BEGIN", "BEGIN ISOLATION LEVEL "+level)
		b.run("BEGIN", "BEGIN ISOLATION LEVEL "+level)
		a.run("SELECT count(*) on call  (2, so Alice may go)", countOnCall)
		b.run("SELECT count(*) on call  (2, so Bob may go)", countOnCall)
		_, errA := a.run("UPDATE alice: on_call = false", `UPDATE iso_doctors SET on_call = false WHERE name = 'alice'`)
		_, errB := b.run("UPDATE bob:   on_call = false", `UPDATE iso_doctors SET on_call = false WHERE name = 'bob'`)
		if errA == nil {
			_, errA = a.run("COMMIT", "COMMIT")
		}
		if errA != nil {
			a.run("ROLLBACK", "ROLLBACK")
		}
		if errB == nil {
			_, errB = b.run("COMMIT", "COMMIT")
		}
		if errB != nil {
			b.run("ROLLBACK", "ROLLBACK")
		}
		final(a, "doctors on call", countOnCall)
	}

	// ------------------------------------------------------------------
	fmt.Println("\n5. The withdrawals again at READ COMMITTED, as one statement that checks and changes together")
	reset(a)
	withdraw := `UPDATE iso_accounts SET balance = balance - 80 WHERE id = 1 AND balance >= 80`
	a.run("BEGIN", "BEGIN")
	b.run("BEGIN", "BEGIN")
	a.run("UPDATE balance = balance - 80 WHERE balance >= 80", withdraw)
	finish = b.runBlocked("UPDATE balance = balance - 80 WHERE balance >= 80", withdraw)
	a.run("COMMIT", "COMMIT")
	finish()
	b.run("COMMIT", "COMMIT")
	final(a, "final balance", readBalance)
	fmt.Println("   B's statement changed no rows, so the application refuses the second withdrawal.")

	a.conn.Exec(ctx, `DROP TABLE IF EXISTS iso_accounts, iso_doctors`)
}
