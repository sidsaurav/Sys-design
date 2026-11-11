-- Chapter 1.5 — idempotency keys in PostgreSQL.
--
-- Run against a scratch database:
--
--   psql -f phase-1-foundations/code/ch05-idempotency/idempotency.sql
--
-- The unique constraint on (account_id, key) is what makes "have I seen this
-- request before?" a single atomic step. The key row and the order row are
-- written in ONE transaction, so there is never one without the other.

DROP TABLE IF EXISTS idempotency_keys, orders;

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id  bigint NOT NULL,
    total_cents int    NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE idempotency_keys (
    account_id    bigint      NOT NULL,   -- keys are scoped to the caller
    key           text        NOT NULL,
    request_hash  text        NOT NULL,   -- fingerprint of the request body
    response_code int         NOT NULL,
    response_body jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, key)
);

-- create_order does everything in one statement's transaction:
--   1. try to claim the key;
--   2. if we claimed it, create the order and store the response;
--   3. if we did not, return what was stored the first time.
CREATE OR REPLACE FUNCTION create_order(
    p_account bigint, p_key text, p_hash text, p_total int
) RETURNS TABLE (code int, body jsonb, replayed boolean)
LANGUAGE plpgsql AS $$
DECLARE
    v_claimed  boolean;
    v_order_id bigint;
    v_existing idempotency_keys%ROWTYPE;
BEGIN
    INSERT INTO idempotency_keys (account_id, key, request_hash, response_code, response_body)
    VALUES (p_account, p_key, p_hash, 0, '{}')
    ON CONFLICT (account_id, key) DO NOTHING;
    GET DIAGNOSTICS v_claimed = ROW_COUNT;

    IF v_claimed THEN
        INSERT INTO orders (account_id, total_cents)
        VALUES (p_account, p_total)
        RETURNING id INTO v_order_id;

        UPDATE idempotency_keys
        SET response_code = 201,
            response_body = jsonb_build_object('order_id', v_order_id, 'total_cents', p_total)
        WHERE account_id = p_account AND key = p_key
        RETURNING response_code, response_body INTO code, body;

        replayed := false;
        RETURN NEXT;
        RETURN;
    END IF;

    SELECT * INTO v_existing FROM idempotency_keys
    WHERE account_id = p_account AND key = p_key;

    IF v_existing.request_hash <> p_hash THEN
        code := 422;
        body := '{"type": "idempotency-key-reused"}';
    ELSE
        code := v_existing.response_code;
        body := v_existing.response_body;
    END IF;
    replayed := true;
    RETURN NEXT;
END $$;

\echo
\echo ==== First request ====
SELECT * FROM create_order(7, 'key-abc', 'hash-of-body-1', 4599);

\echo ==== The same request again (a retry) ====
SELECT * FROM create_order(7, 'key-abc', 'hash-of-body-1', 4599);

\echo ==== The same key with a different body (a client bug) ====
SELECT * FROM create_order(7, 'key-abc', 'hash-of-body-2', 9999);

\echo ==== Orders created: exactly one ====
SELECT count(*) AS orders FROM orders;

DROP FUNCTION create_order(bigint, text, text, int);
DROP TABLE idempotency_keys, orders;
