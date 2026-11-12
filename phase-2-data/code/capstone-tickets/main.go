// Capstone 2 — ticket booking: never sell the same seat twice.
//
// A reference implementation of the worked design in
// phase-2-data/capstone-ticket-booking.md, and a simulated on-sale rush
// that tries hard to break it.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/capstone-tickets demo            # one walk through every case
//	go run ./phase-2-data/code/capstone-tickets rush            # 12,000 customers, 5,000 seats
//	go run ./phase-2-data/code/capstone-tickets rush -naive     # the same, with check-then-act code
//	go run ./phase-2-data/code/capstone-tickets rush -naive -no-constraint
//	go run ./phase-2-data/code/capstone-tickets plans           # the query plans of the main queries
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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ctx = context.Background()

// ------------------------------------------------------------ the schema

const schema = `
DROP TABLE IF EXISTS tk_tickets, tk_seats, tk_customer_limits, tk_orders, tk_events;

CREATE TABLE tk_events (
    id        bigint PRIMARY KEY,
    name      text NOT NULL,
    starts_at timestamptz NOT NULL
);

-- An order is one customer's attempt to buy some seats for one event.
CREATE TABLE tk_orders (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id        bigint NOT NULL REFERENCES tk_events,
    customer_id     bigint NOT NULL,
    idempotency_key text   NOT NULL,
    status          text   NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'paid', 'expired', 'refund_due')),
    seat_count      int    NOT NULL CHECK (seat_count BETWEEN 1 AND 6),
    total_cents     bigint NOT NULL CHECK (total_cents >= 0),
    expires_at      timestamptz NOT NULL,
    payment_ref     text UNIQUE,                    -- one payment pays for one order
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (customer_id, idempotency_key)           -- a retried request finds its first attempt
);
-- The sweeper's query: pending orders, oldest expiry first. Paid orders are not in it.
CREATE INDEX tk_orders_to_expire ON tk_orders (expires_at) WHERE status = 'pending';

-- One row per seat per event: the thing being sold. Its state lives here.
CREATE TABLE tk_seats (
    event_id    bigint NOT NULL REFERENCES tk_events,
    seat_no     int    NOT NULL,
    section     int    NOT NULL,
    price_cents int    NOT NULL CHECK (price_cents >= 0),
    status      text   NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'held', 'sold')),
    order_id    bigint REFERENCES tk_orders,
    held_until  timestamptz,
    PRIMARY KEY (event_id, seat_no),
    CHECK ((status = 'available') = (order_id IS NULL)),     -- a taken seat always says who took it
    CHECK ((status = 'held') = (held_until IS NOT NULL))     -- a hold always has an end
);
CREATE INDEX tk_seats_by_section ON tk_seats (event_id, section);                   -- the seat map
CREATE INDEX tk_seats_by_order   ON tk_seats (order_id) WHERE order_id IS NOT NULL; -- an order's seats

-- A ticket is the proof of a sale. The UNIQUE constraint is the last line
-- of defence: whatever the application does, a seat cannot have two.
CREATE TABLE tk_tickets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),   -- unguessable on purpose: it is printed on the ticket
    order_id    bigint NOT NULL REFERENCES tk_orders,
    event_id    bigint NOT NULL,
    seat_no     int    NOT NULL,
    price_cents int    NOT NULL,
    issued_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT one_ticket_per_seat UNIQUE (event_id, seat_no),
    FOREIGN KEY (event_id, seat_no) REFERENCES tk_seats
);
CREATE INDEX tk_tickets_by_order ON tk_tickets (order_id);

-- How many seats each customer holds or owns for each event. The CHECK
-- enforces the limit of six, atomically, however many requests race.
CREATE TABLE tk_customer_limits (
    event_id    bigint NOT NULL REFERENCES tk_events,
    customer_id bigint NOT NULL,
    seats       int    NOT NULL CHECK (seats BETWEEN 0 AND 6),
    PRIMARY KEY (event_id, customer_id)
);`

