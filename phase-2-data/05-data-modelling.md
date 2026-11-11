# 2.5 Data modelling: keys, constraints, and denormalisation

[Syllabus](../SYLLABUS.md) · Phase 2 · Previous: [2.4 Concurrency control in application code](04-concurrency-control.md) · Next: [2.6 The database as a shared resource](06-database-as-shared-resource.md)

> **Time:** about 50 minutes to read, 75 minutes of practice.
> **You need:** [Chapter 2.1](01-how-databases-store-data.md) (B-trees) and [Chapter 2.4](04-concurrency-control.md) (constraints as the net under your code), and a PostgreSQL for the examples — see [the phase overview](README.md).

**By the end of this chapter you can**

- choose a primary key — its type, and how it is generated — knowing what each choice costs in five years;
- pick column types that cannot hold a wrong value, and avoid the well-known traps;
- write constraints that make invalid data impossible rather than merely unlikely;
- normalise a schema, and say precisely what you buy and what you risk when you denormalise;
- tell real duplication from a deliberate record of history.

---

## The problem

On 9 November 2018, Basecamp 3 was read-only for almost five hours. From the company's [postmortem](https://m.signalvnoise.com/postmortem-on-the-read-only-outage-of-basecamp-on-november-9th-2018/), written by David Heinemeier Hansson: "users could access existing messages, todo lists, and files, but no new information could be entered, and no existing information could be altered. Everything was frozen in place."

