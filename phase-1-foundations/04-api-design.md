# 1.4 API design: resources, errors, pagination, and versioning

[Syllabus](../SYLLABUS.md) · Phase 1 · Previous: [1.3 HTTP for backend engineers](03-http-for-backend-engineers.md) · Next: [1.5 Timeouts, retries, and idempotency](05-timeouts-retries-idempotency.md)

> **Time:** about 55 minutes to read, 60 minutes of practice.
> **You need:** [Chapter 1.3](03-http-for-backend-engineers.md) (methods, status codes, ETags). Basic SQL helps for the pagination section.

**By the end of this chapter you can**

- model an API as resources with a small, predictable set of operations;
- design an error response that a program — not just a person — can act on;
- implement cursor pagination, and explain exactly why offset pagination gets slow and shows users the wrong rows;
- tell a compatible change from a breaking one, and choose how to version;
- put a limit on everything a client is allowed to ask for.

---

## The problem

An API is the part of your system that is hardest to change, because the code that depends on it is not yours to redeploy. Stripe's engineers describe it as "a contract for communication that can't be changed without considerable cooperation and effort" ([Stripe blog, *APIs as infrastructure*](https://stripe.com/blog/api-versioning)). Two things happen to every API that succeeds, and both break designs that looked fine on the first day.

**The data grows.** *An illustrative scenario:* `GET /orders` returns all of a shop's orders as one JSON array. With 50 orders nobody thinks about it. The response grows with every sale, and one day a single request reads five million rows, builds a response of several gigabytes in memory, and takes the server down with it. Slack tells the real version of this story: "Endpoints that were designed around the expectation that they would, in the most extreme cases, return several hundred records, are now returning hundreds of thousands of records" ([Slack Engineering, *Evolving API Pagination at Slack*](https://slack.engineering/evolving-api-pagination-at-slack/)).

So you add `?page=3&per_page=20`. That holds until somebody asks for page 200,000. On a five-million-row table in PostgreSQL, the first page of twenty orders takes 0.03 ms. A page four million rows in takes 454 ms — fifteen thousand times longer, for the same twenty rows. (You will reproduce that measurement below.)

**The clients get old.** *An illustrative scenario:* you rename the response field `name` to `full_name`. The website is updated in the same deploy. The mobile app is not: version 3.1 is installed on phones whose owners will not update for a year, and from the moment you deploy, it crashes on launch.

This chapter is about designing for those two futures: data a million times larger, and clients years older than your server.

---

## The concept

### 1. Model resources, not actions