// ------------------------------------------------------------ the service

var (
	errSeatTaken = errors.New("one or more of those seats is no longer available")
	errLimit     = errors.New("a customer may have at most six seats for an event")
)

type service struct {
	pool      *pgxpool.Pool
	hold      time.Duration // how long a customer has to pay
	naive     bool          // use check-then-act code instead (to show what goes wrong)
	deadlocks atomic.Int64
}

func isCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// inTx runs fn in a transaction, and runs it again if the database chose it
// as the victim of a deadlock or a serialization failure (Chapter 2.4).
func (s *service) inTx(fn func(tx pgx.Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := pgx.BeginFunc(ctx, s.pool, fn)
		if (isCode(err, "40P01") || isCode(err, "40001")) && attempt < 5 {
			s.deadlocks.Add(1)
			time.Sleep(time.Duration(rand.Intn(5*attempt)) * time.Millisecond)
			continue
		}
		return err
	}
}

// holdSeats reserves seats for a customer for a limited time. It is all or
// nothing: either every seat is held, or none is and nothing has changed.
// Calling it again with the same key returns the first call's order.
//
// Locks are always taken in the same order — order, limit, seats — in
// every transaction in this file, so that they cannot deadlock.
func (s *service) holdSeats(event, customer int64, seats []int32, key string) (orderID, total int64, err error) {
	err = s.inTx(func(tx pgx.Tx) error {
		// 1. The order. If this key was used before, this is a retry: return the original.
		err := tx.QueryRow(ctx, `
			INSERT INTO tk_orders (event_id, customer_id, idempotency_key, seat_count, total_cents, expires_at)
			VALUES ($1, $2, $3, $4, 0, now() + $5)
			ON CONFLICT (customer_id, idempotency_key) DO NOTHING
			RETURNING id`, event, customer, key, len(seats), s.hold).Scan(&orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `SELECT id, total_cents FROM tk_orders
				WHERE customer_id = $1 AND idempotency_key = $2`, customer, key).Scan(&orderID, &total)
		}
		if err != nil {
			return err
		}

		// 2. The limit of six. One statement; the CHECK constraint refuses a seventh seat.
		_, err = tx.Exec(ctx, `
			INSERT INTO tk_customer_limits (event_id, customer_id, seats) VALUES ($1, $2, $3)
			ON CONFLICT (event_id, customer_id) DO UPDATE SET seats = tk_customer_limits.seats + EXCLUDED.seats`,
			event, customer, len(seats))
		if isCode(err, "23514") {
			return errLimit
		}
		if err != nil {
			return err
		}

		// 3. The seats.
		var held int
		if s.naive {
			held, total, err = claimNaive(tx, event, seats, orderID, s.hold)
		} else {
			held, total, err = claim(tx, event, seats, orderID, s.hold)
		}
		if err != nil {
			return err
		}
		if held != len(seats) {
			return errSeatTaken // rolling back undoes steps 1 and 2 as well
		}
		_, err = tx.Exec(ctx, `UPDATE tk_orders SET total_cents = $1 WHERE id = $2`, total, orderID)
		return err
	})
	return orderID, total, err
}

// claim takes the seats in one statement. A seat can be taken if it is
// available, or if its hold has run out. Two customers who ask for the
// same seat at the same moment both run this statement; the second waits
// for the first's row lock, re-reads the row, finds it held, and gets
// fewer rows back than it asked for.
func claim(tx pgx.Tx, event int64, seats []int32, orderID int64, hold time.Duration) (held int, total int64, err error) {
	rows, err := tx.Query(ctx, `
		UPDATE tk_seats
		SET    status = 'held', order_id = $1, held_until = now() + $2
		WHERE  event_id = $3 AND seat_no = ANY ($4)
		AND    (status = 'available' OR (status = 'held' AND held_until < now()))
		RETURNING price_cents`, orderID, hold, event, seats)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var price int64
		if err := rows.Scan(&price); err != nil {
			return 0, 0, err
		}
		held++
		total += price
	}
	return held, total, rows.Err()
}

