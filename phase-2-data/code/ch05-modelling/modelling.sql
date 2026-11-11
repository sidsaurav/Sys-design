-- Chapter 2.5 — what the schema can do for you: types, constraints, foreign
-- keys, and the cost of key order.
--
-- Run against a scratch PostgreSQL database (12 or newer):
--
--   psql -f phase-2-data/code/ch05-modelling/modelling.sql
--
-- Several statements below are SUPPOSED to fail: the point is to see the
-- database refuse bad data. Takes about two minutes.

\pset footer off
\set ON_ERROR_STOP off
SET max_parallel_workers_per_gather = 0;
DROP TABLE IF EXISTS m_orders, m_customers, m_events, keys_staging, k_random_order, k_sorted_order, k_bigint;

\echo
\echo ==== 1. Types have limits: a 4-byte integer key runs out ====
CREATE TABLE m_events (id integer PRIMARY KEY, what text NOT NULL);
INSERT INTO m_events VALUES (2147483647, 'the last event that fits');
INSERT INTO m_events VALUES (2147483648, 'one more');
DROP TABLE m_events;

\echo
\echo ==== 2. Floating point is not for money ====
SELECT 0.1::float8  + 0.2::float8  = 0.3::float8  AS "float: 0.1 + 0.2 = 0.3?",
       0.1::numeric + 0.2::numeric = 0.3::numeric AS "numeric: 0.1 + 0.2 = 0.3?";

\echo
\echo ==== 3. Constraints: the database refuses bad data, whoever sends it ====
CREATE TABLE m_customers (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         text   NOT NULL,
    balance_cents bigint NOT NULL DEFAULT 0 CHECK (balance_cents >= 0)
);
CREATE UNIQUE INDEX m_customers_email ON m_customers (lower(email));

CREATE TABLE m_orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint      NOT NULL REFERENCES m_customers (id),
    status      text        NOT NULL CHECK (status IN ('pending', 'paid', 'shipped', 'cancelled')),
    total_cents bigint      NOT NULL CHECK (total_cents >= 0),
    created_at  timestamptz NOT NULL DEFAULT now()
);

INSERT INTO m_customers (email) VALUES ('ada@example.com');
\echo -- the same email in different capitals:
INSERT INTO m_customers (email) VALUES ('Ada@Example.com');
\echo -- a negative balance:
UPDATE m_customers SET balance_cents = -500 WHERE email = 'ada@example.com';
\echo -- an order for a customer who does not exist:
INSERT INTO m_orders (customer_id, status, total_cents) VALUES (999999, 'paid', 100);
\echo -- a status nobody defined:
INSERT INTO m_orders (customer_id, status, total_cents) VALUES (1, 'payed', 100);
\echo -- a missing value:
INSERT INTO m_orders (customer_id, status) VALUES (1, 'paid');

\echo
\echo ==== 4. A foreign key does not index the referencing column ====
\set ON_ERROR_STOP on
-- 100,000 more customers and 5 million orders. The last ten customers get
-- no orders, so they can be deleted.
INSERT INTO m_customers (email) SELECT 'user' || g || '@example.com' FROM generate_series(1, 100000) AS g;
SELECT min(id) AS first_id, max(id) AS last_id FROM m_customers WHERE email LIKE 'user%' \gset
INSERT INTO m_orders (customer_id, status, total_cents)
SELECT :first_id + (g % 99990), 'paid', 1000 FROM generate_series(1, 5000000) AS g;
ANALYZE m_customers; ANALYZE m_orders;

\echo -- delete one customer who has no orders. The database must first check that:
EXPLAIN (ANALYZE, COSTS OFF)
DELETE FROM m_customers WHERE id = :last_id;

CREATE INDEX m_orders_customer ON m_orders (customer_id);
\echo -- the same, after indexing orders.customer_id:
EXPLAIN (ANALYZE, COSTS OFF)
DELETE FROM m_customers WHERE id = :last_id - 1;

\echo
\echo ==== 5. Key order: the same 5 million UUIDs, inserted in random order and in sorted order ====
CREATE UNLOGGED TABLE keys_staging AS SELECT gen_random_uuid() AS id FROM generate_series(1, 5000000);
CREATE TABLE k_random_order (id uuid PRIMARY KEY);
CREATE TABLE k_sorted_order (id uuid PRIMARY KEY);
CREATE TABLE k_bigint       (id bigint PRIMARY KEY);
CHECKPOINT;
\echo -- UUIDs in random order (what random UUIDs give you):
\timing on
INSERT INTO k_random_order SELECT id FROM keys_staging;
\timing off
CHECKPOINT;
\echo -- the same UUIDs in sorted order (what time-ordered UUIDs give you):
\timing on
INSERT INTO k_sorted_order SELECT id FROM keys_staging ORDER BY id;
\timing off
CHECKPOINT;
\echo -- 5 million sequential bigints, for comparison:
\timing on
INSERT INTO k_bigint SELECT g FROM generate_series(1, 5000000) AS g;
\timing off
SELECT c.relname AS index, pg_size_pretty(pg_relation_size(c.oid)) AS size
FROM pg_class c
WHERE c.relname IN ('k_bigint_pkey', 'k_random_order_pkey', 'k_sorted_order_pkey') ORDER BY 1;

DROP TABLE m_orders, m_customers, keys_staging, k_random_order, k_sorted_order, k_bigint;
