// Chapter 5.7 — moving money between two services, each with its own data.
//
// Bank A and bank B each keep their own table of accounts and use their own
// connection: neither can see the other's transaction. (They are two tables
// in one PostgreSQL server here, so that the example needs only one; what
// matters is that each side commits separately.) A transfer takes 10 from an
// account at A and adds 10 to an account at B.
//
//	part 1  two commits, one after the other
//	part 2  two-phase commit
//	part 3  a saga: local commits, and a compensation when the second fails
//	part 4  both accounts in one place: one transaction
//
// Each part makes 2,000 transfers while an auditor adds up all the money
// 200 times. In one transfer in a hundred, the process that is carrying it
// out dies half-way.
//
// Needs the database from Phase 2 (DATABASE_URL, as before). Part 2 needs
// one setting, max_prepared_transactions; the program says how if it is missing.
//
//	go run ./phase-5-distributed-data/code/ch07-transfers
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
DROP TABLE IF EXISTS tx_bank_a, tx_bank_b, tx_saga;
CREATE TABLE tx_bank_a (id int PRIMARY KEY, balance int NOT NULL CHECK (balance >= 0));
CREATE TABLE tx_bank_b (id int PRIMARY KEY, balance int NOT NULL, open boolean NOT NULL DEFAULT true);
INSERT INTO tx_bank_a SELECT g, 100000 FROM generate_series(1, 50) g;
INSERT INTO tx_bank_b SELECT g, 0, g % 20 <> 0 FROM generate_series(1, 50) g;   -- one account in twenty at B is closed
-- The saga's own record: which step each transfer has reached.
CREATE TABLE tx_saga (id int PRIMARY KEY, from_a int, to_b int, state text NOT NULL);`

const total = 50 * 100000

var errDied = errors.New("the process died") // half-way through a transfer

const (
	debit  = "UPDATE tx_bank_a SET balance = balance - 10 WHERE id = $1"
	credit = "UPDATE tx_bank_b SET balance = balance + 10 WHERE id = $1 AND open"
	refund = "UPDATE tx_bank_a SET balance = balance + 10 WHERE id = $1"
)

// ------------------------------------------------------------ four ways to transfer

// 1. Commit at A, then commit at B.
func twoCommits(id, from, to int, dies bool) error {
	if _, err := pool.Exec(ctx, debit, from); err != nil {
		return err
	}
	if dies {
		return errDied // the money has left A
	}
	tag, err := pool.Exec(ctx, credit, to)
	if err == nil && tag.RowsAffected() == 0 {
		return errors.New("the account at B is closed") // and the money has left A
	}
	return err
}

// 2. Two-phase commit: both sides prepare — promise that they can commit —
// and only when both have promised are both told to commit.
func twoPhase(id, from, to int, dies bool) error {
	a, b := fmt.Sprint("transfer-", id, "-a"), fmt.Sprint("transfer-", id, "-b")
	prepare := func(name, sql string, account int) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		// Do not wait for ever for a row that someone else has locked.
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '100ms'"); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, sql, account)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("the account at B is closed")
		}
		_, err = tx.Exec(ctx, fmt.Sprintf("PREPARE TRANSACTION '%s'", name))
		return err
	}
	if err := prepare(a, debit, from); err != nil {
		return err
	}
	if err := prepare(b, credit, to); err != nil {
		pool.Exec(ctx, fmt.Sprintf("ROLLBACK PREPARED '%s'", a)) // B said no: A is told to abandon
		return err
	}
	if dies {
		return errDied // the coordinator dies. Both sides have promised, and neither has been told what to do
	}
	_, err := pool.Exec(ctx, fmt.Sprintf("COMMIT PREPARED '%s'", a))
	must(err)
	_, err = pool.Exec(ctx, fmt.Sprintf("COMMIT PREPARED '%s'", b))
	return err
}

// 3. A saga: each step is an ordinary local transaction, which also records
// how far the transfer has got. If a later step fails, earlier ones are undone
// by a new transaction that does the opposite.
func saga(id, from, to int, dies bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, debit, from); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO tx_saga VALUES ($1, $2, $3, 'debited')", id, from, to); err != nil {
		return err
	}
	must(tx.Commit(ctx)) // step 1 is done, and recorded as done, in one commit
	if dies {
		return errDied // nothing is lost: the record says what to do next
	}
	return sagaContinue(id, from, to)
}

// sagaContinue carries a transfer on from "debited". It is also what a
// recovery process runs for transfers that it finds left in that state.
func sagaContinue(id, from, to int) error {
	tag, err := pool.Exec(ctx, credit, to)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		_, err = pool.Exec(ctx, "UPDATE tx_saga SET state = 'done' WHERE id = $1", id)
		return err
	}
	// B refused: compensate. The refund and the record of it are one transaction.
	_, err = pool.Exec(ctx, `WITH undone AS (UPDATE tx_saga SET state = 'refunded' WHERE id = $1 AND state = 'debited' RETURNING from_a)
	                          UPDATE tx_bank_a SET balance = balance + 10 WHERE id = (SELECT from_a FROM undone)`, id)
	return err
}

// 4. One transaction, because both accounts are in the same database.
func oneTransaction(id, from, to int, dies bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, debit, from); err != nil {
		return err
	}
	if dies {
		return errDied // rolled back: nothing happened
	}
	tag, err := tx.Exec(ctx, credit, to)
	if err != nil || tag.RowsAffected() == 0 {
		return errors.New("the account at B is closed")
	}
	return tx.Commit(ctx)
}

// ------------------------------------------------------------ the experiment

func money() (sum int) {
	must(pool.QueryRow(ctx, "SELECT (SELECT sum(balance) FROM tx_bank_a) + (SELECT sum(balance) FROM tx_bank_b)").Scan(&sum))
	return sum
}

// abandon rolls back any transaction left prepared by an earlier, interrupted run.
func abandon() {
	rows, err := pool.Query(ctx, "SELECT gid FROM pg_prepared_xacts WHERE gid LIKE 'transfer-%'")
	must(err)
	var names []string
	for rows.Next() {
		var gid string
		must(rows.Scan(&gid))
		names = append(names, gid)
	}
	for _, gid := range names {
		pool.Exec(ctx, fmt.Sprintf("ROLLBACK PREPARED '%s'", gid))
	}
}

func run(name string, transfer func(id, from, to int, dies bool) error, recover func()) {
	abandon()
	_, err := pool.Exec(ctx, schema)
	must(err)
	var wrongAudits, audits, blocked atomic.Int64
	stop := make(chan struct{})
	auditing := make(chan struct{})
	go func() { // the auditor: adds up every account at both banks, again and again
		defer close(auditing)
		for {
			select {
			case <-stop:
				return
			default:
			}
			audits.Add(1)
			if money() != total {
				wrongAudits.Add(1)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	rng := rand.New(rand.NewSource(7))
	var mu sync.Mutex
	var times []time.Duration
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for id := 1; id <= 2000; id++ {
		from, to, dies := 1+rng.Intn(50), 1+rng.Intn(50), rng.Intn(100) == 0
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			begin := time.Now()
			err := transfer(id, from, to, dies)
			if err == nil {
				mu.Lock()
				times = append(times, time.Since(begin))
				mu.Unlock()
			} else if strings.Contains(err.Error(), "55P03") {
				blocked.Add(1) // gave up waiting for a lock
			}
			<-slots
		}()
	}
	wg.Wait()
	close(stop)
	<-auditing

	var stuck int
	must(pool.QueryRow(ctx, "SELECT count(*) FROM pg_prepared_xacts WHERE gid LIKE 'transfer-%'").Scan(&stuck))
	before := money()
	if recover != nil {
		recover()
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	fmt.Printf("  %-34s %10.2f ms %12d%% %16d %18d %16d %18d\n", name, float64(times[len(times)/2].Microseconds())/1000,
		wrongAudits.Load()*100/audits.Load(), (total-before)/10, stuck, blocked.Load(), (total-money())/10)
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	flag.Parse()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	var err error
	pool, err = pgxpool.New(ctx, url+"?pool_max_conns=20")
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the database of Phase 2 first: see phase-2-data/README.md", url, err)
	}
	defer pool.Close()
	var prepared int
	must(pool.QueryRow(ctx, "SELECT current_setting('max_prepared_transactions')::int").Scan(&prepared))

	fmt.Println("2,000 transfers of 10 from accounts at bank A to accounts at bank B. One account in twenty at B is closed.")
	fmt.Println("In one transfer in a hundred, the process carrying it out dies half-way. An auditor adds up all the money")
	fmt.Println("throughout; it should always come to the same total.")
	fmt.Println()
	fmt.Printf("  %-34s %13s %13s %16s %18s %16s %18s\n", "", "one transfer", "audits that", "transfers' worth", "transactions left", "transfers that", "transfers' worth")
	fmt.Printf("  %-34s %13s %13s %16s %18s %16s %18s\n", "", "takes", "found the", "of money missing", "prepared, holding", "gave up waiting", "missing after")
	fmt.Printf("  %-34s %13s %13s %16s %18s %16s %18s\n", "", "(median)", "total wrong", "at the end", "locks", "for a lock", "recovery")

	if *part == 0 || *part == 1 {
		run("two commits, one after the other", twoCommits, nil)
	}
	if *part == 0 || *part == 2 {
		if prepared == 0 {
			fmt.Println("  two-phase commit                   needs  ALTER SYSTEM SET max_prepared_transactions = 20;  and a restart of")
			fmt.Println("                                     PostgreSQL (with Docker: add  -c max_prepared_transactions=20  to docker run)")
		} else {
			run("two-phase commit", twoPhase, func() {
				// Recovery: someone who knows that the decision was "commit" finishes the job.
				rows, err := pool.Query(ctx, "SELECT gid FROM pg_prepared_xacts WHERE gid LIKE 'transfer-%'")
				must(err)
				var names []string
				for rows.Next() {
					var gid string
					must(rows.Scan(&gid))
					names = append(names, gid)
				}
				for _, gid := range names {
					_, err := pool.Exec(ctx, fmt.Sprintf("COMMIT PREPARED '%s'", gid))
					must(err)
				}
			})
		}
	}
	if *part == 0 || *part == 3 {
		run("a saga, with compensation", saga, func() {
			// Recovery: carry on every transfer that was left half-way.
			rows, err := pool.Query(ctx, "SELECT id, from_a, to_b FROM tx_saga WHERE state = 'debited'")
			must(err)
			type left struct{ id, from, to int }
			var pending []left
			for rows.Next() {
				var l left
				must(rows.Scan(&l.id, &l.from, &l.to))
				pending = append(pending, l)
			}
			for _, l := range pending {
				must(sagaContinue(l.id, l.from, l.to))
			}
		})
	}
	if *part == 0 || *part == 4 {
		run("one transaction, in one database", oneTransaction, nil)
	}
	_, err = pool.Exec(ctx, "DROP TABLE IF EXISTS tx_bank_a, tx_bank_b, tx_saga")
	must(err)
}