// claimNaive is the version that looks right and is wrong: check, then
// act. Between the SELECT and the UPDATE another customer can do the same.
func claimNaive(tx pgx.Tx, event int64, seats []int32, orderID int64, hold time.Duration) (held int, total int64, err error) {
	err = tx.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(price_cents), 0) FROM tk_seats
		WHERE  event_id = $1 AND seat_no = ANY ($2)
		AND    (status = 'available' OR (status = 'held' AND held_until < now()))`, event, seats).Scan(&held, &total)
	if err != nil || held != len(seats) {
		return held, total, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE tk_seats SET status = 'held', order_id = $1, held_until = now() + $2
		WHERE  event_id = $3 AND seat_no = ANY ($4) AND status <> 'sold'`, orderID, hold, event, seats)
	return held, total, err
}

// confirmPayment records that the payment provider has taken the money for
// an order, and issues the tickets. The provider may tell us twice, or
// late; the result says what happened.
func (s *service) confirmPayment(orderID int64, paymentRef string, naiveSeats []int32) (result string, err error) {
	err = s.inTx(func(tx pgx.Tx) error {
		var event, customer int64
		var seatCount int
		// Only a pending order can be paid, and only once.
		err := tx.QueryRow(ctx, `
			UPDATE tk_orders SET status = 'paid', payment_ref = $2
			WHERE  id = $1 AND status = 'pending'
			RETURNING event_id, customer_id, seat_count`, orderID, paymentRef).Scan(&event, &customer, &seatCount)
		if errors.Is(err, pgx.ErrNoRows) {
			// Not pending. Either we have seen this payment before, or the hold expired first.
			tag, err := tx.Exec(ctx, `UPDATE tk_orders SET status = 'refund_due', payment_ref = $2
				WHERE id = $1 AND status = 'expired'`, orderID, paymentRef)
			if err == nil && tag.RowsAffected() == 1 {
				result = "refund due: the hold had expired"
			} else {
				result = "duplicate confirmation, ignored"
			}
			return err
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM tk_customer_limits
			WHERE event_id = $1 AND customer_id = $2 FOR UPDATE`, event, customer); err != nil {
			return err
		}

		// The seats must still be held by this order.
		var tag pgconn.CommandTag
		if s.naive {
			tag, err = tx.Exec(ctx, `UPDATE tk_seats SET status = 'sold', held_until = NULL, order_id = $1
				WHERE event_id = $2 AND seat_no = ANY ($3)`, orderID, event, naiveSeats)
		} else {
			tag, err = tx.Exec(ctx, `UPDATE tk_seats SET status = 'sold', held_until = NULL
				WHERE order_id = $1 AND status = 'held'`, orderID)
		}
		if err != nil {
			return err
		}
		if int(tag.RowsAffected()) != seatCount {
			// The hold ran out and someone else took a seat. Give back what is left.
			if _, err := tx.Exec(ctx, `UPDATE tk_seats SET status = 'available', order_id = NULL, held_until = NULL
				WHERE order_id = $1`, orderID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tk_customer_limits SET seats = seats - $3
				WHERE event_id = $1 AND customer_id = $2`, event, customer, seatCount); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE tk_orders SET status = 'refund_due' WHERE id = $1`, orderID)
			result = "refund due: a seat had gone"
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO tk_tickets (order_id, event_id, seat_no, price_cents)
			SELECT order_id, event_id, seat_no, price_cents FROM tk_seats WHERE order_id = $1`, orderID)
		result = "paid"
		return err
	})
	if isCode(err, "23505") {
		// The ticket constraint refused a second ticket for a seat: the
		// seat had already been sold. Correct code never gets here; a bug
		// can. The sale has been rolled back. Record that the money is owed
		// back, and put the seats right.
		err = s.inTx(func(tx pgx.Tx) error {
			var event, customer int64
			var seatCount int
			err := tx.QueryRow(ctx, `UPDATE tk_orders SET status = 'refund_due', payment_ref = $2
				WHERE id = $1 AND status = 'pending'
				RETURNING event_id, customer_id, seat_count`, orderID, paymentRef).Scan(&event, &customer, &seatCount)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // another confirmation of the same payment got here first
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tk_customer_limits SET seats = seats - $3
				WHERE event_id = $1 AND customer_id = $2`, event, customer, seatCount); err != nil {
				return err
			}
			// The ticket is the truth. A seat that has a ticket goes back to that ticket's order...
			if _, err := tx.Exec(ctx, `UPDATE tk_seats s SET status = 'sold', order_id = t.order_id, held_until = NULL
				FROM tk_tickets t
				WHERE s.order_id = $1 AND s.status = 'held'
				AND   t.event_id = s.event_id AND t.seat_no = s.seat_no`, orderID); err != nil {
				return err
			}
			// ...and the rest of this order's seats go back on sale.
			_, err = tx.Exec(ctx, `UPDATE tk_seats SET status = 'available', order_id = NULL, held_until = NULL
				WHERE order_id = $1 AND status = 'held'`, orderID)
			return err
		})
		result = "refund due: the seat was already sold"
	}
	return result, err
}

// sweep expires orders whose hold has run out, and puts their seats back
// on sale. Correctness does not depend on it running promptly: a hold that
// has run out stops counting the moment it runs out (see claim). The
// sweeper only tidies up, and releases the customer's allowance.
func (s *service) sweep() (expired int) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM tk_orders
		WHERE status = 'pending' AND expires_at < now() ORDER BY expires_at LIMIT 500`)
	if err != nil {
		log.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		log.Fatal(err)
	}
	for _, id := range ids {
		err := s.inTx(func(tx pgx.Tx) error {
			var event, customer int64
			var seatCount int
			err := tx.QueryRow(ctx, `UPDATE tk_orders SET status = 'expired'
				WHERE id = $1 AND status = 'pending' AND expires_at < now()
				RETURNING event_id, customer_id, seat_count`, id).Scan(&event, &customer, &seatCount)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // it was paid in the meantime
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tk_customer_limits SET seats = seats - $3
				WHERE event_id = $1 AND customer_id = $2`, event, customer, seatCount); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE tk_seats SET status = 'available', order_id = NULL, held_until = NULL
				WHERE order_id = $1 AND status = 'held'`, id)
			expired++
			return err
		})
		if err != nil {
			log.Fatalf("sweep: %v", err)
		}
	}
	return expired
}

