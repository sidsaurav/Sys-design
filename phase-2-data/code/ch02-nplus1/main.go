// Chapter 2.2 — the N+1 query problem: the same data fetched with 501
// round trips, with 2, and with 1.
//
// Run (needs PostgreSQL; see phase-2-data/README.md):
//
//	go run ./phase-2-data/code/ch02-nplus1
//	go run ./phase-2-data/code/ch02-nplus1 -latency 500us
//
// The task is "show the 500 most recent orders with the customer's name".
// With -latency, every round trip to the database is delayed by that much,
// to stand in for a database on another machine. (Chapter 1.1: a round trip
// inside one datacenter is about half a millisecond.)
//
// The connection string comes from DATABASE_URL and defaults to
// postgres://postgres:postgres@localhost:5432/postgres.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

type order struct {
	ID         int64
	CustomerID int64
	TotalCents int64
	Customer   string
}

// db counts the round trips made through it and can delay each one.
type db struct {
	conn       *pgx.Conn
	latency    time.Duration
	roundTrips int
}

func (d *db) query(ctx context.Context, sql string, args ...any) pgx.Rows {
	d.roundTrips++
	time.Sleep(d.latency)
	rows, err := d.conn.Query(ctx, sql, args...)
	if err != nil {
		log.Fatal(err)
	}
	return rows
}

const recentOrders = `SELECT id, customer_id, total_cents FROM np1_orders ORDER BY id DESC LIMIT 500`

// nPlusOne is what an object-relational mapper does by default when code
// loops over orders and touches order.Customer.Name: one query for the
// list, then one more query per row.
func nPlusOne(ctx context.Context, d *db) []order {
	var orders []order
	rows := d.query(ctx, recentOrders)
	for rows.Next() {
		var o order
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.TotalCents); err != nil {
			log.Fatal(err)
		}
		orders = append(orders, o)
	}
	rows.Close()

	for i := range orders {
		r := d.query(ctx, `SELECT name FROM np1_customers WHERE id = $1`, orders[i].CustomerID)
		if r.Next() {
			r.Scan(&orders[i].Customer)
		}
		r.Close()
	}
	return orders
}

// twoQueries fetches the list, then all the customers it mentions at once.
// This is what "eager loading" in an ORM generates.
func twoQueries(ctx context.Context, d *db) []order {
	var orders []order
	var ids []int64
	rows := d.query(ctx, recentOrders)
	for rows.Next() {
		var o order
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.TotalCents); err != nil {
			log.Fatal(err)
		}
		orders = append(orders, o)
		ids = append(ids, o.CustomerID)
	}
	rows.Close()

	names := map[int64]string{}
	r := d.query(ctx, `SELECT id, name FROM np1_customers WHERE id = ANY($1)`, ids)
	for r.Next() {
		var id int64
		var name string
		r.Scan(&id, &name)
		names[id] = name
	}
	r.Close()
	for i := range orders {
		orders[i].Customer = names[orders[i].CustomerID]
	}
	return orders
}

// join lets the database do the matching, in one statement.
func join(ctx context.Context, d *db) []order {
	var orders []order
	rows := d.query(ctx, `
		SELECT o.id, o.customer_id, o.total_cents, c.name
		FROM np1_orders o JOIN np1_customers c ON c.id = o.customer_id
		ORDER BY o.id DESC LIMIT 500`)
	for rows.Next() {
		var o order
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.TotalCents, &o.Customer); err != nil {
			log.Fatal(err)
		}
		orders = append(orders, o)
	}
	rows.Close()
	return orders
}

func main() {
	latency := flag.Duration("latency", 0, "extra delay added to every round trip to the database")
	flag.Parse()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatalf("cannot connect to PostgreSQL (%v). See phase-2-data/README.md.", err)
	}
	defer conn.Close(ctx)

	setup := []string{
		`DROP TABLE IF EXISTS np1_orders, np1_customers`,
		`CREATE TABLE np1_customers (id bigint PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE np1_orders (id bigint PRIMARY KEY, customer_id bigint NOT NULL REFERENCES np1_customers, total_cents bigint NOT NULL)`,
		`INSERT INTO np1_customers SELECT g, 'Customer ' || g FROM generate_series(1, 10000) g`,
		`INSERT INTO np1_orders SELECT g, 1 + (g * 7919) % 10000, (g * 31) % 50000 FROM generate_series(1, 100000) g`,
		`ANALYZE np1_customers`, `ANALYZE np1_orders`,
	}
	for _, stmt := range setup {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			log.Fatal(err)
		}
	}
	defer conn.Exec(ctx, `DROP TABLE IF EXISTS np1_orders, np1_customers`)

	fmt.Printf("Fetch the 500 most recent orders with customer names. Extra delay per round trip: %v\n\n", *latency)
	fmt.Println("  approach                         round trips      time")
	for _, a := range []struct {
		name string
		run  func(context.Context, *db) []order
	}{
		{"N+1: one query per order", nPlusOne},
		{"two queries (id = ANY(...))", twoQueries},
		{"one query with a JOIN", join},
	} {
		d := &db{conn: conn, latency: *latency}
		a.run(ctx, d) // once to warm the cache, so we time the queries and not the disk
		d.roundTrips = 0
		start := time.Now()
		orders := a.run(ctx, d)
		took := time.Since(start)
		if len(orders) != 500 || orders[0].Customer == "" {
			log.Fatalf("%s returned the wrong result", a.name)
		}
		fmt.Printf("  %-32s %6d      %8.1f ms\n", a.name, d.roundTrips, float64(took.Microseconds())/1000)
	}
}
