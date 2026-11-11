# Phase 2 — Data: correct and fast on one node

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Foundations](../phase-1-foundations/README.md)

Phase 1 ended with a list of what breaks first, and most of the list was about data. Before spreading data across many machines (Phases 3 and 5), you need to know what one database does: how it stores and finds rows, what a transaction really guarantees, how to change a schema while the system is running, and how to get your data back after a disaster.

**After this phase you can** explain why a query is slow and fix it, pick an isolation level on purpose, prevent lost updates and double-bookings, change a schema while the system is serving traffic, and restore from backup with confidence.

> **Status: in progress.** Chapters 2.1 to 2.8 are written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-2--data-correct-and-fast-on-one-node).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 2.1 | [How a database stores and finds data](01-how-databases-store-data.md) | Tuning by superstition because the database is a black box. | [`ch01-storage`](code/ch01-storage/storage.sql) — pages, a B-tree's levels, the cost of a commit |
| 2.2 | [Indexes and query plans](02-indexes-and-query-plans.md) | A query that is instant in staging and takes half a second for your biggest customer. | [`ch02-indexes`](code/ch02-indexes/indexes.sql) — eleven experiments on a skewed table; [`ch02-nplus1`](code/ch02-nplus1/main.go) — 501 round trips or one |
| 2.3 | [Transactions and isolation levels](03-transactions-and-isolation.md) | Two requests each read a balance of 100, each withdraw 80, and both succeed. | [`ch03-isolation`](code/ch03-isolation/main.go) — two real sessions, five experiments |
| 2.4 | [Concurrency control in application code](04-concurrency-control.md) | "Check then act" code that is right in every test and wrong under load. | [`ch04-concurrency`](code/ch04-concurrency/main.go) — five ways to sell the last items, and a deadlock; [`ch04-coupon`](code/ch04-coupon/main.go) — the exercise, tested under load |
| 2.5 | [Data modelling: keys, constraints, and denormalisation](05-data-modelling.md) | A key that runs out after two billion rows; a schema that lets bad data in. | [`ch05-modelling`](code/ch05-modelling/modelling.sql) — types, constraints, foreign-key indexes, key order; [`review_solution.sql`](code/ch05-modelling/review_solution.sql) |
| 2.6 | [The database as a shared resource](06-database-as-shared-resource.md) | One slow report takes the whole product down. | [`ch06-shared`](code/ch06-shared/main.go) — the connection limit, three timeouts, the lock queue, a starved pool |
| 2.7 | [Changing schemas without downtime](07-schema-changes.md) | `ALTER TABLE` takes a lock and the site is down for six minutes. | [`ch07-locks`](code/ch07-locks/main.go) — what each schema change blocks, measured; [`ch07-expand-contract`](code/ch07-expand-contract/main.go) — renaming a column under traffic, three ways |
| 2.8 | [Choosing a datastore](08-choosing-a-datastore.md) | A store chosen for "flexibility" that now needs joins and transactions. | [`ch08-layouts`](code/ch08-layouts/main.go) — the same data row by row and column by column; [`ch08-search`](code/ch08-search/search.sql) — a text search with and without an inverted index |
| 2.9 | Durability, backups, and restore | The backups existed; nobody had ever restored one. | *planned* |
| C2 | Capstone: ticket booking — never sell the same seat twice | — | *planned* |

## What you need

**PostgreSQL**, version 12 or newer. The examples in this phase are experiments on a real database: you run a script and read what the database tells you about itself.

The quickest way to get one is Docker:

```bash
docker run --name course-pg -e POSTGRES_PASSWORD=postgres -p 5432:5432 -d postgres:16

# tell psql where it is (once per terminal)
export PGHOST=localhost PGUSER=postgres PGPASSWORD=postgres PGDATABASE=postgres
```

Then every example runs with one command, given in its chapter:

```bash
psql -f phase-2-data/code/ch01-storage/storage.sql
```

Some examples are Go programs that talk to the same database. They read its address from `DATABASE_URL` and default to the Docker container above:

```bash
go run ./phase-2-data/code/ch02-nplus1
```

These use one library from outside Go's standard library, the [`pgx`](https://pkg.go.dev/github.com/jackc/pgx/v5) PostgreSQL driver. `go run` downloads it the first time.

The scripts create the tables they need and drop them when they finish. Use a scratch database, not one you care about.

Outputs printed in the chapters come from PostgreSQL 16. Row counts, page counts and sizes should match yours exactly; timings will not.