// ------------------------------------------------------------ setup

func connect(maxConns int32) *pgxpool.Pool {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = maxConns
	// Chapter 2.6: no session of this application may run a statement for
	// long, wait long for a lock, or sit idle inside a transaction.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5s"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "3s"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10s"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("cannot connect to PostgreSQL (%v). See phase-2-data/README.md.", err)
	}
	return pool
}

func mustExec(pool *pgxpool.Pool, sql string, args ...any) {
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		log.Fatalf("%s: %v", strings.TrimSpace(sql)[:min(60, len(strings.TrimSpace(sql)))], err)
	}
}

// createEvent adds an event with the given number of seats, in sections of
// 500. Seats nearer the front (lower numbers) cost more.
func createEvent(pool *pgxpool.Pool, id int64, seats int) {
	mustExec(pool, `INSERT INTO tk_events VALUES ($1::bigint, 'Event ' || $1::bigint, now() + interval '30 days')`, id)
	mustExec(pool, `INSERT INTO tk_seats (event_id, seat_no, section, price_cents)
		SELECT $1::bigint, g, (g - 1) / 500, 12000 - ((g - 1) / 500) * 500 FROM generate_series(1, $2::int) g`, id, seats)
}

func count(pool *pgxpool.Pool, sql string, args ...any) (n int64) {
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
	return n
}

