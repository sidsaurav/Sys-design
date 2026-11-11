// Chapter 2.4 — five ways to sell the last items in stock, and one way to
// deadlock. All against a real PostgreSQL.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/ch04-concurrency
//
// Part 1: 5 items are in stock and 40 customers try to buy one at the same
// moment. Exactly 5 should succeed. Each strategy is run from the same
// starting state and judged by what ends up in the database.
//
// Part 2: money moves between two accounts in both directions at once.
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	initialStock = 5
	buyers       = 40
	// thinking stands in for the work an application does between reading
	// a row and writing it back: validation, pricing, calling other code.
	thinking = 2 * time.Millisecond
)

var ctx = context.Background()

var errSoldOut = errors.New("sold out")

func connect() *pgx.Conn {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatalf("cannot connect to PostgreSQL (%v). See phase-2-data/README.md.", err)
	}
	return conn
}

// sqlState returns the five-character error code of a database error.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// A strategy tries to buy one item for one customer. It returns nil if the
// customer got an item, errSoldOut if there were none, and reports how many
// times it had to start again.
type strategy func(conn *pgx.Conn, customer int) (retries int, err error)

// 1. Read the stock, decide in the application, write the new value.
// This is the natural way to write it, and it is wrong.
func naive(conn *pgx.Conn, customer int) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var stock int
	if err := tx.QueryRow(ctx, `SELECT stock FROM cc_items WHERE id = 1`).Scan(&stock); err != nil {
		return 0, err
	}
	if stock <= 0 {
		return 0, errSoldOut
	}
	time.Sleep(thinking)
	if _, err := tx.Exec(ctx, `UPDATE cc_items SET stock = $1 WHERE id = 1`, stock-1); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cc_purchases (customer) VALUES ($1)`, customer); err != nil {
		return 0, err
	}
	return 0, tx.Commit(ctx)
}

// 2. One statement that checks and changes together. The database
// re-evaluates "stock > 0" against the latest committed row (Chapter 2.3).
func oneStatement(conn *pgx.Conn, customer int) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE cc_items SET stock = stock - 1 WHERE id = 1 AND stock > 0`)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, errSoldOut
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cc_purchases (customer) VALUES ($1)`, customer); err != nil {
		return 0, err
	}
	return 0, tx.Commit(ctx)
}

// 3. Pessimistic locking: lock the row first, so nobody else can read it
// "for update" or change it until we finish. Everyone else waits in line.
func pessimistic(conn *pgx.Conn, customer int) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var stock int
	if err := tx.QueryRow(ctx, `SELECT stock FROM cc_items WHERE id = 1 FOR UPDATE`).Scan(&stock); err != nil {
		return 0, err
	}
	if stock <= 0 {
		return 0, errSoldOut
	}
	time.Sleep(thinking)
	if _, err := tx.Exec(ctx, `UPDATE cc_items SET stock = $1 WHERE id = 1`, stock-1); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cc_purchases (customer) VALUES ($1)`, customer); err != nil {
		return 0, err
	}
	return 0, tx.Commit(ctx)
}

// 4. Optimistic locking: take no lock. Remember the row's version, and make
// the write conditional on the version being unchanged. If somebody got in
// first, zero rows change and we start again from the read.
func optimistic(conn *pgx.Conn, customer int) (retries int, err error) {
	for {
		var stock, version int
		if err := conn.QueryRow(ctx, `SELECT stock, version FROM cc_items WHERE id = 1`).Scan(&stock, &version); err != nil {
			return retries, err
		}
		if stock <= 0 {
			return retries, errSoldOut
		}
		time.Sleep(thinking)

		tx, err := conn.Begin(ctx)
		if err != nil {
			return retries, err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE cc_items SET stock = $1, version = version + 1 WHERE id = 1 AND version = $2`,
			stock-1, version)
		if err != nil {
			tx.Rollback(ctx)
			return retries, err
		}
		if tag.RowsAffected() == 0 { // someone changed the row since we read it
			tx.Rollback(ctx)
			retries++
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cc_purchases (customer) VALUES ($1)`, customer); err != nil {
			tx.Rollback(ctx)
			return retries, err
		}
		return retries, tx.Commit(ctx)
	}
}

// 5. The naive code, unchanged, but run at SERIALIZABLE inside a loop that
// retries when the database reports a serialization failure (40001).
func serializable(conn *pgx.Conn, customer int) (retries int, err error) {
	for {
		err := func() error {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)

			var stock int
			if err := tx.QueryRow(ctx, `SELECT stock FROM cc_items WHERE id = 1`).Scan(&stock); err != nil {
				return err
			}
			if stock <= 0 {
				return errSoldOut
			}
			time.Sleep(thinking)
			if _, err := tx.Exec(ctx, `UPDATE cc_items SET stock = $1 WHERE id = 1`, stock-1); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO cc_purchases (customer) VALUES ($1)`, customer); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if sqlState(err) == "40001" {
			retries++
			continue // the whole transaction, from the read
		}
		return retries, err
	}
}

func resetStock(admin *pgx.Conn) {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS cc_items, cc_purchases, cc_accounts`,
		`CREATE TABLE cc_items (id int PRIMARY KEY, stock int NOT NULL, version int NOT NULL DEFAULT 0)`,
		fmt.Sprintf(`INSERT INTO cc_items VALUES (1, %d)`, initialStock),
		`CREATE TABLE cc_purchases (customer int NOT NULL)`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			log.Fatal(err)
		}
	}
}