The cause was not load, a bad deploy or failed hardware. It was a decision made when a table was created: "The root cause was that our database hit the ceiling of 2,147,483,647 on our very busy events table" ([Signal v. Noise, *Postmortem on the read-only outage of Basecamp on November 9th, 2018*](https://m.signalvnoise.com/postmortem-on-the-read-only-outage-of-basecamp-on-november-9th-2018/)).

2,147,483,647 is the largest number a four-byte integer can hold. The table's primary key was a four-byte integer. After a little over two billion rows there were no more numbers, and every insert failed. Because "almost every single activity in Basecamp is tracked in this table", nearly every action in the product failed with it. The fix was an eight-byte integer, which the company's own framework had already made the default a year earlier. The [postmortem](https://m.signalvnoise.com/postmortem-on-the-read-only-outage-of-basecamp-on-november-9th-2018/) says so plainly: "This was an avoidable problem."

That is what makes schema decisions different from most others in this course. A slow query can be fixed next week. A schema is the thing all your code and all your data are built on, and it gets harder to change with every row you add (Chapter 2.7). Its mistakes are also patient: an integer key works for years, a missing constraint lets in one bad row in a million, a float is off by a cent only in the monthly totals.

This chapter is about the decisions worth getting right on day one: keys, types, constraints, and when to store a fact more than once.

---

## The concept

### 1. Start from the rules, not the screens

It is tempting to design tables from the user interface: one table per form. Design instead from two lists.

- **The things and how they relate**: users, products, orders; an order *belongs to* a user and *contains* products.
- **The rules that must always be true**: every order belongs to an existing user; an email identifies at most one account; a quantity is at least one; a balance is never negative.

The first list becomes tables and keys. The second becomes constraints. A good schema is one in which a row that breaks a rule *cannot be stored* — not by buggy code, not by a one-off script, not by an engineer fixing data at 3 a.m. Section 3 shows why that has to be the database's job.

### 2. Primary keys

Every table needs a column, or columns, that identify a row for ever. Three decisions.

**Natural or surrogate?** A *natural key* is something from the real world: an email address, a tax number, a product code. A *surrogate key* is a value that means nothing and exists only to identify the row. Use surrogates. Real-world identifiers change (people change email addresses), turn out not to be unique after all, and are sometimes missing. A meaningless key never needs to change. Keep the natural identifier as an ordinary column with a `UNIQUE` constraint.

**How big?** This is the Basecamp decision. PostgreSQL's `integer` holds up to 2,147,483,647; `bigint`, up to 9,223,372,036,854,775,807 ([PostgreSQL docs, *Numeric Types*](https://www.postgresql.org/docs/current/datatype-numeric.html)). Four extra bytes per row buy a limit you cannot reach: at a million inserts per second, a `bigint` lasts about three hundred thousand years. **Use `bigint` for every key.** The example below runs out of `integer` on purpose so you can see the error Basecamp saw.

**Generated how?** Two families.

| | Sequential number | UUID |
|---|---|---|
| Looks like | `48213` | `019535d9-3df7-79fb-b466-fa907fa17f9e` |
| Who generates it | The database, one at a time | Anyone, anywhere, with no coordination |
| Size | 8 bytes | 16 bytes |
| Order of arrival | Always increasing | Random (version 4) or increasing (version 7) |
| Reveals | How many rows you have, and every other ID | Nothing (version 4); roughly when it was made (version 7) |

*Sequential numbers* are compact and arrive in increasing order, which a B-tree handles best: each new key goes at the right-hand end of the index. In PostgreSQL, declare them with `GENERATED ALWAYS AS IDENTITY`. Two things to know about them. They are **not gap-free**: a value handed out to a transaction that then fails is not reused, which "can result in gaps in the sequence of assigned values" ([PostgreSQL docs, *Sequence Manipulation Functions*](https://www.postgresql.org/docs/current/functions-sequence.html)) — so never use them for anything that must be consecutive, such as invoice numbers. And they are **guessable**: if order 48213 exists, so does 48212. Shown in URLs, they tell competitors your order volume and invite attackers to try the neighbours. OWASP's advice for public identifiers is to "prefer the use of random and unpredictable values as GUIDs for records' IDs" ([OWASP API Security Top 10](https://owasp.org/API-Security/editions/2023/en/0xa1-broken-object-level-authorization/)) — though the real defence is checking permissions on every request (Chapter 8.3).

*UUIDs* can be created by any server, or by the client, without asking the database. That is what Chapter 1.5's idempotency keys rely on, and it becomes essential once data lives on several machines (Phase 5). The classic kind, version 4, is 122 random bits — and randomness is exactly what a B-tree dislikes. The current UUID specification explains: such UUIDs "have poor database-index locality. This means that new values created in succession are not close to each other in the index; thus, they require inserts to be performed at random locations. The resulting negative performance effects on the common structures used for this (B-tree and its variants) can be dramatic" ([RFC 9562, §2.1](https://www.rfc-editor.org/rfc/rfc9562.html#section-2.1)).

Chapter 2.1 tells you why. Sequential keys touch only the last page of the index, which is always in memory. Random keys touch a different page each time; once the index is bigger than memory, most inserts need a disk read, and pages fill unevenly and split.

The same RFC defines the fix. **UUID version 7** begins with a timestamp, so new values sort after old ones: it "features a time-ordered value field derived from the widely implemented and well-known Unix Epoch timestamp source" ([§5.7](https://www.rfc-editor.org/rfc/rfc9562.html#section-5.7)), and such UUIDs "benefit from greater database-index locality because the new values are near each other in the index" ([§6.11](https://www.rfc-editor.org/rfc/rfc9562.html#section-6.11)). PostgreSQL 18 generates them with a built-in `uuidv7()` function ([docs](https://www.postgresql.org/docs/current/functions-uuid.html)); for earlier versions, generate them in the application.

**A workable default:** `bigint` identity keys inside the database; where an ID is shown to the outside world or created outside the database, a UUID — version 7 unless the creation time must stay private.

### 3. Constraints: make wrong data impossible

Application code validates input. So why should the database check again?

Because the application is not the only thing that writes. There are background jobs, data migrations, the admin console, the script someone runs by hand, the second service that is added next year — and two requests at once, which validation in code cannot see at all. The Ruby on Rails guide makes that last point about its own uniqueness validation: it "does not create a uniqueness constraint in the database, so a scenario can occur whereby two different database connections create two records with the same value for a column that you intended to be unique. To avoid this, you must create a unique index on that column in your database" ([Rails Guides, *Active Record Validations*](https://guides.rubyonrails.org/active_record_validations.html#uniqueness)).

Validation in the application is for good error messages. Constraints in the database are for correctness. PostgreSQL's [constraint types](https://www.postgresql.org/docs/current/ddl-constraints.html):

| Constraint | The rule it enforces | Example |
|---|---|---|
| `NOT NULL` | The value is always present | Every order has a customer |
| `UNIQUE` | No two rows share this value | One account per email |
| `CHECK` | The value satisfies a condition | `quantity > 0`; `status IN ('pending', 'paid')` |
| `FOREIGN KEY` | The referenced row exists | An order's customer is a real customer |
| Exclusion | No two rows conflict under some comparison | No overlapping bookings of a room |

Guidance for each:

- **`NOT NULL` by default.** A column that may be null has three states where you probably meant two, and every query on it has to remember. Allow null only when "unknown" or "not applicable" is a real state.
- **`UNIQUE` on every real-world identifier**, considering case: `ada@example.com` and `Ada@Example.com` are one mailbox, so make the index on `lower(email)`.
- **`CHECK` for ranges and small fixed sets.** A status column without one will eventually contain `'payed'`.
- **`FOREIGN KEY` for every reference.** It guarantees there are no orphans, at the price of a lookup on each insert. One thing it does *not* do: "the declaration of a foreign key constraint does not automatically create an index on the referencing columns." Without that index, deleting or re-keying a parent row "will require a scan of the referencing table" ([same page](https://www.postgresql.org/docs/current/ddl-constraints.html#DDL-CONSTRAINTS-FK)). **Index your foreign-key columns.** The example measures the difference: 298 ms against 0.5 ms.

### 4. Types that cannot lie

A type is the cheapest constraint: it rules out whole classes of bad values for free. The PostgreSQL community keeps a list of common mistakes, [*Don't Do This*](https://wiki.postgresql.org/wiki/Don%27t_Do_This); these are the ones that matter most.

| For | Use | Not | Because |
|---|---|---|---|
| A moment in time | `timestamptz` | `timestamp` | `timestamptz` "records a single moment in time" ([wiki](https://wiki.postgresql.org/wiki/Don%27t_Do_This)). A plain `timestamp` is a wall-clock reading with no zone attached — ambiguous the day you have a second timezone or a daylight-saving change. |
| Money | `bigint` in the smallest unit (cents), or `numeric` | `float`, `real`, `money` | Binary floating point cannot represent most decimal fractions. The manual: "If you require exact storage and calculations (such as for monetary amounts), use the numeric type instead" ([docs](https://www.postgresql.org/docs/current/datatype-numeric.html)). In the example, `0.1 + 0.2 = 0.3` is *false* in floating point. |
| Text | `text` | `varchar(50)` | An arbitrary length limit is a future bug with no benefit. If there is a real limit, state it as a `CHECK`. |
| Yes or no | `boolean` | `'Y'`/`'N'`, `0`/`1` | A boolean cannot hold a third value. |
| An auto-generated key | `bigint GENERATED ALWAYS AS IDENTITY` | `serial` | "For new applications, identity columns should be used instead" of `serial` ([wiki](https://wiki.postgresql.org/wiki/Don%27t_Do_This)). |

Always store the currency alongside an amount. `1999` means nothing without `'EUR'`.

### 5. Normalisation: every fact in one place

**Normalisation** is, in Microsoft's plain definition, organising data "to protect the data and to make the database more flexible by eliminating redundancy and inconsistent dependency" ([Microsoft Learn](https://learn.microsoft.com/en-us/troubleshoot/microsoft-365-apps/access/database-normalization-description)). The formal theory has numbered "normal forms"; the working rule fits in a sentence:

> **Each fact is stored once, in the row whose key it is a fact about.**

A customer's email is a fact about the customer, so it lives in `customers` — not copied into each of their orders. The reason is the cost of a copy: "If data that exists in more than one place must be changed, the data must be changed in exactly the same way in all locations" ([same page](https://learn.microsoft.com/en-us/troubleshoot/microsoft-365-apps/access/database-normalization-description)). Miss one and the database now holds two answers to the same question, with nothing to say which is right.

Three patterns that normalisation rules out, and their fixes:

| Smell | Example | Fix |
|---|---|---|
| **A list in one column** | `tags = 'red,cotton,sale'`; `product_ids = '12,15,15'` | A separate table with one row per item. A list in a string cannot be indexed, joined, constrained or counted. |
| **Repeated columns** | `phone1`, `phone2`, `phone3` | A separate table. The fourth phone number should not need a schema change. |
| **A copy of another table's fact** | `orders.customer_email` | Join to `customers` when you need it. |

### 6. Denormalisation: copying on purpose

A fully normalised schema is correct and sometimes slow: a page that needs a count, or data from five tables, pays for the aggregation or the joins on every request. **Denormalisation** is storing a derived or duplicated value so that reads are cheap — and accepting the job of keeping it right.

You have already met one. In Chapter 2.4, the `coupons.used` counter duplicated information that was also in the `redemptions` table (count the rows). It existed because the alternative — counting rows on every redemption — was both slow and impossible to make safe under concurrency.

Common, defensible forms:

- **A counter or total** on the parent row (`posts.comment_count`).
- **A summary table** refreshed periodically (sales per day).
- **A copied column** to avoid a join on a very hot path.

The rules for doing it safely:

1. **Do it for a measured reason.** Denormalise a query that Chapter 2.2's tools have shown to be too slow, not one you suspect might be.
2. **Know which copy is the truth.** One is the source; the other is derived and must be rebuildable from it.
3. **Update both in the same transaction** — or decide, explicitly, how stale the copy may be.
4. **Be able to check and repair it.** A query that recomputes the value and compares belongs in a scheduled job.

**The thing that looks like duplication and is not.** An order line stores the price that was paid. The product also has a price. That is not redundancy: "what this product costs now" and "what this customer paid then" are *different facts* that merely start out equal. If the order looked the price up from the product, every past order would change when the price did. The same goes for the shipping address on an order, and the tax rate on an invoice. **When a row records an event, copy the values as they were at the time.** The example shows it.

### 7. Three recurring questions

**JSON columns.** PostgreSQL's `jsonb` type stores a JSON document in a column. It is the right tool for data whose shape really does vary — the settings of third-party integrations, attributes that differ per product category. It is the wrong tool for avoiding schema design: fields inside JSON get no types and no constraints, and are awkward to index. The manual's own advice is that "even for applications where maximal flexibility is desired, it is still recommended that JSON documents have a somewhat fixed structure" ([PostgreSQL docs, *JSON Types*](https://www.postgresql.org/docs/current/datatype-json.html)). A useful test: if you filter on it, join on it, or have a rule about it, it deserves to be a column.

**Soft deletes.** Marking rows with `deleted_at` instead of deleting them makes undo and auditing easy. It also means *every* query must remember `WHERE deleted_at IS NULL`, and uniqueness must now apply only to live rows — a partial unique index (Chapter 2.2). If you need history rather than undo, moving deleted rows to an archive table keeps the main table honest.

**Who owns the row.** If the system serves many customers from the same tables, every table needs the tenant's ID, and every key and index should begin with it. That is a security boundary as much as a modelling one (Chapter 8.3).

---

## A real example

### Run it: what the schema will and will not let you do

[`code/ch05-modelling/modelling.sql`](code/ch05-modelling/modelling.sql) runs five experiments. Several of its statements are *meant* to fail — the point is to watch the database refuse.

```bash
psql -f phase-2-data/code/ch05-modelling/modelling.sql
```

Output from PostgreSQL 16 on the machine this chapter was written on, lightly trimmed (timings will differ on yours):

```text
==== 1. Types have limits: a 4-byte integer key runs out ====
ERROR:  integer out of range

==== 2. Floating point is not for money ====
 float: 0.1 + 0.2 = 0.3? | numeric: 0.1 + 0.2 = 0.3?
-------------------------+---------------------------
 f                       | t

==== 3. Constraints: the database refuses bad data, whoever sends it ====
-- the same email in different capitals:
ERROR:  duplicate key value violates unique constraint "m_customers_email"
DETAIL:  Key (lower(email))=(ada@example.com) already exists.
-- a negative balance:
ERROR:  new row for relation "m_customers" violates check constraint "m_customers_balance_cents_check"
-- an order for a customer who does not exist:
ERROR:  insert or update on table "m_orders" violates foreign key constraint "m_orders_customer_id_fkey"
DETAIL:  Key (customer_id)=(999999) is not present in table "m_customers".
-- a status nobody defined:
ERROR:  new row for relation "m_orders" violates check constraint "m_orders_status_check"
-- a missing value:
ERROR:  null value in column "total_cents" of relation "m_orders" violates not-null constraint

==== 4. A foreign key does not index the referencing column ====
-- delete one customer who has no orders. The database must first check that:
 Delete on m_customers (actual time=0.030..0.031 rows=0 loops=1)
 Trigger for constraint m_orders_customer_id_fkey: time=297.698 calls=1
 Execution Time: 297.762 ms

-- the same, after indexing orders.customer_id:
 Trigger for constraint m_orders_customer_id_fkey: time=0.473 calls=1
 Execution Time: 0.516 ms

==== 5. Key order: the same 5 million UUIDs, inserted in random order and in sorted order ====
-- UUIDs in random order (what random UUIDs give you):
Time: 20934.241 ms (00:20.934)
-- the same UUIDs in sorted order (what time-ordered UUIDs give you):
Time: 9017.229 ms (00:09.017)
-- 5 million sequential bigints, for comparison:
Time: 7211.830 ms (00:07.212)
        index        |  size
---------------------+--------
 k_bigint_pkey       | 107 MB
 k_random_order_pkey | 191 MB
 k_sorted_order_pkey | 150 MB
```

What to take from each:

1. **`integer out of range`** is the whole of Basecamp's outage in one line. The insert before it, at 2,147,483,647, succeeded.
2. **`0.1 + 0.2 = 0.3` is false in floating point**, true in `numeric`. Now imagine summing a million order totals.
3. **Five different kinds of bad data, five refusals.** None of these checks is in application code. They apply to every writer for ever. Look at the first: the application's "does this email exist?" check would have compared `Ada@Example.com` with `ada@example.com` and found nothing.
4. **Deleting one customer took 298 ms** — the time to scan five million orders looking for references to it — and 0.5 ms once the foreign-key column was indexed. A bulk job deleting a thousand customers would take five minutes or half a second. The plan even names the culprit: `Trigger for constraint m_orders_customer_id_fkey`.
5. **The same five million UUIDs took 20.9 s to insert in random order and 9.0 s in sorted order**, and the random-order index came out 27% larger. Nothing differed but the order of arrival. That is the "index locality" RFC 9562 describes — and this is a mild case, with an index that still fits in memory. The [RFC's](https://www.rfc-editor.org/rfc/rfc9562.html#section-6.11) warning that the effect "can be one order of magnitude or more" is about indexes that do not.

A footnote from the script itself. The identity column in experiment 3 skipped a number: the insert that failed on the duplicate email had already been handed an ID, and it was not given back. Sequences are for uniqueness, not for counting.

### Basecamp, concluded

Two more details from the [postmortem](https://m.signalvnoise.com/postmortem-on-the-read-only-outage-of-basecamp-on-november-9th-2018/) are worth carrying. First, the team knew: "We were actively working on expanding the capacity" of the table, but "failed to properly account for how quickly we were running out of headroom." A known risk with no alarm on it is still a surprise. Second, the fix had been the framework's default for a year — Rails 5.1 moved new tables' keys "from 2,147,483,647 to 9,223,372,036,854,775,807" — but defaults apply to *new* tables. The old one kept the type it was born with.

The general lesson is in that last sentence. A schema remembers every decision that was made the day it was created, and nothing upgrades it for you.

---

## Trade-offs

**Sequential keys or UUIDs.** Sequential keys are smaller and faster, and leak information when exposed. Random UUIDs leak nothing, can be generated anywhere, and cost index size and insert speed. Time-ordered UUIDs recover most of the speed and reveal creation time. There is no best choice, only a fit: internal tables rarely need UUIDs; public identifiers and client-generated keys rarely suit sequences.

**Every constraint costs a little on every write** — a foreign key is an index lookup; a unique constraint is an index to maintain — and saves you from a class of bugs that are otherwise found in production, weeks later, as corrupt data with no clue to its origin. That is nearly always a good trade. The exception is a table taking writes at a rate where the checks are measurably the bottleneck, and you will know when you are there, because you measured.

**Normalised or denormalised.**

| | Normalised | Denormalised |
|---|---|---|
| Writes | Simple: one place to change | Must update every copy, in one transaction |
| Reads | May need joins or aggregation | Cheap |
| Wrong data | Structurally hard | Possible whenever a copy is missed |
| Change the schema later | Easier | Harder: more places depend on the shape |

Start normalised. Denormalise specific things for measured reasons, and write down which copy is the truth.

**Strict now or flexible later.** Loose schemas — nullable columns, free-text fields, JSON for everything — feel fast in the first month, because nothing has to be decided. The decisions do not go away; they move into every piece of code that reads the data, and into the clean-up project two years on. Tightening a schema later is far harder than loosening one, because the bad rows are already there.

---

## Practice

### Check yourself

Answer before reading on.

1. A table gets about 3,000 new rows per second. Its primary key is an `integer` identity column. Roughly how long until inserts start failing?
2. Your application checks `SELECT count(*) FROM users WHERE email = ?` before creating an account, inside a transaction. Give two different ways a duplicate email still gets in.
3. An `orders` row stores `customer_id`, and also the shipping address the customer chose at checkout — copied from the `addresses` table. A colleague says the copy violates normalisation and should be replaced with an `address_id`. Who is right?

<details>
<summary>Answers</summary>

1. 2,147,483,647 ÷ 3,000 per second ≈ 716,000 seconds ≈ **a little over eight days**. If that seems too short to be a realistic mistake: at 30 rows per second it is a little over two years — long enough for everyone who created the table to have moved on. Either way, the day it happens, every insert into that table fails at once.

2. (a) **Two requests at the same moment** both count zero and both insert — check-then-insert, exactly as in Chapters 2.3 and 2.4; being in a transaction does not help at the default isolation level. (b) **A different spelling**: `Ada@Example.com` when `ada@example.com` exists; the comparison is case-sensitive unless you made it otherwise. And (c) **any writer that does not go through that code**: an import script, an admin tool, another service. A unique index on `lower(email)` closes all three.

3. **You are — keep the copy.** The address on an order is a record of where *that order* was sent. If it were only a reference, the order's history would change whenever the customer edited or deleted the address. "Where the customer lives now" and "where we shipped order 8812" are different facts. Copying values at the time of an event is not denormalisation; it is modelling the event correctly.

</details>

### Exercise: review this schema

A colleague has written the schema for a small shop and asks for a review before it goes to production.

```sql
CREATE TABLE users (
    id        serial PRIMARY KEY,
    email     varchar(50),
    name      varchar(50),
    is_admin  varchar(5),          -- 'true' or 'false'
    created   timestamp
);

CREATE TABLE products (
    id     serial PRIMARY KEY,
    name   text,
    price  float,
    tags   text                    -- e.g. 'cotton,sale'
);

CREATE TABLE orders (
    id           serial PRIMARY KEY,
    user_id      int,
    user_email   varchar(50),
    status       text,
    total        float,
    product_ids  text,             -- e.g. '12,15,15'
    created      timestamp
);
```

Find every problem you can — there are at least ten. For each, say what will go wrong in production and how you would fix it. Then write the corrected schema.

<details>
<summary>Worked solution</summary>

**The problems**

| # | Problem | What goes wrong | Fix |
|---|---|---|---|
| 1 | `serial` keys are four-byte integers | The Basecamp outage, eventually | `bigint GENERATED ALWAYS AS IDENTITY` |
| 2 | Almost every column is nullable | Users with no email; orders with no user; code full of null checks, and the one that was forgotten | `NOT NULL` unless absence is meaningful |
| 3 | `users.email` is not unique | Duplicate accounts; password resets going to the wrong one | Unique index on `lower(email)` |
| 4 | `varchar(50)` | A legitimate 51-character email or name is rejected or truncated | `text` |
| 5 | `is_admin` as text | `'True'`, `'yes'`, `'1'`, `''` — and a permission check that compares with `'true'` | `boolean NOT NULL DEFAULT false` |
| 6 | `timestamp` without time zone | Order times that are ambiguous or shift when a server's timezone changes | `timestamptz` |
| 7 | `float` for `price` and `total` | Totals off by a cent; reports that do not reconcile | `bigint` cents (plus a currency, if there is ever more than one) |
| 8 | `tags` as a comma-separated string | "All products tagged sale" is an unindexable string search; `'presale'` matches `'sale'` | A `tags` table and a `product_tags` join table |
| 9 | `orders.user_id` has no foreign key | Orders pointing at users who do not exist | `REFERENCES users`, with an index |
| 10 | `orders.user_email` copies `users.email` | The user changes their email; old orders show the old one — or half of them do | Remove it; join to `users` |
| 11 | `status` is free text | `'payed'`, `'Paid'`, `'PAID'` | `CHECK (status IN (…))` |
| 12 | `product_ids` as a string | No quantities, no way to join to products, no guarantee the products exist | An `order_items` table |
| 13 | **Nothing records the price paid** | When a product's price changes, the meaning of every past order's total changes with it — or cannot be recomputed | `order_items.unit_price_cents`, copied at purchase |
| 14 | `total` stored with nothing to derive it from | Cannot be checked or repaired | Compute it from the items — or, if stored for speed, treat it as a derived copy and maintain it in the same transaction |

Problem 13 is the one most reviews miss, because it is something *absent*. It is also the only place where the right answer is to copy a value.

**The corrected schema** is in [`code/ch05-modelling/review_solution.sql`](code/ch05-modelling/review_solution.sql), which also loads a few rows and demonstrates the point about prices:

```bash
psql -f phase-2-data/code/ch05-modelling/review_solution.sql
```

```sql
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
```

```text
==== The order total, computed from its items ====
 order_id | total_cents
----------+-------------
        1 |        5796

==== The price of the T-shirt goes up. The old order does not change. ====
 order_id | total_cents
----------+-------------
        1 |        5796
```

**Things to notice in the corrected version**

- The three tables became six. That is normal: lists hidden inside strings become tables of their own.
- Each foreign key has an index that starts with it. `orders_user_newest` does double duty: it serves "this user's recent orders" (Chapter 2.2) *and* indexes the foreign key.
- The composite primary key on `order_items (order_id, product_id)` is itself a rule: a product appears at most once per order; buy two by setting `quantity`.
- Nothing here would need to change for the application to work on day one. Every item is cheap now and expensive later.

</details>

---

## Production tips and common mistakes

**Tips**

- **`bigint` keys, always.** And alert when any sequence passes half of its type's range — for the tables that already exist.
- **Constraints first, validation second.** Write the `NOT NULL`, `UNIQUE`, `CHECK` and foreign keys with the table, not after the first incident.
- **Index every foreign-key column** — often as the leading column of an index you need anyway.
- **`timestamptz` for moments, integer minor units for money, `text` for text, `boolean` for booleans.**
- **Give every table `created_at`**, and `updated_at` where rows change. You will want them for debugging long before you want them for features.
- **Copy values into the record of an event** — prices, addresses, rates — as they were at the time.
- **Keep public IDs separate from internal ones** where enumeration or volume matters.
- **When you denormalise, write down which copy is the truth, and schedule a check** that recomputes and compares.
- **Name things consistently**: one convention for tables, keys, timestamps and booleans, everywhere. It costs nothing and removes a class of mistakes.

**Common mistakes**

- **A four-byte integer key** on a table that turns out to be busy.
- **Relying on application validation for uniqueness.**
- **Nullable by default**, because nobody decided otherwise.
- **Floats for money.**
- **Lists, or JSON, in a column** to avoid creating a table — for data you then need to query.
- **A foreign key with no index**, discovered when a delete takes minutes and locks a table.
- **Denormalising first**, before any query was measured, then living with the inconsistencies.
- **Looking up the current price, address or name** when displaying a past event.
- **Expecting identity values to be consecutive.**
- **Random UUIDs as the primary key of a very large, insert-heavy table**, without having weighed the cost.
- **Exposing sequential IDs** and treating their obscurity as access control.

---

## Key takeaways

- A schema's mistakes are patient and expensive: they work for years and then fail all at once, and the fix gets harder with every row.
- Use surrogate `bigint` keys. Use UUIDs where IDs are public or created outside the database — time-ordered ones if the table is large.
- Constraints make invalid data impossible for every writer, including the ones not yet written. Application validation cannot.
- A foreign key does not index its own column. Do it yourself.
- Choose types that cannot hold wrong values: `timestamptz`, integer cents or `numeric`, `text`, `boolean`.
- Normalise by default: each fact once. Denormalise on purpose, for a measured reason, knowing which copy is the truth.
- Copying a value into the record of an event is not duplication. It is history.

**Next:** [2.6 The database as a shared resource](06-database-as-shared-resource.md) — connections, locks and timeouts, and how one query takes down everything else.

---

## Sources

- David Heinemeier Hansson, [*Postmortem on the read-only outage of Basecamp on November 9th, 2018*](https://m.signalvnoise.com/postmortem-on-the-read-only-outage-of-basecamp-on-november-9th-2018/), Signal v. Noise.
- [RFC 9562](https://www.rfc-editor.org/rfc/rfc9562.html) — Universally Unique IDentifiers (UUIDs), 2024.
- PostgreSQL documentation: [Constraints](https://www.postgresql.org/docs/current/ddl-constraints.html); [Numeric Types](https://www.postgresql.org/docs/current/datatype-numeric.html); [Sequence Manipulation Functions](https://www.postgresql.org/docs/current/functions-sequence.html); [UUID Functions](https://www.postgresql.org/docs/current/functions-uuid.html); [JSON Types](https://www.postgresql.org/docs/current/datatype-json.html).
- PostgreSQL wiki: [*Don't Do This*](https://wiki.postgresql.org/wiki/Don%27t_Do_This).
- Ruby on Rails Guides: [Active Record Validations — uniqueness](https://guides.rubyonrails.org/active_record_validations.html#uniqueness).
- Microsoft Learn: [Description of the database normalization basics](https://learn.microsoft.com/en-us/troubleshoot/microsoft-365-apps/access/database-normalization-description).
- OWASP API Security Top 10 (2023): [API1 — Broken Object Level Authorization](https://owasp.org/API-Security/editions/2023/en/0xa1-broken-object-level-authorization/).