// ------------------------------------------------------------ demo

func demo() {
	pool := connect(4)
	defer pool.Close()
	mustExec(pool, schema)
	createEvent(pool, 1, 1000)
	s := &service{pool: pool, hold: 500 * time.Millisecond}
	say := func(format string, args ...any) { fmt.Printf("  "+format+"\n", args...) }
	seat := func(n int32) string {
		var status string
		pool.QueryRow(ctx, `SELECT status FROM tk_seats WHERE event_id = 1 AND seat_no = $1`, n).Scan(&status)
		return status
	}

	fmt.Println("1. Ana holds seats 1 and 2")
	ana, total, err := s.holdSeats(1, 100, []int32{1, 2}, "ana-1")
	say("order %d, total $%d, error: %v", ana, total/100, err)

	fmt.Println("2. Ana's app retries the same request (same idempotency key)")
	again, _, err := s.holdSeats(1, 100, []int32{1, 2}, "ana-1")
	say("order %d again, error: %v", again, err)

	fmt.Println("3. Ben asks for seats 2 and 3 — seat 2 is Ana's")
	_, _, err = s.holdSeats(1, 200, []int32{2, 3}, "ben-1")
	say("error: %v", err)
	say("seat 3 is %s: Ben's request took nothing", seat(3))

	fmt.Println("4. Ana pays; the provider confirms twice")
	result, err := s.confirmPayment(ana, "pay_ana_1", nil)
	say("first confirmation:  %s (error: %v)", result, err)
	result, err = s.confirmPayment(ana, "pay_ana_1", nil)
	say("second confirmation: %s (error: %v)", result, err)
	say("tickets issued for the order: %d", count(pool, `SELECT count(*) FROM tk_tickets WHERE order_id = $1`, ana))

	fmt.Println("5. Ana tries for five more seats: that would make seven")
	_, _, err = s.holdSeats(1, 100, []int32{10, 11, 12, 13, 14}, "ana-2")
	say("error: %v", err)

	fmt.Println("6. Cy holds seat 5 and walks away; half a second later Dee asks for it")
	cy, _, _ := s.holdSeats(1, 300, []int32{5}, "cy-1")
	time.Sleep(600 * time.Millisecond)
	dee, _, err := s.holdSeats(1, 400, []int32{5}, "dee-1")
	say("Dee gets order %d, error: %v — before any clean-up job has run", dee, err)

	fmt.Println("7. Cy's payment arrives after all")
	result, err = s.confirmPayment(cy, "pay_cy_1", nil)
	say("%s (error: %v)", result, err)
	result, _ = s.confirmPayment(dee, "pay_dee_1", nil)
	say("Dee pays: %s; seat 5 is %s", result, seat(5))

	fmt.Println("8. Somebody tries to insert a second ticket for seat 1 by hand")
	_, err = pool.Exec(ctx, `INSERT INTO tk_tickets (order_id, event_id, seat_no, price_cents) VALUES ($1, 1, 1, 0)`, dee)
	say("%v", err)

	fmt.Println("9. A bug marks seat 1 — sold to Ana — as available. Eve reserves it and pays")
	mustExec(pool, `UPDATE tk_seats SET status = 'available', order_id = NULL WHERE event_id = 1 AND seat_no = 1`)
	eve, _, err := s.holdSeats(1, 500, []int32{1}, "eve-1")
	say("Eve gets order %d, error: %v — the seat row said the seat was free", eve, err)
	result, err = s.confirmPayment(eve, "pay_eve_1", nil)
	say("Eve pays: %s (error: %v)", result, err)
	say("tickets for seat 1: %d; the seat is %s again, to order %d",
		count(pool, `SELECT count(*) FROM tk_tickets WHERE event_id = 1 AND seat_no = 1`), seat(1),
		count(pool, `SELECT order_id FROM tk_seats WHERE event_id = 1 AND seat_no = 1`))
}