func partOne(admin *pgx.Conn, conns []*pgx.Conn) {
	fmt.Printf("PART 1 — %d items in stock, %d customers buy at the same moment. Exactly %d should succeed.\n\n",
		initialStock, buyers, initialStock)
	fmt.Println("  strategy                              told \"yours\"   stock left   start-overs    time   verdict")

	for _, s := range []struct {
		name string
		run  strategy
	}{
		{"1. read, check, write (naive)", naive},
		{"2. one statement: check and change", oneStatement},
		{"3. pessimistic: SELECT ... FOR UPDATE", pessimistic},
		{"4. optimistic: version column", optimistic},
		{"5. SERIALIZABLE with a retry loop", serializable},
	} {
		resetStock(admin)
		var wg sync.WaitGroup
		var retries, failures atomic.Int64
		start := time.Now()
		for i, conn := range conns {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := s.run(conn, i)
				retries.Add(int64(n))
				if err != nil && !errors.Is(err, errSoldOut) {
					failures.Add(1)
					log.Printf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		took := time.Since(start)

		var sold, stock int
		admin.QueryRow(ctx, `SELECT count(*) FROM cc_purchases`).Scan(&sold)
		admin.QueryRow(ctx, `SELECT stock FROM cc_items WHERE id = 1`).Scan(&stock)
		verdict := "correct"
		if sold != initialStock || stock != 0 {
			verdict = fmt.Sprintf("WRONG: sold %d items that did not exist", sold-initialStock)
		}
		fmt.Printf("  %-38s %6d        %6d      %6d      %5dms   %s\n",
			s.name, sold, stock, retries.Load(), took.Milliseconds(), verdict)
	}
}

// transfer moves 1 from one account to another, locking both rows first.
// If inOrder is true it always locks the lower id first, whatever the
// direction of the transfer.
func transfer(conn *pgx.Conn, from, to int, inOrder bool) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	first, second := from, to
	if inOrder && first > second {
		first, second = second, first
	}
	var balance int
	if err := tx.QueryRow(ctx, `SELECT balance FROM cc_accounts WHERE id = $1 FOR UPDATE`, first).Scan(&balance); err != nil {
		return err
	}
	time.Sleep(thinking)
	if err := tx.QueryRow(ctx, `SELECT balance FROM cc_accounts WHERE id = $1 FOR UPDATE`, second).Scan(&balance); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE cc_accounts SET balance = balance - 1 WHERE id = $1`, from); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE cc_accounts SET balance = balance + 1 WHERE id = $1`, to); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func partTwo(admin *pgx.Conn, conns []*pgx.Conn) {
	fmt.Printf("\nPART 2 — %d transfers at once: half from account 1 to 2, half from 2 to 1.\n\n", len(conns))
	fmt.Println("  locking order                          completed   deadlocks (40P01)   total money")
	for _, mode := range []struct {
		name    string
		inOrder bool
	}{
		{"lock the sender, then the receiver", false},
		{"always lock the lower id first", true},
	} {
		for _, stmt := range []string{
			`DROP TABLE IF EXISTS cc_accounts`,
			`CREATE TABLE cc_accounts (id int PRIMARY KEY, balance int NOT NULL)`,
			`INSERT INTO cc_accounts VALUES (1, 1000), (2, 1000)`,
		} {
			if _, err := admin.Exec(ctx, stmt); err != nil {
				log.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		var ok, deadlocks atomic.Int64
		for i, conn := range conns {
			from, to := 1, 2
			if i%2 == 1 {
				from, to = 2, 1
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch err := transfer(conn, from, to, mode.inOrder); {
				case err == nil:
					ok.Add(1)
				case sqlState(err) == "40P01":
					deadlocks.Add(1)
				default:
					log.Printf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		var total int
		admin.QueryRow(ctx, `SELECT sum(balance) FROM cc_accounts`).Scan(&total)
		fmt.Printf("  %-38s %6d        %6d            %6d\n", mode.name, ok.Load(), deadlocks.Load(), total)
	}
}

func main() {
	admin := connect()
	defer admin.Close(ctx)
	conns := make([]*pgx.Conn, buyers)
	for i := range conns {
		conns[i] = connect()
		defer conns[i].Close(ctx)
	}

	partOne(admin, conns)
	partTwo(admin, conns)

	admin.Exec(ctx, `DROP TABLE IF EXISTS cc_items, cc_purchases, cc_accounts`)
}
