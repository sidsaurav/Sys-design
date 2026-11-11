-- Chapter 2.8 — text search with and without an inverted index.
--
-- Run against a scratch PostgreSQL database (12 or newer):
--
--   psql -f phase-2-data/code/ch08-search/search.sql
--
-- Builds 500,000 product descriptions and looks for the few that mention
-- a rare word: first by scanning every description, then through an
-- inverted index, the structure a search engine is built around. Takes
-- under a minute.

\pset footer off
SET max_parallel_workers_per_gather = 0;   -- keep the plans simple
SET jit = off;
SET client_min_messages = warning;
DROP TABLE IF EXISTS s_products;
\o /dev/null
SELECT setseed(0.42);
\o

CREATE TABLE s_products (
    id          bigint PRIMARY KEY,
    description text NOT NULL
);

-- Each description is 25 to 34 words drawn from a small vocabulary.
-- One product in 5,000 is also a pair of waterproof hiking boots.
INSERT INTO s_products
SELECT g,
       (SELECT string_agg(word, ' ')
        FROM (SELECT (ARRAY['red','blue','green','black','white','cotton','wool','linen','leather','denim',
                            'shirt','jacket','trousers','dress','skirt','scarf','gloves','socks','sweater','coat',
                            'light','warm','soft','classic','modern','casual','formal','summer','winter','comfortable',
                            'pocket','button','zip','collar','sleeve','lined','fitted','loose','striped','plain'])
                           [1 + floor(random() * 40)::int] AS word
              FROM generate_series(1, 25 + (g % 10))) AS words)
       || CASE WHEN g % 5000 = 0 THEN ' waterproof hiking boots' ELSE '' END
FROM generate_series(1, 500000) AS g;

VACUUM ANALYZE s_products;

\echo
\echo ==== 1. The table ====
SELECT count(*) AS products, pg_size_pretty(pg_table_size('s_products')) AS size FROM s_products;
SELECT description FROM s_products WHERE id = 5000;

\echo
\echo ==== 2. Find the waterproof products by scanning every description ====
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)
SELECT count(*) FROM s_products WHERE description ILIKE '%waterproof%';

\echo
\echo ==== 3. What full-text search stores for one description ====
\echo (each word reduced to its stem, with its positions)
SELECT to_tsvector('english', 'Waterproof hiking boots, lined') AS tsvector;

\echo
\echo ==== 4. Build an inverted index over those word lists ====
\timing on
CREATE INDEX s_products_search ON s_products USING gin (to_tsvector('english', description));
\timing off
SELECT pg_size_pretty(pg_relation_size('s_products_search')) AS index_size;

\echo
\echo ==== 5. The same question through the index ====
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)
SELECT count(*) FROM s_products
WHERE to_tsvector('english', description) @@ to_tsquery('english', 'waterproof');

\echo
\echo ==== 6. Search understands words; LIKE understands characters ====
\echo (a user searches for "hike"; the descriptions say "hiking")
SELECT (SELECT count(*) FROM s_products WHERE description ILIKE '%hike%') AS found_by_like,
       (SELECT count(*) FROM s_products
        WHERE to_tsvector('english', description) @@ to_tsquery('english', 'hike')) AS found_by_search;

\echo
\echo ==== 7. Two words, best matches first ====
SELECT id, round(ts_rank(to_tsvector('english', description), query)::numeric, 3) AS rank,
       left(description, 60) || '...' AS description
FROM s_products, to_tsquery('english', 'waterproof & leather') AS query
WHERE to_tsvector('english', description) @@ query
ORDER BY rank DESC, id
LIMIT 3;

DROP TABLE s_products;