// ------------------------------------------------------------ rush

type tally struct {
	started, taken, overLimit, paid, late, lateRefund, duplicates, abandoned, soldAlready atomic.Int64
	mu                                                                                    sync.Mutex
	holdLatency                                                                           []time.Duration
}

func rush(naive, noConstraint bool) {
	const (
		seats     = 5000
		customers = 12000
		workers   = 64
		hold      = 400 * time.Millisecond
	)
	pool := connect(16)
	defer pool.Close()
	mustExec(pool, schema)
	if noConstraint {
		mustExec(pool, `ALTER TABLE tk_tickets DROP CONSTRAINT one_ticket_per_seat`)
	}
	createEvent(pool, 1, seats)
	s := &service{pool: pool, hold: hold, naive: naive}
	var t tally

	fmt.Printf("ON SALE: %d seats, %d customers, %d at a time. A hold lasts %v; a payment takes 5 to 30 ms.\n",
		seats, customers, workers, hold)
	if naive {
		fmt.Printf("Running the NAIVE hold (check, then act)%s.\n",
			map[bool]string{true: ", and WITHOUT the one-ticket-per-seat constraint", false: ""}[noConstraint])
	}

	stopSweeper := make(chan struct{})
	var sweeperDone sync.WaitGroup
	sweeperDone.Add(1)
	go func() {
		defer sweeperDone.Done()
		for {
			select {
			case <-stopSweeper:
				return
			case <-time.After(100 * time.Millisecond):
				s.sweep()
			}
		}
	}()

	queue := make(chan int64)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for customer := range queue {
				shop(s, &t, rng, customer, seats)
			}
		}(w)
	}
	for c := int64(1); c <= customers; c++ {
		queue <- 1 + c%10000 // one customer in six comes back for more
	}
	close(queue)
	wg.Wait()
	elapsed := time.Since(start)
	time.Sleep(hold + 200*time.Millisecond) // let the last holds run out
	close(stopSweeper)
	sweeperDone.Wait()
	s.sweep()

	sort.Slice(t.holdLatency, func(i, j int) bool { return t.holdLatency[i] < t.holdLatency[j] })
	pct := func(p float64) time.Duration { return t.holdLatency[int(p*float64(len(t.holdLatency)-1))] }
	line := func(label string, n int64) { fmt.Printf("  %-52s %7d\n", label, n) }
	fmt.Printf("\nWHAT HAPPENED  (%.1f s)\n", elapsed.Seconds())
	line("attempts to hold seats", t.started.Load())
	line("  refused: a seat was already taken", t.taken.Load())
	line("  refused: more than six seats for one customer", t.overLimit.Load())
	line("orders paid, tickets issued", t.paid.Load())
	line("  of which paid after the hold ran out, seats kept", t.late.Load())
	line("holds abandoned, expired and released", t.abandoned.Load())
	line("paid too late, seats gone: refund due", t.lateRefund.Load())
	line("paid, but the seat was already sold: refund due", t.soldAlready.Load())
	line("duplicate payment confirmations ignored", t.duplicates.Load())
	line("deadlocks retried", s.deadlocks.Load())
	fmt.Printf("  %-52s p50 %.1f ms, p99 %.1f ms\n", "time to hold seats",
		float64(pct(0.50).Microseconds())/1000, float64(pct(0.99).Microseconds())/1000)

	fmt.Println("\nCHECKS")
	line("seats sold (of 5,000)", count(pool, `SELECT count(*) FROM tk_seats WHERE status = 'sold'`))
	line("seats with more than one ticket", count(pool, `SELECT count(*) FROM
		(SELECT 1 FROM tk_tickets GROUP BY event_id, seat_no HAVING count(*) > 1) d`))
	line("tickets that do not belong to a paid order", count(pool, `SELECT count(*) FROM tk_tickets t
		JOIN tk_orders o ON o.id = t.order_id WHERE o.status <> 'paid'`))
	line("paid orders without exactly their tickets", count(pool, `SELECT count(*) FROM tk_orders o
		WHERE o.status = 'paid' AND o.seat_count <> (SELECT count(*) FROM tk_tickets t WHERE t.order_id = o.id)`))
	line("sold seats without a ticket", count(pool, `SELECT count(*) FROM tk_seats s WHERE s.status = 'sold'
		AND NOT EXISTS (SELECT 1 FROM tk_tickets t WHERE t.event_id = s.event_id AND t.seat_no = s.seat_no AND t.order_id = s.order_id)`))
	line("seats still held after every hold has run out", count(pool, `SELECT count(*) FROM tk_seats WHERE status = 'held'`))
	line("customers with more than six seats", count(pool, `SELECT count(*) FROM
		(SELECT 1 FROM tk_tickets t JOIN tk_orders o ON o.id = t.order_id GROUP BY o.event_id, o.customer_id HAVING count(*) > 6) c`))
	line("customers whose allowance does not match their tickets", count(pool, `SELECT count(*) FROM tk_customer_limits l
		WHERE l.seats <> (SELECT count(*) FROM tk_tickets t JOIN tk_orders o ON o.id = t.order_id
		                  WHERE o.event_id = l.event_id AND o.customer_id = l.customer_id)`))
	paid := count(pool, `SELECT coalesce(sum(total_cents), 0) FROM tk_orders WHERE status = 'paid'`)
	issued := count(pool, `SELECT coalesce(sum(price_cents), 0) FROM tk_tickets`)
	fmt.Printf("  %-52s $%d against $%d", "money: paid orders against tickets issued", paid/100, issued/100)
	if paid == issued {
		fmt.Println("  (equal)")
	} else {
		fmt.Println("  (DIFFERENT)")
	}
}

