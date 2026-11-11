# Phase 2 — Data: correct and fast on one node

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Foundations](../phase-1-foundations/README.md)

Phase 1 ended with a list of what breaks first, and most of the list was about data. Before spreading data across many machines (Phases 3 and 5), you need to know what one database does: how it stores and finds rows, what a transaction really guarantees, how to change a schema while the system is running, and how to get your data back after a disaster.

**After this phase you can** explain why a query is slow and fix it, pick an isolation level on purpose, prevent lost updates and double-bookings, change a schema while the system is serving traffic, and restore from backup with confidence.

> **Status: in progress.** Chapter 2.1 is written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-2--data-correct-and-fast-on-one-node).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 2.1 | [How a database stores and finds data](01-how-databases-store-data.md) | Tuning by superstition because the database is a black box. | [`ch01-storage`](code/ch01-storage/storage.sql) — pages, a B-tree's levels, the cost of a commit |
| 2.2 | Indexes and query plans | A query takes 30 ms in staging and 30 s in production. | *planned* |
| 2.3 | Transactions and isolation levels | Two requests each read a balance of 100, each withdraw 80, and both succeed. | *planned* |
| 2.4 | Concurrency control in application code | "Check then act" code that is right in every test and wrong under load. | *planned* |
| 2.5 | Data modelling: keys, constraints, and denormalisation | A schema that lets bad data in, or makes every feature a migration. | *planned* |
| 2.6 | The database as a shared resource | One slow report takes the whole product down. | *planned* |
| 2.7 | Changing schemas without downtime | `ALTER TABLE` takes a lock and the site is down for six minutes. | *planned* |
| 2.8 | Choosing a datastore | A store chosen for "flexibility" that now needs joins and transactions. | *planned* |
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

The scripts create the tables they need and drop them when they finish. Use a scratch database, not one you care about.

Outputs printed in the chapters come from PostgreSQL 16. Row counts, page counts and sizes should match yours exactly; timings will not.
