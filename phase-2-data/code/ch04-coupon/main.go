// Chapter 2.4, exercise — a coupon that may be redeemed 100 times in total
// and once per user, under heavy concurrency.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/ch04-coupon
//
// 40 connections make 1,000 redemption attempts on behalf of 300 users,
// so the same user often tries more than once and most attempts arrive
// after the coupon has run out. The program then checks both rules against
// what is in the database.
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

	"github.com/jackc/pgx/v5"
)

var ctx = context.Background()

var (
	errAlreadyUsed = errors.New("this user has already used the coupon")
	errRunOut      = errors.New("the coupon has run out")
)

// redeem applies the coupon for one user. Each statement does its own
// check, and the transaction makes the two changes all-or-nothing.
func redeem(conn *pgx.Conn, coupon string, user int) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // undoes the redemption row if we return early

	// Rule 2: once per user. Insert without checking; the primary key decides.
	tag, err := tx.Exec(ctx,
		`INSERT INTO cp_redemptions (coupon, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, coupon, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errAlreadyUsed
	}

	// Rule 1: at most max_uses in total. Check and change in one statement.
	tag, err = tx.Exec(ctx,
		`UPDATE cp_coupons SET used = used + 1 WHERE code = $1 AND used < max_uses`, coupon)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRunOut
	}
	return tx.Commit(ctx)
}

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

func main() {
	admin := connect()
	defer admin.Close(ctx)
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS cp_redemptions, cp_coupons`,
		`CREATE TABLE cp_coupons (
		     code     text PRIMARY KEY,
		     max_uses int  NOT NULL,
		     used     int  NOT NULL DEFAULT 0,
		     CHECK (used <= max_uses))`,
		`CREATE TABLE cp_redemptions (
		     coupon  text   NOT NULL REFERENCES cp_coupons,
		     user_id bigint NOT NULL,
		     PRIMARY KEY (coupon, user_id))`,
		`INSERT INTO cp_coupons (code, max_uses) VALUES ('SPRING', 100)`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			log.Fatal(err)
		}
	}

	const workers, attemptsEach, users = 40, 25, 300
	var applied, alreadyUsed, runOut, failed atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		conn := connect()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close(ctx)
			for i := range attemptsEach {
				user := (w*attemptsEach + i) % users
				switch err := redeem(conn, "SPRING", user); {
				case err == nil:
					applied.Add(1)
				case errors.Is(err, errAlreadyUsed):
					alreadyUsed.Add(1)
				case errors.Is(err, errRunOut):
					runOut.Add(1)
				default:
					failed.Add(1)
					log.Printf("unexpected error: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	var used, rows, distinctUsers int
	admin.QueryRow(ctx, `SELECT used FROM cp_coupons WHERE code = 'SPRING'`).Scan(&used)
	admin.QueryRow(ctx, `SELECT count(*), count(DISTINCT user_id) FROM cp_redemptions`).Scan(&rows, &distinctUsers)

	fmt.Printf("%d attempts by %d users over %d connections, for a coupon with 100 uses\n\n", workers*attemptsEach, users, workers)
	fmt.Printf("  told \"coupon applied\":        %4d\n", applied.Load())
	fmt.Printf("  told \"already used\":          %4d\n", alreadyUsed.Load())
	fmt.Printf("  told \"run out\":               %4d\n", runOut.Load())
	fmt.Printf("  unexpected errors:            %4d\n\n", failed.Load())
	fmt.Printf("  in the database: counter = %d, redemption rows = %d, distinct users = %d\n", used, rows, distinctUsers)
	if used == 100 && rows == 100 && distinctUsers == 100 && applied.Load() == 100 {
		fmt.Println("  both rules hold")
	} else {
		fmt.Println("  A RULE WAS BROKEN")
	}
	admin.Exec(ctx, `DROP TABLE IF EXISTS cp_redemptions, cp_coupons`)
}