// shop is one customer: pick some neighbouring seats, try to hold them,
// and then pay, pay late, or walk away.
func shop(s *service, t *tally, rng *rand.Rand, customer int64, seats int) {
	var order int64
	var wanted []int32
	for attempt := 0; attempt < 3 && order == 0; attempt++ {
		n := 1 + rng.Intn(4)
		first := 1 + rng.Intn(seats-n)
		wanted = wanted[:0]
		for i := 0; i < n; i++ {
			wanted = append(wanted, int32(first+i))
		}
		t.started.Add(1)
		begin := time.Now()
		id, _, err := s.holdSeats(1, customer, wanted, fmt.Sprintf("c%d-%d", customer, rng.Int63()))
		took := time.Since(begin)
		t.mu.Lock()
		t.holdLatency = append(t.holdLatency, took)
		t.mu.Unlock()
		switch {
		case err == nil:
			order = id
		case errors.Is(err, errSeatTaken):
			t.taken.Add(1)
		case errors.Is(err, errLimit):
			t.overLimit.Add(1)
			return
		default:
			log.Fatalf("hold: %v", err)
		}
	}
	if order == 0 {
		return
	}

	paymentRef := fmt.Sprintf("pay_%d", order)
	confirm := func() string {
		result, err := s.confirmPayment(order, paymentRef, wanted)
		if err != nil {
			log.Fatalf("confirm: %v", err)
		}
		return result
	}
	record := func(result string, wasLate bool) {
		switch {
		case result == "paid":
			t.paid.Add(1)
			if wasLate {
				t.late.Add(1)
			}
		case strings.HasPrefix(result, "duplicate"):
			t.duplicates.Add(1)
		case strings.Contains(result, "already sold"):
			t.soldAlready.Add(1)
		default:
			t.lateRefund.Add(1)
		}
	}

	switch r := rng.Intn(100); {
	case r < 70: // pays promptly. The provider call happens outside any transaction.
		time.Sleep(time.Duration(5+rng.Intn(25)) * time.Millisecond)
		if rng.Intn(10) == 0 { // one confirmation in ten arrives twice, at the same moment
			var both sync.WaitGroup
			results := make([]string, 2)
			for i := range results {
				both.Add(1)
				go func(i int) { defer both.Done(); results[i] = confirm() }(i)
			}
			both.Wait()
			record(results[0], false)
			record(results[1], false)
		} else {
			record(confirm(), false)
		}
	case r < 85: // pays, but slowly: around the time the hold runs out
		wait := s.hold - 50*time.Millisecond + time.Duration(rng.Intn(300))*time.Millisecond
		time.Sleep(wait)
		record(confirm(), wait > s.hold)
	default: // walks away
		t.abandoned.Add(1)
	}
}

