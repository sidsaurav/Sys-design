# 2.2 Indexes and query plans

[Syllabus](../SYLLABUS.md) · Phase 2 · Previous: [2.1 How a database stores and finds data](01-how-databases-store-data.md) · Next: [2.3 Transactions and isolation levels](03-transactions-and-isolation.md)

> **Time:** about 55 minutes to read, 75 minutes of practice.
> **You need:** [Chapter 2.1](01-how-databases-store-data.md) (pages, B-trees, "count pages, not milliseconds"), and a PostgreSQL to run the examples on — see [the phase overview](README.md).

**By the end of this chapter you can**

- read an `EXPLAIN` plan: which rows were touched, how, and where the planner's guess was wrong;
- design a multi-column index from a query — which columns, in what order — and say which queries it can and cannot serve;
- explain why the database sometimes ignores an index, and why it is usually right to;
- use partial, covering and expression indexes where they fit;
- recognise the N+1 query pattern and remove it;
- weigh what each index costs on every write.

---

## The problem

*An illustrative scenario, with real measurements you will reproduce.* A shop's "my orders" page shows a customer's twenty most recent orders. The developer adds an index on `customer_id`. In staging, where every test customer has a few dozen orders, the page takes a quarter of a millisecond. It ships.

In production, most customers see the same quarter-millisecond. But the shop's largest customer — a reseller with a million orders — waits half a second, on a query that returns twenty rows. During their morning rush the database's CPU is pinned and everyone's pages slow down.

Same query. Same index. A difference of two thousand times, and it depends only on *whose* orders are asked for.

Nothing was missing in the usual sense: there *was* an index, and the database used it. The index was simply an answer to a different question from the one the query asks. This chapter is about seeing that difference before production shows it to you. The tool is the query plan.

---

## The concept

### 1. The planner, and how to read its plan

SQL says *what* you want. The database's **planner** decides *how* to get it: which index to use, if any; in what order to join tables; whether to sort. It considers the alternatives, estimates what each would cost, and picks the cheapest.

`EXPLAIN` shows the plan it picked, without running the query:

```text
 Limit  (cost=104129.80..104129.85 rows=20 width=33)
   ->  Sort  (cost=104129.80..104129.98 rows=71 width=33)
         Sort Key: created_at DESC
         ->  Seq Scan on orders  (cost=0.00..104127.91 rows=71 width=33)
               Filter: (customer_id = 4242)
```

How to read it:

- **It is a tree, and it runs from the inside out.** "The structure of a query plan is a tree of plan nodes. Nodes at the bottom level of the tree are scan nodes: they return raw rows from a table" ([PostgreSQL docs, *Using EXPLAIN*](https://www.postgresql.org/docs/current/using-explain.html)). Here: scan the whole table keeping rows for customer 4242; sort them by date; keep the first twenty.
- **`cost` is the planner's estimate, in its own units** — by convention, "units of disk page fetches". It is for comparing plans, not for predicting milliseconds.
- **`rows` is how many rows the planner *expects* that step to produce.** It expects 71 orders for this customer.

`EXPLAIN ANALYZE` runs the query and adds what really happened:

```text
         ->  Seq Scan on orders (actual time=2.947..277.335 rows=48 loops=1)
               Filter: (customer_id = 4242)
               Rows Removed by Filter: 4999952
               Buffers: shared hit=12322 read=29306
 Execution Time: 277.448 ms
```

Three things to look at, in this order:

1. **How many rows did each step read, compared with how many it returned?** Here: five million read, 48 kept. A step that reads far more than it returns is where the time goes.
2. **How many pages?** (`Buffers`, from Chapter 2.1.) 41,628 — the whole table.
3. **Was the planner's guess close?** It expected 71 rows and got 48: close enough. The [documentation](https://www.postgresql.org/docs/current/using-explain.html) calls this the most important check: "whether the estimated row counts are reasonably close to reality." When the guess is wrong by orders of magnitude, the planner has chosen its plan for a situation that does not exist — see section 6.

### 2. An index must match the *whole* question

The query in this chapter's scenario is:

```sql
SELECT * FROM orders
WHERE customer_id = $1
ORDER BY created_at DESC
LIMIT 20;
```

It asks three things: *which rows* (this customer's), *in what order* (newest first), and *how many* (twenty).

**An index on `(customer_id)`** answers only the first. The database uses it to find all of the customer's orders, fetches every one of them from the table, sorts them, and throws away all but twenty. For a customer with 48 orders, that is nothing. For the customer with a million:

```text
 Limit (actual time=443.003..443.008 rows=20 loops=1)
   ->  Sort (actual time=443.002..443.004 rows=20 loops=1)
         Sort Key: created_at DESC
         ->  Bitmap Heap Scan on orders (actual time=30.735..305.723 rows=1001457 loops=1)
               Recheck Cond: (customer_id = 1)
               Buffers: shared hit=9 read=42464
               ->  Bitmap Index Scan on orders_customer (rows=1001457 loops=1)
 Execution Time: 443.030 ms
```

One million rows fetched, to return twenty. The cost of this plan is proportional to *how many orders the customer has* — which is why it passed every test and failed on the biggest customer.

**An index on `(customer_id, created_at DESC)`** answers all three. Within the index, each customer's entries sit together, already in date order. The database goes to the start of the customer's entries and reads the first twenty:

```text
 Limit (actual time=0.023..0.029 rows=20 loops=1)
   ->  Index Scan using orders_customer_newest on orders (actual time=0.022..0.026 rows=20 loops=1)
         Index Cond: (customer_id = 1)
         Buffers: shared hit=2 read=3
 Execution Time: 0.044 ms
```

No `Sort` node. Twenty rows read. Five pages. And the cost no longer depends on the customer: it is the same for 48 orders or a million.

PostgreSQL's documentation singles out exactly this case: "an explicit sort will have to process all the data to identify the first n rows, but if there is an index matching the ORDER BY, the first n rows can be retrieved directly, without scanning the remainder at all" ([*Indexes and ORDER BY*](https://www.postgresql.org/docs/current/indexes-ordering.html)).

### 3. Column order in a multi-column index

A multi-column index is sorted by its first column, then by the second within equal values of the first, and so on — like a phone book sorted by surname, then first name. A phone book is useful for finding all the Smiths, or Smith, Anna. It is useless for finding everyone called Anna.

The rule in PostgreSQL's words: a multicolumn B-tree "is most efficient when there are constraints on the leading (leftmost) columns. The exact rule is that equality constraints on leading columns, plus any inequality constraints on the first column that does not have an equality constraint, will always be used to limit the portion of the index that is scanned" ([*Multicolumn Indexes*](https://www.postgresql.org/docs/current/indexes-multicolumn.html)).

So the index `(customer_id, created_at DESC)`:

| Query | Can the index narrow the search? |
|---|---|
| `WHERE customer_id = 7` | Yes — leading column |
| `WHERE customer_id = 7 AND created_at > '2024-06-01'` | Yes — equality on the first, range on the second |
| `WHERE customer_id = 7 ORDER BY created_at DESC LIMIT 20` | Yes — and no sort is needed |
| `WHERE created_at > '2024-06-01'` | **Not usefully** — it skips the leading column |

The last row, measured: a ten-minute time range, with only the composite index available, falls back to scanning the whole table — 343 ms for 600 rows.

*A refinement for recent versions.* PostgreSQL 18 added a *skip scan*, which can use a multi-column index without a condition on its leading column by searching once for each value of that column. It pays only when the skipped column has very few distinct values; otherwise, the documentation says, "the entire index will have to be scanned, so in most cases the planner will prefer a sequential table scan" ([same page](https://www.postgresql.org/docs/current/indexes-multicolumn.html)). With 100,000 customers, that is the case here on any version. Treat skip scan as a bonus, and design the index for the query.

**A recipe for designing an index from a query:**

1. Columns compared with `=` go first.
2. Then the column you sort by, or compare with a range (`<`, `>`, `BETWEEN`).
3. Stop there, unless you want a covering index (section 5).

And one index can serve several queries: `(customer_id, created_at)` makes a separate index on `(customer_id)` redundant, because `customer_id` is its leading column.

### 4. Selectivity: why the planner ignores your index

An index lookup is not free. For each matching entry the database follows a pointer to a table page, and matching rows are usually scattered all over the table. Fetching a few rows that way is cheap. Fetching most of the table that way is *slower* than just reading the table from start to finish.

So the planner uses an index only when the condition is **selective** — when it picks out a small fraction of the rows. With an index on `status`, where 1% of orders are `pending` and 96% are `delivered`:

```text
-- WHERE status = 'pending'    (1% of rows)
   ->  Index Scan using orders_status on orders (actual time=0.024..116.258 rows=49952 loops=1)
 Execution Time: 123.653 ms

-- WHERE status = 'delivered'  (96% of rows)
   ->  Seq Scan on orders (actual time=0.012..537.263 rows=4799737 loops=1)
         Rows Removed by Filter: 200263
 Execution Time: 859.416 ms
```

For the common value the planner did not use the index, and it was right not to. "The planner didn't use my index" is very often the planner being correct. An index on a column with only a few distinct values — a status, a boolean, a country — helps only for the rare values.

Which leads to a better tool for this case. If you only ever search for the rare rows, index *only* those. A **partial index** is "an index built over a subset of a table" ([PostgreSQL docs, *Partial Indexes*](https://www.postgresql.org/docs/current/indexes-partial.html)):

```sql
CREATE INDEX orders_pending ON orders (created_at) WHERE status = 'pending';
```

Measured: this index is **1.1 MB**, against 34 MB for the index on all of `status`, and "the twenty oldest pending orders" takes 0.05 ms. It is also far cheaper to maintain, because 99% of inserts and updates do not touch it.

### 5. Two more kinds of index, and one way to defeat any of them

**Covering indexes.** If an index contains every column a query needs, the database can answer from the index alone, "without any heap access" — an **index-only scan** ([PostgreSQL docs, *Index-Only Scans and Covering Indexes*](https://www.postgresql.org/docs/current/indexes-index-only-scans.html)). `INCLUDE` adds columns to an index for this purpose without making them part of the sort order:

```sql
CREATE INDEX orders_customer_total ON orders (customer_id) INCLUDE (total_cents);
-- SELECT sum(total_cents) FROM orders WHERE customer_id = 4242;   → Index Only Scan, Heap Fetches: 0
```

It is a real saving when a query reads many rows, and it costs a larger index. One caveat: PostgreSQL must still check that each row is visible to your transaction, and it can skip the table only for pages that vacuum has marked all-visible. On a table with heavy recent writes, an index-only scan gains less.

**Expression indexes.** An index can be built on a computed value: "an index column need not be just a column of the underlying table, but can be a function or scalar expression" ([PostgreSQL docs, *Indexes on Expressions*](https://www.postgresql.org/docs/current/indexes-expressional.html)). The usual example is case-insensitive lookup: `CREATE INDEX ON users (lower(email))` serves `WHERE lower(email) = 'ada@example.com'`.

**How to defeat an index: wrap the column in a function.** An index on `created_at` is sorted by `created_at`. It is not sorted by `created_at::date`, or `date_trunc('day', created_at)`, or `created_at + interval '1 day'`. Put the column inside an expression and — unless there is an index on exactly that expression — the database is back to scanning:

```text
-- WHERE created_at::date = '2024-02-01'
   ->  Seq Scan on orders (rows=86400)        Rows Removed by Filter: 4913600
 Execution Time: 535.553 ms

-- WHERE created_at >= '2024-02-01' AND created_at < '2024-02-02'
   ->  Index Only Scan using orders_created on orders (rows=86400)
 Execution Time: 14.528 ms
```

Same question, same answer, 37 times faster. The fix is to leave the column bare and move the arithmetic to the other side of the comparison. This is one of the commonest causes of "but there *is* an index".

### 6. Why staging and production disagree

The planner chooses by estimating row counts, and it estimates from **statistics** it keeps about each table: "the query planner needs to estimate the number of rows retrieved by a query in order to make good choices of query plans" ([PostgreSQL docs, *Statistics Used by the Planner*](https://www.postgresql.org/docs/current/planner-stats.html)).

That gives four reasons a query can be fast in staging and slow in production — none of them a change in the code:

| What differs | What happens |
|---|---|
| **Size** | A plan that scans is fine at ten thousand rows and an outage at a hundred million. On a small table the planner may even *choose* a scan, correctly, so you never see the plan production will use. |
| **Skew** | Real data is uneven: one customer with a million orders, one status with 96% of rows. Test data is usually uniform. This chapter's scenario. |
| **Stale statistics** | After a bulk load or a large change, the statistics describe the old data. "With no statistics or obsolete statistics, the planner might make poor decisions" — so "running ANALYZE is strongly recommended" after bulk changes ([PostgreSQL docs, *Populating a Database*](https://www.postgresql.org/docs/current/populate.html)). |
| **Cache** | Staging's whole dataset fits in memory. Production's does not (Chapter 2.1). |

The defence is to test queries against data that has production's *size and shape*, and to read the plan rather than the stopwatch.

### 7. The N+1 query: many small queries where one would do

So far, one slow query. The opposite problem is just as common: hundreds of fast ones.

Code that uses an object-relational mapper (ORM) often looks like this:

```text
orders = load the 500 most recent orders          -- 1 query
for each order:
    print order.customer.name                      -- 1 query each: 500 more
```

Each access to `order.customer` quietly runs `SELECT … FROM customers WHERE id = ?`. One query for the list, plus N for its members: the **N+1 query problem**. The Ruby on Rails guide describes it with ten books and their authors: "The above code executes 1 (to find 10 books) + 10 (one per each book to load the author) = 11 queries in total" ([Rails Guides, *Active Record Query Interface*](https://guides.rubyonrails.org/active_record_querying.html#n-1-queries-problem)).

Every one of those queries is fast — a primary-key lookup, four pages. None will ever appear in a slow-query log. The cost is the **round trips**: each query is a network exchange with the database (Chapter 1.2), and the code waits for each before sending the next. Five hundred round trips at half a millisecond each is a quarter of a second spent on the wire.

The fixes all turn N+1 round trips into one or two:

- **A join**: ask the database for orders and customer names together.
- **A second query with a list**: fetch the orders, collect their customer IDs, and fetch those customers with `WHERE id = ANY(…)`. This is what an ORM's "eager loading" generates — Rails's `includes`, for example.

The example below measures all three.

### 8. Every index is paid for on every write

"After an index is created, the system has to keep it synchronized with the table. This adds overhead to data manipulation operations… Therefore indexes that are seldom or never used in queries should be removed" ([PostgreSQL docs, *Indexes: Introduction*](https://www.postgresql.org/docs/current/indexes-intro.html)).

Measured: inserting 500,000 rows into a table with no indexes took 0.7 s. Into the same table with the seven indexes this chapter created, 5.6 s — nearly eight times longer. Indexes also take space (the seven total over 500 MB, more than the 325 MB table) and compete with the table for memory.

So an index is a purchase: faster reads of one shape, paid for by slower writes of every shape. Section 11 of the example shows how to see which of your indexes are earning their keep.

---

## A real example

### Run it: eleven experiments on one table

[`code/ch02-indexes/indexes.sql`](code/ch02-indexes/indexes.sql) builds a five-million-row `orders` table with realistic skew — one customer owns a fifth of the orders; 96% of orders are `delivered` — and runs every measurement in this chapter.

```bash
psql -f phase-2-data/code/ch02-indexes/indexes.sql
```

A summary of the output from PostgreSQL 16 on the machine this was written on. Run it yourself for the full plans; your times will differ and the row counts will not.

| # | Experiment | Plan | Rows read | Time |
|---|---|---|---|---|
| 1 | Customer 4242's newest 20 — no index | Seq Scan, Sort | 5,000,000 | 277 ms |
| 2 | Same — index on `(customer_id)` | Bitmap scan, Sort | 48 | 0.24 ms |
| 3 | **Customer 1's** newest 20 — same index | Bitmap scan, Sort | 1,001,457 | **443 ms** |
| 4 | Customer 1 — index on `(customer_id, created_at DESC)` | Index Scan, no sort | 20 | **0.04 ms** |
| 5 | A ten-minute range of `created_at` — only the composite index | Seq Scan | 5,000,000 | 343 ms |
| 6a | `status = 'pending'` (1%) | Index Scan | 49,952 | 124 ms |
| 6b | `status = 'delivered'` (96%) | Seq Scan — index ignored | 5,000,000 | 859 ms |
| 7 | Oldest 20 pending — partial index (1.1 MB) | Index Scan | 20 | 0.05 ms |
| 8a | `created_at::date = …` | Seq Scan — index unusable | 5,000,000 | 536 ms |
| 8b | The same day as a range | Index Only Scan | 86,400 | 14.5 ms |
| 9 | `sum(total_cents)` for one customer — covering index | Index Only Scan, 0 heap fetches | 48 | 0.06 ms |
| 10 | Insert 500,000 rows: no indexes / seven indexes | — | — | 0.7 s / 5.6 s |

Rows 2 and 3 are the scenario from the top of the chapter: one query, one index, and a factor of nearly two thousand between two customers. Row 4 is the fix, and it is faster than row 2 *for everyone*.

The last experiment asks the database which indexes have been used:

```text
==== 11. Which indexes are actually used? ====
         index          | times_used |  size
------------------------+------------+---------
 orders_pkey            |          3 | 107 MB
 orders_customer        |          2 | 34 MB
 orders_created         |          1 | 107 MB
 orders_customer_newest |          1 | 150 MB
 orders_customer_total  |          1 | 107 MB
 orders_pending         |          1 | 1112 kB
 orders_status          |          1 | 34 MB
```

That comes from the `pg_stat_user_indexes` view, whose `idx_scan` column is the "number of index scans initiated on this index" ([PostgreSQL docs, *The Cumulative Statistics System*](https://www.postgresql.org/docs/current/monitoring-stats.html)). On a production database, an index whose count stays at zero for weeks is one you are paying for on every write and getting nothing from.

### Run it: 501 round trips, or one

[`code/ch02-nplus1/main.go`](code/ch02-nplus1/main.go) fetches the 500 most recent orders with their customers' names, three ways, against a real PostgreSQL. It is the first example in the course to use a library outside Go's standard library — the [`pgx`](https://pkg.go.dev/github.com/jackc/pgx/v5) PostgreSQL driver — which `go run` downloads automatically.

```bash
go run ./phase-2-data/code/ch02-nplus1
```

```text
Fetch the 500 most recent orders with customer names. Extra delay per round trip: 0s

  approach                         round trips      time
  N+1: one query per order            501          51.4 ms
  two queries (id = ANY(...))           2           1.6 ms
  one query with a JOIN                 1           1.3 ms
```

That is with the database on the same machine, where a round trip costs almost nothing — and N+1 is already forty times slower. In production the database is on another machine. The `-latency` flag adds a delay to every round trip to stand in for that:

```bash
go run ./phase-2-data/code/ch02-nplus1 -latency 500us
```

```text
Fetch the 500 most recent orders with customer names. Extra delay per round trip: 500µs

  approach                         round trips      time
  N+1: one query per order            501         715.3 ms
  two queries (id = ANY(...))           2           4.1 ms
  one query with a JOIN                 1           3.5 ms
```

Seven-tenths of a second against four milliseconds. (The simulated delay overshoots a little, as short sleeps do; the shape is what matters.) And remember Chapter 1.6: for those 715 ms, the request is holding a database connection.

The N+1 version does not look wrong when you read it:

```go
for i := range orders {
    r := d.query(ctx, `SELECT name FROM np1_customers WHERE id = $1`, orders[i].CustomerID)
    ...
}
```

A query inside a loop. With an ORM you do not even see the query — only `order.Customer.Name`. That is why this is found by counting queries, not by reading code.

---

## Trade-offs

**Each index speeds up some reads and slows down every write.** Add one when a query that matters cannot meet its target without it. Remove it when nothing uses it.

**A wider index serves more queries and costs more.** `(customer_id, created_at)` replaces `(customer_id)`; adding `INCLUDE` columns enables index-only scans. Each addition makes the index bigger and more expensive to maintain. Stop when the queries you care about are served.

**A partial index is small and cheap, and narrow.** It serves only queries whose `WHERE` clause implies its condition. That is ideal for "the few rows that need attention" — pending jobs, unpaid invoices, undeleted rows — and useless for anything else.

**A join or two queries?** A join is one round trip and lets the database do the matching. Two queries are simpler to write generically, which is why ORMs prefer them, and avoid repeating the parent's columns on every row when one parent has many children. Either is fine. N+1 is not.

**Denormalising instead of indexing.** Sometimes the cheapest read is one that needs no join at all, because the value was copied into the row. That trades write complexity and the risk of inconsistency for read speed (Chapter 2.5).

**When not to index.** A table with a few hundred rows fits in a page or two; scanning it is as fast as anything. A column you never filter or sort by needs no index. And a table that is written constantly and read rarely — an audit log — should have as few as you can manage.

---

## Practice

### Check yourself

Answer before reading on.

1. A table has an index on `(tenant_id, status, created_at)`. Which of these can use it to narrow the search? (a) `WHERE tenant_id = 5` (b) `WHERE status = 'open'` (c) `WHERE tenant_id = 5 AND created_at > '2024-01-01'` (d) `WHERE tenant_id = 5 AND status = 'open' ORDER BY created_at DESC LIMIT 10`
2. A `users` table has ten million rows and an index on `country`. 60% of users are in one country. A colleague complains that `WHERE country = 'US'` "isn't using the index". What do you tell them?
3. An endpoint returns 50 blog posts, each with its author's name and its number of comments. The ORM's log shows 101 queries. What happened, and how many queries should there be?

<details>
<summary>Answers</summary>

1. **(a)** Yes — the leading column. **(b)** Not usefully — it skips `tenant_id`; the index is sorted by tenant first, so "open" entries are scattered through it. (A skip scan would need one search per tenant, which pays only if tenants are few.) **(c)** Partly — `tenant_id = 5` narrows the search, but `status` sits between the two conditions, so on PostgreSQL 17 and earlier the range on `created_at` cannot narrow it further: the database reads all of tenant 5's entries and filters them. On PostgreSQL 18 and later, because `status` has only a handful of values, a skip scan can search each status in turn and use the date range. **(d)** Yes, perfectly — equality on the first two columns, and within them the entries are already in `created_at` order, so it reads ten entries and stops.

2. That the planner is right. Fetching six million rows through an index means six million pointer-chases into the table; reading the table straight through is cheaper. An index on a column with few distinct values only helps for the *rare* values. If they need the query to be fast, the real questions are whether it needs all those rows at all (does it have a `LIMIT`? another, more selective condition?), and whether a composite index starting with that other condition would serve it.

3. **N+1, twice.** One query for the posts; then one per post for the author (50); then one per post to count comments (50): 101. It should be **two or three**: the posts; the authors, with `WHERE id = ANY(…)` or a join; and the comment counts for all 50 posts in one grouped query — `SELECT post_id, count(*) FROM comments WHERE post_id = ANY(…) GROUP BY post_id`.

</details>

### Exercise: design the indexes

You are given this table and the five queries the application runs against it. It will hold 200 million rows. Writes are frequent: about 500 new tickets and 2,000 updates per second.

```sql
CREATE TABLE tickets (
    id          bigint PRIMARY KEY,
    tenant_id   int         NOT NULL,   -- about 5,000 tenants, very uneven in size
    assignee_id int,                    -- NULL when unassigned
    status      text        NOT NULL,   -- 'open' 3%, 'pending' 2%, 'closed' 95%
    priority    int         NOT NULL,   -- 1 (urgent) to 4
    subject     text        NOT NULL,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
```

```sql
-- Q1  A tenant's ticket list: newest first, paginated with a cursor. The most frequent query.
SELECT … FROM tickets WHERE tenant_id = $1 AND (created_at, id) < ($2, $3)
ORDER BY created_at DESC, id DESC LIMIT 50;

-- Q2  An agent's work queue: their open tickets, most urgent first, oldest first within a priority.
SELECT … FROM tickets WHERE tenant_id = $1 AND assignee_id = $2 AND status = 'open'
ORDER BY priority, created_at LIMIT 50;

-- Q3  Open one ticket.
SELECT … FROM tickets WHERE id = $1;

-- Q4  A nightly job: close tickets that have been pending for 30 days.
SELECT id FROM tickets WHERE status = 'pending' AND updated_at < now() - interval '30 days';

-- Q5  An admin dashboard, loaded a few times a day: tickets per status for one tenant.
SELECT status, count(*) FROM tickets WHERE tenant_id = $1 GROUP BY status;
```

Design the indexes. For each one give the columns in order, say which queries it serves, and justify it. Then say which query you chose *not* to build an index for, and why.

<details>
<summary>Worked solution</summary>

**Q3 — nothing to do.** The primary key already is an index on `id`.

**Q1 — `(tenant_id, created_at DESC, id DESC)`.**
Equality column first, then the sort columns in the sort's own order. This is the pattern from section 2 and from Chapter 1.4: the database seeks to the cursor position within the tenant's entries and reads 50. Cost: a handful of pages, for any tenant of any size, at any depth. This index is not optional — Q1 is the most frequent query, and without it the largest tenants would get the "million-order customer" plan.

**Q2 — a partial index: `(tenant_id, assignee_id, priority, created_at) WHERE status = 'open'`.**
- The two equality columns first.
- Then `priority, created_at` — exactly the `ORDER BY` — so the first 50 entries are the answer and no sort is needed.
- `WHERE status = 'open'` makes it partial. Only 3% of tickets are open, so the index holds about 6 million entries instead of 200 million, and the 95% of writes that touch closed tickets do not maintain it. The query's own `status = 'open'` is what allows the planner to use it.

Compare the non-partial alternative, `(tenant_id, assignee_id, status, priority, created_at)`: it would serve Q2 equally well and be some thirty times larger.

**Q4 — a partial index: `(updated_at) WHERE status = 'pending'`.**
Pending tickets are 2% of the table. The job wants the old ones, so index `updated_at` among pending tickets only. It is small, and the job reads just the entries it needs. Without it, the nightly job would scan 200 million rows to find a few thousand.

**Q5 — no index of its own.**
It can already use the Q1 index: `tenant_id` is that index's leading column, so the database reads only that tenant's entries, then visits those rows to read `status`. For a big tenant that is a lot of rows — but the dashboard is loaded a few times a day. A dedicated `(tenant_id, status)` index would make it an index-only scan, and it would also add a fifth index to maintain under **2,500 writes per second** — and every change of status would have to update it. That is a bad trade for a rare query. If the dashboard later becomes popular, revisit — or keep the counts in a small summary table instead (Chapter 2.5).

**The result: three indexes plus the primary key**, for five queries.

| Index | Serves | Size, roughly |
|---|---|---|
| Primary key `(id)` | Q3 | 200 M entries |
| `(tenant_id, created_at DESC, id DESC)` | Q1, and Q5 acceptably | 200 M entries |
| `(tenant_id, assignee_id, priority, created_at) WHERE status = 'open'` | Q2 | 6 M entries |
| `(updated_at) WHERE status = 'pending'` | Q4 | 4 M entries |

**What to check before shipping it.** Load a table with production's size *and skew* — a few very large tenants — run `EXPLAIN (ANALYZE, BUFFERS)` on each query for a large tenant and a small one, and confirm that Q1 and Q2 read about 50 rows and contain no `Sort`.

**The habit behind the answer.** Start from the queries, not the columns. For each: how often does it run, how many rows must it read, and does it sort? Build the fewest indexes that keep the frequent queries reading only the rows they return. Let rare queries be slower.

</details>

---

## Production tips and common mistakes

**Tips**

- **Read plans, not stopwatches.** For any query that matters, run `EXPLAIN (ANALYZE, BUFFERS)` and compare rows read with rows returned.
- **Test on data with production's size and skew.** Include the largest customer.
- **Find slow queries from the database, not by guessing.** The [`pg_stat_statements`](https://www.postgresql.org/docs/current/pgstatstatements.html) module provides "a means for tracking planning and execution statistics of all SQL statements executed by a server" — sort it by total time, not by average, so that a fast query run a million times shows up. And [`log_min_duration_statement`](https://www.postgresql.org/docs/current/runtime-config-logging.html) logs every statement slower than a threshold.
- **Count queries per request.** Log it, or assert on it in tests. A request that issues hundreds of queries has an N+1 in it.
- **Design an index from the query**: equality columns, then the sort or range column.
- **Use partial indexes for "the few rows that need attention".**
- **Run `ANALYZE` after bulk loads and large changes.**
- **Review index usage periodically** and drop what is unused.
- **Keep columns bare in `WHERE` clauses.** Put functions and arithmetic on the other side of the comparison.

**Common mistakes**

- **An index that matches the filter but not the sort**, on a query with `ORDER BY … LIMIT`. This chapter's scenario.
- **Indexing every column "to be safe"**, then wondering why writes are slow.
- **Several single-column indexes** where one multi-column index was needed.
- **Wrong column order**: putting the range or sort column before the equality column.
- **A function or cast around an indexed column.**
- **Fighting the planner** over a low-selectivity condition, instead of asking why the query needs so many rows.
- **N+1 queries hidden behind an ORM.**
- **Judging by the average.** The query was fast for the average customer.
- **Adding an index to a large, busy table without thinking about locks.** Building an index can block writes for as long as it takes; there is a way to avoid that, and it is in Chapter 2.7.

---

## Key takeaways

- The planner picks a plan from estimates. `EXPLAIN ANALYZE` shows the plan and what really happened; compare rows read with rows returned, and estimated rows with actual.
- An index should match the whole question: the filter, the order, and the limit. `(customer_id, created_at DESC)` makes "this customer's newest twenty" cost the same for any customer.
- In a multi-column index, order matters: equality columns first, then the sort or range column. A query that skips the leading column gets little or nothing from it.
- The planner ignores an index when the condition matches too many rows — correctly. For rare rows, a partial index is smaller and cheaper.
- Wrapping a column in a function hides it from its index.
- N+1 is many fast queries; its cost is round trips. Fix it with a join or a single query with a list.
- Every index slows every write. Build the fewest that serve the queries that matter.

**Next:** [2.3 Transactions and isolation levels](03-transactions-and-isolation.md) — what happens when two of these queries run at the same moment.

---

## Sources

- PostgreSQL documentation: [Using EXPLAIN](https://www.postgresql.org/docs/current/using-explain.html); [Indexes: Introduction](https://www.postgresql.org/docs/current/indexes-intro.html); [Multicolumn Indexes](https://www.postgresql.org/docs/current/indexes-multicolumn.html); [Indexes and ORDER BY](https://www.postgresql.org/docs/current/indexes-ordering.html); [Partial Indexes](https://www.postgresql.org/docs/current/indexes-partial.html); [Index-Only Scans and Covering Indexes](https://www.postgresql.org/docs/current/indexes-index-only-scans.html); [Indexes on Expressions](https://www.postgresql.org/docs/current/indexes-expressional.html); [Statistics Used by the Planner](https://www.postgresql.org/docs/current/planner-stats.html); [Populating a Database](https://www.postgresql.org/docs/current/populate.html); [pg_stat_statements](https://www.postgresql.org/docs/current/pgstatstatements.html); [Error Reporting and Logging](https://www.postgresql.org/docs/current/runtime-config-logging.html).
- Ruby on Rails Guides: [Active Record Query Interface — N + 1 Queries Problem](https://guides.rubyonrails.org/active_record_querying.html#n-1-queries-problem).
- Markus Winand, [*Use The Index, Luke*](https://use-the-index-luke.com/) — a free book-length guide to SQL indexing, for every major database.
- [`pgx`](https://pkg.go.dev/github.com/jackc/pgx/v5) — PostgreSQL driver for Go.
