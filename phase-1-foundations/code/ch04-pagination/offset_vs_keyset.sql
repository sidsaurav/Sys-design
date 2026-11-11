-- Chapter 1.4 — why deep OFFSET pagination is slow, and what keyset
-- (cursor) pagination does instead.
--
-- Run against any PostgreSQL (12 or newer), in a scratch database:
--
--   psql -f phase-1-foundations/code/ch04-pagination/offset_vs_keyset.sql
--
-- It creates a 5-million-row table, then runs the same "next 20 orders"
-- query three ways and prints the plan and timing of each.

\timing off
DROP TABLE IF EXISTS orders;

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id int         NOT NULL,
    total_cents int         NOT NULL,
    created_at  timestamptz NOT NULL
);

-- 5 million orders, one per second starting on 2024-01-01.
INSERT INTO orders (customer_id, total_cents, created_at)
SELECT (random() * 100000)::int,
       (random() * 50000)::int,
       timestamptz '2024-01-01 00:00:00+00' + g * interval '1 second'
FROM generate_series(1, 5000000) AS g;

-- The index every "newest first" list needs. The id is included so that
-- the order is total even when two rows share a timestamp.
CREATE INDEX orders_newest_first ON orders (created_at DESC, id DESC);

VACUUM ANALYZE orders;

\echo
\echo ==== 1. OFFSET pagination, first page (OFFSET 0) ====
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)
SELECT id, customer_id, total_cents, created_at
FROM orders
ORDER BY created_at DESC, id DESC
LIMIT 20 OFFSET 0;

\echo
\echo ==== 2. OFFSET pagination, a deep page (OFFSET 4,000,000) ====
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)
SELECT id, customer_id, total_cents, created_at
FROM orders
ORDER BY created_at DESC, id DESC
LIMIT 20 OFFSET 4000000;

\echo
\echo ==== 3. Keyset pagination, the same deep page ====
\echo ==== (continue after the last row the client saw)  ====
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)
SELECT id, customer_id, total_cents, created_at
FROM orders
WHERE (created_at, id) < (timestamptz '2024-01-12 13:46:41+00', 1000001)
ORDER BY created_at DESC, id DESC
LIMIT 20;

DROP TABLE orders;