// ------------------------------------------------------------ plans

func plans() {
	pool := connect(2)
	defer pool.Close()
	mustExec(pool, schema)
	fmt.Println("Loading 100 events of 5,000 seats each (500,000 seat rows), and 60,000 orders...")
	mustExec(pool, `INSERT INTO tk_events SELECT g, 'Event ' || g, now() + interval '30 days' FROM generate_series(1, 100) g`)
	mustExec(pool, `INSERT INTO tk_seats (event_id, seat_no, section, price_cents)
		SELECT e, g, (g - 1) / 500, 9000 FROM generate_series(1, 100) e, generate_series(1, 5000) g`)
	mustExec(pool, `INSERT INTO tk_orders (event_id, customer_id, idempotency_key, status, seat_count, total_cents, expires_at)
		SELECT 1 + g % 100, g, 'k' || g, CASE WHEN g % 200 = 0 THEN 'pending' ELSE 'paid' END, 2, 18000,
		       now() - interval '1 hour' + g * interval '1 second'
		FROM generate_series(1, 60000) g`)
	mustExec(pool, `VACUUM ANALYZE tk_seats`)
	mustExec(pool, `VACUUM ANALYZE tk_orders`)

	explain := func(title, sql string) {
		fmt.Printf("\n%s\n", title)
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+sql)
		if err != nil {
			log.Fatal(err)
		}
		lines, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, l := range lines {
			fmt.Println("  " + l)
		}
	}
	explain("The seat map of one section (500 of 500,000 rows)",
		`SELECT seat_no, CASE WHEN status = 'held' AND held_until < now() THEN 'available' ELSE status END
		 FROM tk_seats WHERE event_id = 42 AND section = 3`)
	explain("Claiming three seats",
		`UPDATE tk_seats SET status = 'held', order_id = 1, held_until = now() + interval '10 minutes'
		 WHERE event_id = 42 AND seat_no = ANY ('{1501,1502,1503}')
		 AND (status = 'available' OR (status = 'held' AND held_until < now()))`)
	explain("The sweeper: pending orders whose hold has run out",
		`SELECT id FROM tk_orders WHERE status = 'pending' AND expires_at < now() ORDER BY expires_at LIMIT 500`)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: capstone-tickets demo | rush [-naive] [-no-constraint] | plans")
		os.Exit(2)
	}
	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	naive := flags.Bool("naive", false, "hold seats with check-then-act code")
	noConstraint := flags.Bool("no-constraint", false, "drop the one-ticket-per-seat constraint first")
	flags.Parse(os.Args[2:])
	switch os.Args[1] {
	case "demo":
		demo()
	case "rush":
		rush(*naive, *noConstraint)
	case "plans":
		plans()
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", os.Args[1])
		os.Exit(2)
	}
	pool := connect(1)
	mustExec(pool, `DROP TABLE IF EXISTS tk_tickets, tk_seats, tk_customer_limits, tk_orders, tk_events`)
	pool.Close()
}
