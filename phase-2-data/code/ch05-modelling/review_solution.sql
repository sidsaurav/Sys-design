-- Chapter 2.5, exercise — the shop schema after review.
--
--   psql -f phase-2-data/code/ch05-modelling/review_solution.sql
--
-- Creates the corrected schema, loads a few rows, shows that an order keeps
-- its price when the product's price later changes, and cleans up.

\pset footer off
DROP TABLE IF EXISTS order_items, orders, product_tags, tags, products, users;

CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text        NOT NULL,
    name       text        NOT NULL,
    is_admin   boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE products (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        text   NOT NULL,
    price_cents bigint NOT NULL CHECK (price_cents >= 0)
);

CREATE TABLE tags (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE
);

CREATE TABLE product_tags (
    product_id bigint NOT NULL REFERENCES products ON DELETE CASCADE,
    tag_id     bigint NOT NULL REFERENCES tags     ON DELETE CASCADE,
    PRIMARY KEY (product_id, tag_id)
);
CREATE INDEX product_tags_tag ON product_tags (tag_id);          -- "all products with this tag"

CREATE TABLE orders (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint      NOT NULL REFERENCES users,
    status     text        NOT NULL DEFAULT 'pending'
                           CHECK (status IN ('pending', 'paid', 'shipped', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_user_newest ON orders (user_id, created_at DESC);   -- also indexes the foreign key

CREATE TABLE order_items (
    order_id         bigint NOT NULL REFERENCES orders ON DELETE CASCADE,
    product_id       bigint NOT NULL REFERENCES products,
    quantity         int    NOT NULL CHECK (quantity > 0),
    unit_price_cents bigint NOT NULL CHECK (unit_price_cents >= 0),     -- the price when it was bought
    PRIMARY KEY (order_id, product_id)
);
CREATE INDEX order_items_product ON order_items (product_id);    -- indexes the other foreign key

-- A little data.
INSERT INTO users (email, name) VALUES ('ada@example.com', 'Ada');
INSERT INTO products (name, price_cents) VALUES ('T-shirt', 1999), ('Mug', 899);
INSERT INTO tags (name) VALUES ('cotton'), ('sale');
INSERT INTO product_tags VALUES (1, 1), (1, 2), (2, 2);
INSERT INTO orders (user_id, status) VALUES (1, 'paid');
INSERT INTO order_items (order_id, product_id, quantity, unit_price_cents)
SELECT 1, id, 2, price_cents FROM products;

\echo
\echo ==== The order total, computed from its items ====
SELECT o.id AS order_id, sum(i.quantity * i.unit_price_cents) AS total_cents
FROM orders o JOIN order_items i ON i.order_id = o.id GROUP BY o.id;

\echo
\echo ==== The price of the T-shirt goes up. The old order does not change. ====
UPDATE products SET price_cents = 2499 WHERE name = 'T-shirt';
SELECT o.id AS order_id, sum(i.quantity * i.unit_price_cents) AS total_cents
FROM orders o JOIN order_items i ON i.order_id = o.id GROUP BY o.id;

\echo
\echo ==== Products tagged "sale": a join, not a string search ====
SELECT p.name FROM products p
JOIN product_tags pt ON pt.product_id = p.id
JOIN tags t ON t.id = pt.tag_id
WHERE t.name = 'sale' ORDER BY p.name;

DROP TABLE order_items, orders, product_tags, tags, products, users;