Chapter 1.3's review exercise had endpoints called `createUser`, `getUser` and `deleteUser`. That style — one URL per action — grows without limit and no two endpoints behave alike. The alternative is **resource-oriented design**, which Google's API guidelines summarise as two ideas: "the fundamental building blocks of an API are individually-named resources (nouns)", and "a small number of standard methods (verbs) provide the semantics for most common operations" ([AIP-121](https://google.aip.dev/121)).

In HTTP terms: URLs name *things*, and the HTTP methods are the verbs.

| Operation | Request | Success response |
|---|---|---|
| List | `GET /orders` | `200` with one page of orders |
| Create | `POST /orders` | `201` with a `Location` header |
| Read one | `GET /orders/{id}` | `200` |
| Replace | `PUT /orders/{id}` | `200` or `204` |
| Change part | `PATCH /orders/{id}` | `200` |
| Delete | `DELETE /orders/{id}` | `204` |

A client that has learned one resource in your API has learned them all. That predictability is the whole point.

A few conventions carry most of the value:

- **Collections are plural nouns** (`/orders`, not `/order` or `/getOrders`) — see [AIP-122](https://google.aip.dev/122).
- **Nest only to show ownership**, and keep it shallow: `/customers/{id}/orders` is clear; four levels deep is not.
- **Actions that are not create, read, update or delete** come in two flavours. Often the action is really a new thing: refunding an order *creates a refund*, so `POST /orders/{id}/refunds`. When that is forced, use an explicit verb on the resource; Google's convention is a colon, as in `POST /orders/{id}:cancel` ([AIP-136](https://google.aip.dev/136)).
- **Be boringly consistent.** The same field names, the same timestamp format, the same pagination and the same error shape on every endpoint. When Slack redesigned its pagination, one of its stated requirements was that "it should be a consistent experience to paginate through different endpoints" ([Slack Engineering](https://slack.engineering/evolving-api-pagination-at-slack/)).

### 2. Errors a program can act on

The status code (Chapter 1.3) tells the client the *class* of failure. The body has to tell it the rest. A good error body has:

- a **stable, machine-readable identifier** that code can branch on;
- a **human-readable message** for whoever reads the logs;
- for validation failures, **which fields** were wrong and why — all of them, not just the first.

You do not need to invent the format. [RFC 9457, *Problem Details for HTTP APIs*](https://www.rfc-editor.org/rfc/rfc9457.html), defines one "to carry machine-readable details of errors in HTTP response content to avoid the need to define new error response formats". This example is taken from it:

```http
HTTP/1.1 422 Unprocessable Content
Content-Type: application/problem+json

{
  "type": "https://example.net/validation-error",
  "title": "Your request is not valid.",
  "errors": [
    { "detail": "must be a positive integer",        "pointer": "#/age" },
    { "detail": "must be 'green', 'red' or 'blue'",  "pointer": "#/profile/color" }
  ]
}
```

`type` is the identifier a client switches on. `title` is the same for every error of that type; `detail` describes this occurrence. The standard members are `type`, `status`, `title`, `detail` and `instance`, and you may add your own, as `errors` is added here.

The rule that matters more than the format: **clients must never have to parse your English.** If the only way to distinguish "email already in use" from "email malformed" is to match the message text, then the message text is now part of your API and you can never reword it. Google's guidelines require an identifier in every error for this reason — "so that users can write code against specific aspects of the error" ([AIP-193](https://google.aip.dev/193)).

### 3. Pagination

**Rule zero: every endpoint that returns a list is paginated from its first release.** Adding it later is a breaking change. Google's guidelines say so directly: list methods "must provide pagination at the outset, as it is a backwards-incompatible change to add pagination to an existing method" ([AIP-158](https://google.aip.dev/158)). Slack, having learned it the hard way, ends its article with: "if you're wondering whether or not you should paginate an endpoint that returns a list of data, no matter how small the dataset seems, we recommend you do" ([Slack Engineering](https://slack.engineering/evolving-api-pagination-at-slack/)).

There are two ways to do it.

#### Offset pagination: "skip N, take 20"

The client sends a page number or an offset; the server runs `… ORDER BY created_at DESC LIMIT 20 OFFSET 4000000`. It is the obvious design, it lets a user jump to page 37, and it has two defects that cannot be fixed.

**Defect one: the cost grows with the depth.** The database cannot jump to row 4,000,001. It has to produce the first four million rows and throw them away. PostgreSQL's manual: "The rows skipped by an OFFSET clause still have to be computed inside the server; therefore a large OFFSET might be inefficient" ([PostgreSQL docs, *LIMIT and OFFSET*](https://www.postgresql.org/docs/current/queries-limit.html)). Slack's summary: "As the offset increases the farther you go within the dataset, the database still has to read up to offset + count rows" ([Slack Engineering](https://slack.engineering/evolving-api-pagination-at-slack/)).

**Defect two: the pages shift while the client is reading them.** Offsets count from the top of a list that is changing. If new rows arrive at the top between two requests, everything moves down and the client sees rows it has already seen. If a row is deleted, everything moves up and the client *never sees* one row. Slack describes it: "After requesting the first page of 10 messages, 10 messages are added to the channel. Now when we fetch the second page, we see the same results from the first page" ([Slack Engineering](https://slack.engineering/evolving-api-pagination-at-slack/)).

#### Cursor pagination: "the 20 after the last one I saw"

Instead of a position, the client sends back a marker for the last item it received. The server uses it to *seek*:

```sql
SELECT id, customer_id, total_cents, created_at
FROM orders
WHERE (created_at, id) < ('2024-01-12 13:46:41+00', 1000001)   -- the last row the client saw
ORDER BY created_at DESC, id DESC
LIMIT 20;
```

With an index on `(created_at DESC, id DESC)` the database goes straight to that point in the index and reads twenty entries. It does not matter whether the page is the first or the two-hundred-thousandth. This technique is also called **keyset pagination** or the *seek method* ([Markus Winand, *We need tool support for keyset pagination*](https://use-the-index-luke.com/no-offset)).

```text
Index on (created_at DESC, id DESC)      newest ───────────────────────────────► oldest

OFFSET 4,000,000 LIMIT 20    [ read 4,000,000 entries and throw them away ..........][20]

Cursor (after the last seen)                                   jump straight here ──►[20]
```

Because the marker is a *value*, not a position, rows added or removed elsewhere in the list do not move it. Both defects are gone.

Three details make it work:

- **The sort order must be total.** If you sort only by `created_at` and two orders share a timestamp, the boundary between pages is ambiguous and a row can be skipped or repeated. Add a unique column as a tie-breaker — hence `(created_at, id)`. PostgreSQL's manual makes the general point: use "an ORDER BY clause that constrains the result rows into a unique order. Otherwise you will get an unpredictable subset of the query's rows" ([PostgreSQL docs](https://www.postgresql.org/docs/current/queries-limit.html)).
- **The cursor is opaque.** Hand the client an encoded string and promise nothing about its contents. [AIP-158](https://google.aip.dev/158): page tokens "must be opaque (but URL-safe) strings, and must not be user-parseable. This is because if users are able to deconstruct these, they will do so." An opaque cursor lets you change what is inside it later. Slack does the same, "using Base64 to encode information on how to retrieve the next set of results within the cursor" ([Slack Engineering](https://slack.engineering/evolving-api-pagination-at-slack/)).
- **The page size has a default and a maximum.** A missing `limit` gets the default. An excessive one is reduced: "the API should coerce down to the maximum permitted page size" ([AIP-158](https://google.aip.dev/158)).

A response then looks like this, with `next_cursor` absent or empty on the last page:

```json
{
  "items": [ { "id": 7, "title": "…" }, { "id": 6, "title": "…" } ],
  "next_cursor": "eyJhZnRlcl9pZCI6Nn0"
}
```

What you give up is on [Slack's list of trade-offs](https://slack.engineering/evolving-api-pagination-at-slack/): "There is no concept of the total number of pages or results in the set. The client can't jump to a specific page." For feeds, exports, and "load more" buttons, nobody misses either.

### 4. Compatibility and versioning

Once anyone depends on your API, every change falls into one of two groups.

| Usually safe (additive) | Breaking |
|---|---|
| A new endpoint | Removing or renaming a field or endpoint |
| A new *optional* request field or query parameter | Changing a field's type or meaning |
| A new field in a response | Making an optional request field required |
| | Changing a default (page size, sort order) |
| | Tightening validation of something previously accepted |

Stripe's rule for the right-hand column is short: "Fields that were present before should stay present, and fields should always preserve their same type and name"; while "it's safe to add a new API endpoint, or a new field to an existing API endpoint that was never present before" ([Stripe blog](https://stripe.com/blog/api-versioning)). Google's guidance has the same shape: new fields and methods may be added within a version, but "new required fields must not be added to existing request messages" ([AIP-180](https://google.aip.dev/180)).

Two observations explain why the "safe" column says *usually*.

**Clients must be tolerant for additions to be safe.** Adding a response field only works if clients ignore fields they do not recognise. Martin Fowler calls this the *Tolerant Reader*: "be as tolerant as possible when reading data from a service… only take the elements you need, ignore anything you don't" ([martinfowler.com](https://martinfowler.com/bliki/TolerantReader.html)). A client that rejects unknown fields has made every future addition a breaking change. Say in your documentation that new fields and new enumeration values may appear at any time, so client authors are told to expect them.

**Anything observable becomes a dependency.** This is *Hyrum's Law*: "With a sufficient number of users of an API, it does not matter what you promise in the contract: all observable behaviors of your system will be depended on by somebody" ([hyrumslaw.com](https://www.hyrumslaw.com/)). The order of an unsorted list, the wording of an error, the fact that IDs happen to be ascending numbers — someone will build on it. This is the deeper reason for opaque cursors and machine-readable error types: they shrink what can be observed.

**When you must break something, you version.** The common strategies:

| Strategy | Looks like | Good | Bad |
|---|---|---|---|
| Major version in the path | `/v1/orders`, `/v2/orders` | Obvious; easy to route and to cache | Coarse — encourages rare, enormous "v2" rewrites that clients put off for years |
| Version in a header, pinned per client | `Stripe-Version: 2017-05-24` | Many small steps; each client stays on the behaviour it was built against | The server has to produce every old behaviour indefinitely |
| Never break; only add | — | Simplest for everyone | Old mistakes stay in the API for ever |

Whichever you choose, **removing something is a process, not a deploy**: announce it, measure who still uses it, and give them a date. Two standard response headers exist to carry the announcement in-band: `Deprecation` ([RFC 9745](https://www.rfc-editor.org/rfc/rfc9745.html)) says a resource "will be or has been deprecated", and `Sunset` ([RFC 8594](https://www.rfc-editor.org/rfc/rfc8594.html)) says it "is likely to become unresponsive at a specified point in the future".

### 5. Limits on everything

Look back at the two failures in "The problem". Both come from something a client was allowed to make arbitrarily large. The defence is a habit: **every input has a maximum.**

- Page size (above).
- Request body size (`413 Content Too Large`, Chapter 1.3).
- The number of items in any array a client sends.
- The length of every string.
- How long a request may run (Chapter 1.5).
- How many requests a client may send (Chapter 6.4).

An API without limits has a hidden one: the point at which the server falls over.

---

## A real example

### Run it: pages that shift, and pages that do not

[`code/ch04-pagination/main.go`](code/ch04-pagination/main.go) serves the same list — items 9 down to 1, newest first — through an offset endpoint and a cursor endpoint. A client reads page 1, the data changes, and the client reads page 2.

```bash
go run ./phase-1-foundations/code/ch04-pagination
```

```text
=== Offset pagination ===
two new items arrive between page 1 and page 2
  page 1: [9 8 7]
  page 2: [8 7 6]   <- item 8 shown twice
item 9 is deleted between page 1 and page 2
  page 1: [9 8 7]
  page 2: [5 4 3]   <- item 6 never shown

=== Cursor pagination ===
two new items arrive between page 1 and page 2
  page 1: [9 8 7]
  page 2: [6 5 4]   <- correct
item 9 is deleted between page 1 and page 2
  page 1: [9 8 7]
  page 2: [6 5 4]   <- correct

=== A machine-readable error ===
400 Bad Request
Content-Type: application/problem+json
{"type":"https://api.example.com/problems/invalid-cursor","title":"Invalid cursor","status":400,"detail":"The cursor is not one this API issued. Start again without a cursor."}
```

The repeated item is merely annoying. The item that is *never shown* is a correctness bug: an export job or a sync built on that endpoint silently loses data.

The cursor is nothing mysterious — a small structure, encoded so that clients treat it as a black box:

```go
// A cursor says where the next page starts. Clients must treat it as an
// opaque string: today it holds the last id seen, tomorrow it may hold
// something else, and no client should break when that changes.
type cursor struct {
    AfterID int `json:"after_id"`
}

func encodeCursor(c cursor) string {
    raw, _ := json.Marshal(c)
    return base64.RawURLEncoding.EncodeToString(raw)
}
```

and the page size is defended in one place:

```go
n, err := strconv.Atoi(raw)
if err != nil || n < 1 {
    return 0, fmt.Errorf("limit must be a positive integer, got %q", raw)
}
return min(n, maxLimit), nil
```

### Run it: what a deep offset costs in a real database

[`code/ch04-pagination/offset_vs_keyset.sql`](code/ch04-pagination/offset_vs_keyset.sql) builds a five-million-row `orders` table in PostgreSQL, with the right index, and asks for "the next 20 orders" three ways. Run it against any scratch PostgreSQL database:

```bash
psql -f phase-1-foundations/code/ch04-pagination/offset_vs_keyset.sql
```

This is the output from PostgreSQL 16 on the machine this chapter was written on. Your times will differ; the row counts will not.

```text
==== 1. OFFSET pagination, first page (OFFSET 0) ====
 Limit (actual rows=20 loops=1)
   ->  Index Scan using orders_newest_first on orders (actual rows=20 loops=1)
 Execution Time: 0.029 ms

==== 2. OFFSET pagination, a deep page (OFFSET 4,000,000) ====
 Limit (actual rows=20 loops=1)
   ->  Index Scan using orders_newest_first on orders (actual rows=4000020 loops=1)
 Execution Time: 454.432 ms

==== 3. Keyset pagination, the same deep page ====
 Limit (actual rows=20 loops=1)
   ->  Index Scan using orders_newest_first on orders (actual rows=20 loops=1)
         Index Cond: (ROW(created_at, id) < ROW('2024-01-12 13:46:41+00'::timestamp with time zone, 1000001))
 Execution Time: 0.085 ms
```

Read the `actual rows` on the inner line of each plan. To return twenty rows, the deep offset query walked **4,000,020** index entries; the keyset query walked **20**. Both used the same index. The index is not the difference — the *question* is. "Skip four million" cannot be answered without counting four million; "everything before this value" can.

Now think about a client that reads the whole table page by page. With offsets, each page costs more than the one before, so the total work grows with the *square* of the table size. With a cursor every page costs the same.

### How Stripe changes an API without breaking anyone

Stripe's API has had to change continuously since 2011 while old integrations keep running. Their published approach ([*APIs as infrastructure: future-proofing Stripe with versioning*](https://stripe.com/blog/api-versioning)) is worth knowing because it shows the header-pinning strategy taken seriously:

- Versions are small and frequent: "rolling versions that are named with the date they're released (for example, 2017-05-24)".
- Each account is frozen at the behaviour it first saw: "The first time a user makes an API request, their account is automatically pinned to the most recent version available, and from then on, every API call they make is assigned that version implicitly."
- A client can opt in per request "by manually setting the Stripe-Version header", and upgrade when it is ready.
- Inside Stripe, the code implements only the *current* behaviour. For an older client, the response is produced at the current version and then passed backwards through a chain of "version change modules", each of which undoes one breaking change, "until that target version is reached".

The result, as of [that article](https://stripe.com/blog/api-versioning): "we've maintained compatibility with every version of our API since the company's inception in 2011." The cost is equally clear — every one of those change modules is code that has to keep working.

---

## Trade-offs

**Cursor or offset?**

| | Offset | Cursor |
|---|---|---|
| Cost of page *n* | Grows with *n* | Constant |
| Data changes between pages | Duplicates and missed rows | Stable |
| Jump to page 37 | Yes | No |
| "Page 3 of 212" | Yes, if you also pay for a count | No |
| Needs | Nothing special | A unique, indexed sort order |
| Effort | Trivial | A little more |

Offset is acceptable when the list is small and will stay small, or when a human really needs numbered pages in an internal tool. For anything a program will read to the end — feeds, exports, synchronisation — use a cursor. Zalando's public API guidelines put it as a rule: "prefer cursor-based pagination, avoid offset-based pagination" ([Zalando RESTful API Guidelines](https://opensource.zalando.com/restful-api-guidelines/)).

**Total counts are more expensive than they look.** "Showing 1–20 of 4,812,112" requires counting every matching row on every request, and in PostgreSQL "a full count of rows in a table can be comparatively slow" ([PostgreSQL wiki, *Slow Counting*](https://wiki.postgresql.org/wiki/Slow_Counting)). [Zalando's guidelines](https://opensource.zalando.com/restful-api-guidelines/) advise against returning one at all, "since calculating it is a costly operation that is usually not required by clients". If the interface needs a number, an estimate is usually enough.

**Resources or RPC?** Resource modelling makes the common cases uniform and the odd cases awkward (is "merge these two accounts" a resource?). Accept a few explicit action endpoints rather than contorting the model. The goal is predictability, not purity.

**A standard error format or your own?** Either is fine; having two is not. RFC 9457 saves you a design discussion and gives client libraries something to recognise.

**Every version you keep alive is code you maintain.** [Stripe's article](https://stripe.com/blog/api-versioning) names the cost of supporting old behaviour: "engineering time is diverted to maintaining old code." Path versioning makes each break cheap for the server and expensive for clients; pinned versions do the opposite. The cheapest version to maintain is the one you never needed, which is the argument for getting additive-only as far as it will go.

---

## Practice

### Check yourself

Answer before reading on.

1. A client pages through a newest-first list using `offset` and a page size of 10. Between its request for page 1 and its request for page 2, five new items are created. What exactly does the client see on page 2?
2. Which of these changes can break an existing client, and why? (a) Adding a field `currency` to a response. (b) Renaming `name` to `full_name`. (c) Making the optional request field `country` required. (d) Adding a new value `"paused"` to the `status` field in responses. (e) Changing the default page size from 100 to 20.
3. Your cursor query is `WHERE created_at < $1 ORDER BY created_at DESC LIMIT 20`. What goes wrong, and when?

<details>
<summary>Answers</summary>

1. The five new items push everything down by five. Offset 10 now points five places earlier in the old ordering, so **the first five items on page 2 are the last five items of page 1 again**, followed by five items the client has not seen.

2. (a) Safe — for clients that ignore unknown fields. A client that validates responses strictly will break, which is why you tell client authors to be tolerant. (b) Breaking: every client reading `name` now gets nothing. (c) Breaking: every client that omits `country` starts getting `400`s. (d) Risky: a client with an exhaustive `switch` over the old values will hit its error branch. Additions like this are only safe if the documentation said from the start that new values may appear. (e) Breaking in behaviour, even though no field changed: a client that assumed "one request returns everything, because I have fewer than 100" now silently processes only the first 20. This is AIP-158's own example of why pagination must exist from the outset.

3. `created_at` is not unique. If the last row of a page and the first row of the next share a timestamp, `created_at < $1` skips every remaining row with that timestamp — silently, and only when timestamps collide, so it will pass every test and fail in production during a bulk import. Order by `(created_at, id)` and compare the pair.

</details>

### Exercise: design a list endpoint

You own the orders service. The `orders` table has five million rows and is growing. The current endpoint is:

```text
GET /orders?customer_id=42&page=3&per_page=50      → {"orders": [...], "total": 18233}
```

It has become slow for large customers, and an integration partner has reported that their nightly sync sometimes misses orders.

Design its replacement:

1. The request (path, parameters, limits) and the response shape.
2. The SQL and the index it needs.
3. What the cursor contains.
4. The error cases and their status codes.
5. How you roll it out without breaking the clients that use `page` today.

<details>
<summary>Worked solution</summary>

**1. Request and response**

```text
GET /v1/customers/42/orders?limit=50&cursor=eyJ…&status=shipped
```

- The customer is in the path: orders belong to a customer, and the path makes "whose orders?" impossible to forget.
- `limit`: default 50, maximum 200; larger values are reduced to 200; non-numeric or below 1 is a `400`.
- `cursor`: opaque; absent on the first request.
- `status`: an optional filter.

```json
{
  "items": [ { "id": "ord_9f3…", "status": "shipped", "total_cents": 4599, "currency": "EUR", "created_at": "2024-03-01T09:30:00Z" } ],
  "next_cursor": "eyJ…"
}
```

- `next_cursor` is absent on the last page. The client's loop is simply "repeat until there is no `next_cursor`".
- No `total`. If the interface needs "about 18,000 orders", provide that as a separate, cached number.

**2. SQL and index**

```sql
SELECT id, status, total_cents, currency, created_at
FROM orders
WHERE customer_id = $1
  AND (created_at, id) < ($2, $3)          -- omitted on the first page
ORDER BY created_at DESC, id DESC
LIMIT $4;

CREATE INDEX orders_by_customer_newest
    ON orders (customer_id, created_at DESC, id DESC);
```

The index starts with the equality column (`customer_id`), followed by the sort columns in the sort's order. The database finds the customer's entries, seeks to the cursor position, and reads `limit` entries — the plan from the experiment above. (Chapter 2.2 explains why the column order matters.)

A practical trick: fetch `limit + 1` rows. If the extra row comes back, there is a next page; return `limit` rows and build the cursor from the last one returned. That avoids handing out a `next_cursor` that leads to an empty page.

**3. What is in the cursor**

`(created_at, id)` of the last item returned, plus the filters the list was requested with (here `status`). If a client sends a cursor with *different* filters from the ones it was issued for, reject it — otherwise it resumes at a position that means nothing in the new list. Encode the lot as base64 so clients treat it as opaque. [Zalando's guidelines](https://opensource.zalando.com/restful-api-guidelines/) describe the same contents: the cursor "encodes the page position… and the applied query filters".

**4. Errors** (all as `application/problem+json`)

| Situation | Status | `type` |
|---|---|---|
| `limit` is not a positive integer | `400` | `…/invalid-parameter` |
| `cursor` cannot be decoded, or was issued for different filters | `400` | `…/invalid-cursor` |
| The caller may not see customer 42 | `404` (or `403`; see Chapter 1.3) | `…/not-found` |
| No orders | `200` with `"items": []` — an empty list is not an error | — |

**5. Rolling it out**

This is a new, additive capability first and a removal much later:

1. Add `cursor` and `limit` to the existing endpoint (or add the new path alongside the old). Existing calls with `page` keep working, unchanged.
2. A request with both `page` and `cursor` is a `400`.
3. Tell the partner whose sync is missing orders to switch — the cursor fixes their bug.
4. Cap how deep `page` may go, at a depth nobody is using, so the worst case stops getting worse.
5. Mark the `page` parameter deprecated: in the documentation, and with `Deprecation` and `Sunset` response headers on requests that use it.
6. Watch the usage of `page` fall. Contact whoever is left. Remove it only when it is unused, or in the next major version.

[Slack](https://slack.engineering/evolving-api-pagination-at-slack/) did the same thing at larger scale, and named the property that made it possible: their new scheme does "forward pagination through an entire dataset in a backwards compatible way".

</details>

---

## Production tips and common mistakes

**Tips**

- **Paginate every list from day one, and cap the page size.**
- **Give every error a stable identifier and every response a request ID.** When a customer reports a failure, the ID is how you find it in your logs (Phase 7).
- **Sort by something unique, and index it.**
- **Keep cursors opaque.** If a client must not be able to forge one, sign or encrypt it.
- **Add; do not change.** A new field with a new name is free. Changing what an old field means breaks every client at once.
- **Before you remove anything, find out who uses it.** Log which clients call which endpoints with which parameters. You cannot deprecate what you cannot measure.
- **Design the API before the code.** Write the description — endpoints, fields, errors — and have someone else review it. [Zalando](https://opensource.zalando.com/restful-api-guidelines/) makes this a rule: "define APIs first, before coding its implementation".
- **Pick formats once.** Timestamps in [RFC 3339](https://www.rfc-editor.org/rfc/rfc3339.html) form, in UTC (`2024-03-01T09:30:00Z`). Money as a whole number of the currency's smallest unit plus a currency code — Stripe's API expects amounts "in the currency's minor unit", so that 10.99 USD is sent as `1099` ([Stripe docs](https://docs.stripe.com/currencies)).

**Common mistakes**

- **A list endpoint with no limit.** It works until the day it takes the service down.
- **Offset pagination behind an export or a sync.** Slow, and it loses rows.
- **Sorting by a column that is not unique.**
- **Returning a total count by default** on a large table.
- **A cursor that clients can read and construct.** They will, and then its format is frozen.
- **Making clients parse error messages.**
- **"It's not a breaking change, it's a bug fix."** If clients depended on the old behaviour, it is breaking — Hyrum's Law does not care what you intended.
- **Leaking your database into the API**: column names as field names, auto-increment keys as public IDs. Every such leak makes a future schema change into an API change.

---

## Key takeaways

- An API is a contract with code you cannot redeploy. Design it for data a million times bigger and clients years older.
- URLs name things; HTTP methods are the verbs; every endpoint behaves like every other.
- An error needs a status code, a stable machine-readable type, and a human message. RFC 9457 is a ready-made format.
- Paginate every list from the start. A deep `OFFSET` reads and discards every skipped row, and shifts under changing data; a cursor seeks in constant time and does not shift.
- Additive changes are safe if clients are tolerant; everything else is breaking. Anything observable will be depended on.
- Put a maximum on every input.

**Next:** [1.5 Timeouts, retries, and idempotency](05-timeouts-retries-idempotency.md) — making `POST` safe to send twice.

---

## Sources

- Slack Engineering: [*Evolving API Pagination at Slack*](https://slack.engineering/evolving-api-pagination-at-slack/).
- Stripe: [*APIs as infrastructure: future-proofing Stripe with versioning*](https://stripe.com/blog/api-versioning); [API pagination reference](https://docs.stripe.com/api/pagination); [currencies and amounts](https://docs.stripe.com/currencies).
- Google API Improvement Proposals: [AIP-121 Resource-oriented design](https://google.aip.dev/121), [AIP-122 Resource names](https://google.aip.dev/122), [AIP-136 Custom methods](https://google.aip.dev/136), [AIP-158 Pagination](https://google.aip.dev/158), [AIP-180 Backwards compatibility](https://google.aip.dev/180), [AIP-193 Errors](https://google.aip.dev/193).
- [Zalando RESTful API and Event Guidelines](https://opensource.zalando.com/restful-api-guidelines/).
- PostgreSQL documentation: [LIMIT and OFFSET](https://www.postgresql.org/docs/current/queries-limit.html); PostgreSQL wiki: [Slow Counting](https://wiki.postgresql.org/wiki/Slow_Counting).
- Markus Winand, [*We need tool support for keyset pagination*](https://use-the-index-luke.com/no-offset), Use The Index, Luke.
- [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457.html) — Problem Details for HTTP APIs. [RFC 9745](https://www.rfc-editor.org/rfc/rfc9745.html) — The Deprecation HTTP Response Header Field. [RFC 8594](https://www.rfc-editor.org/rfc/rfc8594.html) — The Sunset HTTP Header Field. [RFC 3339](https://www.rfc-editor.org/rfc/rfc3339.html) — Date and Time on the Internet: Timestamps.
- Martin Fowler, [*Tolerant Reader*](https://martinfowler.com/bliki/TolerantReader.html).
- Hyrum Wright, [*Hyrum's Law*](https://www.hyrumslaw.com/).
