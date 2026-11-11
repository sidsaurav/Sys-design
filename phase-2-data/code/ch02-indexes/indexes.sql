-- Chapter 2.2 — indexes and query plans.
--
-- Run against a scratch PostgreSQL database (12 or newer):
--
--   psql -f phase-2-data/code/ch02-indexes/indexes.sql
--
-- Builds a 5-million-row orders table whose data is skewed the way real
-- data is: one customer owns a fifth of all orders, and almost every order
-- has the same status. Takes about two minutes.

\pset footer off
SET max_parallel_workers_per_gather = 0;   -- keep the plans simple
SET jit = off;
DROP TABLE IF EXISTS orders;
SELECT setseed(0.42);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id int         NOT NULL,
    status      text        NOT NULL,
    total_cents int         NOT NULL,
    created_at  timestamptz NOT NULL
);

-- Customer 1 is a "whale" with about a million orders. The other 100,000
-- customers have about 40 each. 96% of orders are delivered.
INSERT INTO orders (customer_id, status, total_cents, created_at)
SELECT CASE WHEN random() < 0.2 THEN 1 ELSE 2 + (random() * 99999)::int END,
       CASE WHEN r < 0.96 THEN 'delivered' WHEN r < 0.99 THEN 'shipped' ELSE 'pending' END,
       (random() * 50000)::int,
       timestamptz '2024-01-01 00:00:00+00' + g * interval '1 second'
FROM (SELECT g, random() AS r FROM generate_series(1, 5000000) AS g) AS s;

VACUUM ANALYZE orders;

\o /dev/null
SELECT * FROM orders WHERE id = 1;    -- warm-up
\o

\echo
\echo ==== 0. What the planner expects, before running anything ====
EXPLAIN
SELECT * FROM orders WHERE customer_id = 4242 ORDER BY created_at DESC LIMIT 20;

\echo
\echo ==== 1. "The 20 newest orders of customer 4242" with no useful index ====
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM orders WHERE customer_id = 4242 ORDER BY created_at DESC LIMIT 20;

\echo
\echo ==== 2. The same, with an index on (customer_id) ====
CREATE INDEX orders_customer ON orders (customer_id);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM orders WHERE customer_id = 4242 ORDER BY created_at DESC LIMIT 20;

\echo
\echo ==== 3. The same query and index, for the customer with a million orders ====
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM orders WHERE customer_id = 1 ORDER BY created_at DESC LIMIT 20;

\echo
\echo ==== 4. An index that matches the whole question: (customer_id, created_at DESC) ====
CREATE INDEX orders_customer_newest ON orders (customer_id, created_at DESC);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM orders WHERE customer_id = 1 ORDER BY created_at DESC LIMIT 20;

\echo
\echo ==== 5. The composite index cannot help a query that skips its first column ====
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM orders WHERE created_at >= '2024-02-01' AND created_at < '2024-02-01 00:10';

\echo
\echo ==== 6. Selectivity: the planner uses an index only when it pays ====
CREATE INDEX orders_status ON orders (status);
\echo -- a rare value (1% of rows):
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT sum(total_cents) FROM orders WHERE status = 'pending';
\echo -- a common value (96% of rows):
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT sum(total_cents) FROM orders WHERE status = 'delivered';

\echo
\echo ==== 7. A partial index covers only the rows you ask about ====
CREATE INDEX orders_pending ON orders (created_at) WHERE status = 'pending';
SELECT indexrelname AS index, pg_size_pretty(pg_relation_size(indexrelid)) AS size
FROM pg_stat_user_indexes WHERE relname = 'orders' ORDER BY pg_relation_size(indexrelid) DESC;
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM orders WHERE status = 'pending' ORDER BY created_at LIMIT 20;

\echo
\echo ==== 8. A function wrapped around the column hides it from the index ====
CREATE INDEX orders_created ON orders (created_at);
\echo -- written with a cast on the column:
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT count(*) FROM orders WHERE created_at::date = '2024-02-01';
\echo -- the same question written as a range:
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT count(*) FROM orders WHERE created_at >= '2024-02-01' AND created_at < '2024-02-02';

\echo
\echo ==== 9. An index-only scan never touches the table ====
CREATE INDEX orders_customer_total ON orders (customer_id) INCLUDE (total_cents);
VACUUM orders;
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT sum(total_cents) FROM orders WHERE customer_id = 4242;

\echo
\echo ==== 10. What indexes cost: inserting 500,000 rows ====
CREATE TABLE no_indexes    (LIKE orders);                    -- same columns, no indexes
CREATE TABLE seven_indexes (LIKE orders INCLUDING INDEXES);  -- and all seven of them
\echo -- into a table with no indexes:
\timing on
INSERT INTO no_indexes    SELECT * FROM orders WHERE id <= 500000;
\timing off
\echo -- into a table with the seven indexes created above:
\timing on
INSERT INTO seven_indexes SELECT * FROM orders WHERE id <= 500000;
\timing off

\echo
\echo ==== 11. Which indexes are actually used? ====
SELECT indexrelname AS index, idx_scan AS times_used, pg_size_pretty(pg_relation_size(indexrelid)) AS size
FROM pg_stat_user_indexes WHERE relname = 'orders' ORDER BY idx_scan DESC, indexrelname;

DROP TABLE no_indexes, seven_indexes, orders;
