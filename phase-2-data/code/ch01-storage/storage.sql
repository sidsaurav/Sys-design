-- Chapter 2.1 — look inside the database: pages, a B-tree, and the
-- write-ahead log.
--
-- Run against a scratch PostgreSQL database (12 or newer):
--
--   psql -f phase-2-data/code/ch01-storage/storage.sql
--
-- Part 5 uses the pageinspect extension, which ships with PostgreSQL
-- (package postgresql-contrib on Debian/Ubuntu; included in the official
-- Docker image). It needs a superuser.

\pset footer off
SET max_parallel_workers_per_gather = 0;   -- keep the query plans simple
DROP TABLE IF EXISTS accounts;

CREATE TABLE accounts (
    id            bigint      PRIMARY KEY,
    email         text        NOT NULL,
    name          text        NOT NULL,
    balance_cents bigint      NOT NULL,
    created_at    timestamptz NOT NULL
);

-- 5 million accounts.
INSERT INTO accounts
SELECT g,
       'user' || g || '@example.com',
       'User ' || g,
       (random() * 100000)::bigint,
       timestamptz '2024-01-01 00:00:00+00' + g * interval '1 second'
FROM generate_series(1, 5000000) AS g;

VACUUM ANALYZE accounts;

-- Warm-up: the first query in a session also reads catalog pages, which
-- would muddy the page counts below.
\o /dev/null
SELECT * FROM accounts WHERE id = 1;
EXPLAIN SELECT * FROM accounts WHERE email = 'x';
\o

\echo
\echo ==== 1. A table is an array of 8 kB pages ====
SELECT pg_size_pretty(pg_relation_size('accounts'))           AS table_size,
       pg_relation_size('accounts') / 8192                    AS pages,
       round(5000000.0 / (pg_relation_size('accounts') / 8192)) AS rows_per_page;

\echo
\echo ==== 2. Find one row by email, with no index on email ====
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM accounts WHERE email = 'user4000000@example.com';

\echo
\echo ==== 3. Find one row by primary key (a B-tree index) ====
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM accounts WHERE id = 4000000;

\echo
\echo ==== 4. Add an index on email, and repeat query 2 ====
CREATE INDEX accounts_email ON accounts (email);
\o /dev/null
EXPLAIN SELECT * FROM accounts WHERE email = 'x';
\o
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT * FROM accounts WHERE email = 'user4000000@example.com';

\echo
\echo ==== 5. How tall is the B-tree? ====
CREATE EXTENSION IF NOT EXISTS pageinspect;
SELECT 'accounts_pkey'                                         AS index,
       pg_size_pretty(pg_relation_size('accounts_pkey'))       AS size,
       pg_relation_size('accounts_pkey') / 8192                AS pages,
       (SELECT level FROM bt_metap('accounts_pkey'))           AS levels_above_the_leaves
UNION ALL
SELECT 'accounts_email',
       pg_size_pretty(pg_relation_size('accounts_email')),
       pg_relation_size('accounts_email') / 8192,
       (SELECT level FROM bt_metap('accounts_email'));

\echo
\echo ==== 5b. The pages of the primary-key index, by level ====
SELECT CASE s.type WHEN 'r' THEN '1 root' WHEN 'i' THEN '2 internal' ELSE '3 leaf' END AS level,
       count(*)                   AS pages,
       round(avg(s.live_items))   AS entries_per_page
FROM generate_series(1, (pg_relation_size('accounts_pkey') / 8192 - 1)::int) AS p,
     LATERAL bt_page_stats('accounts_pkey', p) AS s
GROUP BY 1 ORDER BY 1;

\echo
\echo ==== 6. What a commit costs: 2,000 single-row transactions ====
DROP TABLE IF EXISTS ledger;
CREATE TABLE ledger (id int PRIMARY KEY, note text);

\echo -- each COMMIT waits for the write-ahead log to reach the disk:
\timing on
DO $$
BEGIN
    FOR i IN 1..2000 LOOP
        INSERT INTO ledger VALUES (i, 'entry');
        COMMIT;
    END LOOP;
END $$;
\timing off

\echo -- the same, but COMMIT returns before the log is flushed:
SET synchronous_commit = off;
\timing on
DO $$
BEGIN
    FOR i IN 2001..4000 LOOP
        INSERT INTO ledger VALUES (i, 'entry');
        COMMIT;
    END LOOP;
END $$;
\timing off
SET synchronous_commit = on;

\echo
\echo ==== 7. How much log does a write produce? ====
SELECT pg_current_wal_lsn() AS before \gset
INSERT INTO ledger SELECT g, 'entry' FROM generate_series(4001, 104000) AS g;
SELECT pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), :'before')) AS wal_written_for_100000_rows,
       pg_size_pretty(pg_relation_size('ledger'))                       AS table_size_now;

\echo
\echo ==== 8. An UPDATE writes a new copy of the row ====
SELECT pg_size_pretty(pg_relation_size('accounts')) AS table_size_before;
UPDATE accounts SET balance_cents = balance_cents + 1;      -- every row
SELECT pg_size_pretty(pg_relation_size('accounts')) AS after_updating_every_row;
VACUUM accounts;
SELECT pg_size_pretty(pg_relation_size('accounts')) AS after_vacuum;

DROP TABLE ledger;
DROP TABLE accounts;
