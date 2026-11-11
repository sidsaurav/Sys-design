# 2.8 Choosing a datastore

[Syllabus](../SYLLABUS.md) · Phase 2 · Previous: [2.7 Changing schemas without downtime](07-schema-changes.md) · Next: 2.9 Durability, backups, and restore *(planned)*

> **Time:** about 45 minutes to read, 45 minutes of practice.
> **You need:** [Chapter 2.1](01-how-databases-store-data.md) (how a storage engine is organised), [Chapter 2.2](02-indexes-and-query-plans.md) (indexes) and [Chapter 2.5](05-data-modelling.md) (modelling). One example is plain Go; the other needs a PostgreSQL — see [the phase overview](README.md).

**By the end of this chapter you can**

- describe a workload by its access patterns — the handful of facts that decide which store fits;
- say what each family of datastore is built for, and what it gives up to be good at it: relational, key-value, document, wide-column, search, columnar, and object storage;
- explain why transactions and analytics want opposite storage layouts;
- argue for one relational database as the starting point, and name the evidence that justifies adding a second store;
- count what a second store costs.

---

## The problem

Two teams, two migrations, in opposite directions.

**Infisical**, which makes a secrets-management product, started on MongoDB, a document database, and later moved everything to PostgreSQL. Their account of why: "we lost out on many nice features from the relational world like CASCADE which, when specified, deletes all referenced resources across other tables whenever a target resource is deleted; this hurt in particular because our data was very much relational. As a result, we employed hefty delete functions in our old codebase that never fully did the job and left dangling resources in the MongoDB databases" ([Tony Dang, *The Great Migration from MongoDB to PostgreSQL*](https://infisical.com/blog/postgresql-migration-technical), 2024).

**Discord** also started on MongoDB, for chat messages, and moved the other way — to Cassandra, a store with far fewer features than a relational database. Their account: "Around November 2015, we reached 100 million stored messages and at this time we started to see the expected issues appearing: the data and the index could no longer fit in RAM and latencies started to become unpredictable. It was time to migrate to a database more suited to the task" ([Stanislav Vishnevskiy, *How Discord Stores Billions of Messages*](https://discord.com/blog/how-discord-stores-billions-of-messages), 2017).

Neither team chose a bad database. Each had data whose *shape of use* did not match what the store was built for: one needed relationships and integrity between records; the other needed to write and read an ever-growing stream of small records by one key. The cost of the mismatch was a migration of the kind Chapter 2.7 described, on the system's most important data.

The question *which database is best?* has no answer. *Which database is built for the way this data will be read and written?* usually does.

---

## The concept

### 1. A datastore is a bet on access patterns

Chapter 2.1 showed that a storage engine is organised for certain operations: a B-tree keeps keys sorted so that lookups and ranges are cheap; an index is a second copy of some columns, arranged to answer one kind of question. Arranging data for one kind of question makes others expensive. That is true of an index, and it is true of a whole database.

For a long time one design was sold for everything. Michael Stonebraker and Uğur Çetintemel opened a 2005 paper with it: "The last 25 years of commercial DBMS development can be summed up in a single phrase: 'One size fits all'" — and argued that the "commercial world will fracture into a collection of independent database engines" specialised for different work ([*"One Size Fits All": An Idea Whose Time Has Come and Gone*](https://cs.brown.edu/~ugur/fits_all.pdf), 2005). It did. The families in the next section are the result.

So the choice starts from the workload, not from the product list. Five questions describe a workload well enough to choose:

| Question | Typical answers |
|---|---|
| **How is it read?** | By one key · by a range of keys · by arbitrary conditions and joins · by words in text · by aggregating over everything |
| **How is it written?** | Rarely or constantly · inserted once or updated in place · in transactions with other data or alone |
| **What must always be true?** | Constraints between records (Chapter 2.5) · all-or-nothing changes (Chapter 2.3) · or nothing beyond *the write is not lost* |
| **How big, and how fast does it grow?** | Fits on one machine for years · or outgrows any one machine |
| **How fresh, and how fast, must an answer be?** | Milliseconds, and current, for a user waiting · seconds, and slightly stale, for a report |

### 2. The families

Each row is a different bet. The middle column is what the store makes cheap; the next is the price.

| Family | It stores | Built for | Gives up | Examples |
|---|---|---|---|---|
| **Relational** | Tables of rows, related by keys | Questions nobody planned for: any filter, any join. Transactions and constraints | Easy scaling of writes beyond one machine (Phase 5); fast analytics over very large tables | PostgreSQL, MySQL |
| **Key-value** | A value under a key | Get and put by key, at very high rates with predictable latency | Every question that is not *what is under this key?* | Redis, DynamoDB |
| **Document** | Self-contained documents, usually JSON | Reading and writing one whole thing — an order with its lines — in one operation; structure that varies | Relationships *between* documents: joins and many-to-many | MongoDB |
| **Wide-column** | Rows grouped into partitions by one key, sorted inside each | Huge write volumes; *the latest N in this partition*; growing by adding machines | Joins, constraints, and any query the tables were not designed for | Cassandra, ScyllaDB, HBase |
| **Search engine** | An inverted index: word → the documents containing it | Finding documents by words, ranked by relevance | Being the source of truth: it is a copy, and slightly behind | Elasticsearch, OpenSearch |
| **Columnar (analytical)** | Tables, stored column by column | Aggregates over billions of rows that touch a few columns | Fetching or updating single rows quickly | ClickHouse, BigQuery, Redshift |
| **Object storage** | Whole files of any size, under a key | Storing large objects cheaply and very durably | Queries; changing part of an object | Amazon S3, Google Cloud Storage |

A few lines on each, from the people who build them.

**Key-value.** Amazon's Dynamo paper is the classic statement of the case. Many services, it observed, need only primary-key access, and for them "the common pattern of using a relational database would lead to inefficiencies and limit scale and availability. Dynamo provides a simple primary-key only interface to meet the requirements of these applications" ([DeCandia et al., *Dynamo*](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf), 2007). A store that only has to find a value by its key can be spread over many machines very simply (Chapter 5.3). Redis, another common member of the family, keeps its data in memory: "an in-memory data store used by millions of developers as a cache" among other things ([Redis docs](https://redis.io/docs/latest/get-started/)); Chapters 3.3 and 3.4 use it for that.

**Document.** The rule is in MongoDB's manual: "data that's accessed together should be stored together" ([MongoDB docs, *Data Modeling*](https://www.mongodb.com/docs/manual/data-modeling/)). An order and its lines, always read together, become one document and one read. The price appears when data is *not* a self-contained tree. Transactions across documents exist, but the same manual warns that "a distributed transaction incurs a greater performance cost over single document writes, and the availability of distributed transactions should not be a replacement for effective schema design" ([MongoDB docs, *Transactions*](https://www.mongodb.com/docs/manual/core/transactions/)). Relationships and transactions are the two things Infisical's post says they missed. (PostgreSQL's `jsonb`, from Chapter 2.5, gives you documents *inside* a relational database, which is often enough.)

**Wide-column.** Here the design process itself is reversed. Cassandra's documentation: "in Cassandra you don't start with the data model; you start with the query model", and "You cannot perform joins in Cassandra. If you have designed a data model and find that you need something like a join, you'll have to either do the work on the client side, or create a denormalized second table that represents the join results for you" ([Apache Cassandra docs, *RDBMS design*](https://cassandra.apache.org/doc/latest/cassandra/developing/data-modeling/data-modeling_rdbms.html)). Every query gets a table shaped for it. In return, writes are cheap (these stores are built on the LSM-trees of Chapter 2.1) and the cluster grows by adding machines.

**Search.** A search engine's central structure is the inverted index, which the second example below builds. Its copy of the data trails the original: "By default, Elasticsearch periodically refreshes indices every second", which "is why we say that Elasticsearch has near real-time search: document changes are not visible to search immediately" ([Elastic docs, *Near real-time search*](https://www.elastic.co/docs/manage-data/data-store/near-real-time-search)). Treat it as a copy fed *from* a primary database, not as the primary.

**Columnar.** Section 3 and the first example explain this one. In one sentence from Amazon Redshift's documentation: "Columnar storage for database tables is an important factor in optimizing analytic query performance, because it drastically reduces the overall disk I/O requirements" ([Redshift docs](https://docs.aws.amazon.com/redshift/latest/dg/c_columnar_storage_disk_mem_mgmnt.html)).

**Object storage.** "Amazon S3 is an object storage service that stores data as objects… An object is a file and any metadata that describes the file. A bucket is a container for objects" ([Amazon S3 docs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html)). It is where images, videos, exports and backups belong. The usual pattern is two stores working together: the file goes to object storage; its key, owner and size go in a row in the database.

Other families exist for narrower jobs — time-series databases for metrics (Chapter 7.1), graph databases for following long chains of relationships. The same five questions apply to them.

### 3. Rows and columns: why analytics wants a different engine

One distinction deserves a closer look, because it explains the largest split in the table: the same *relational* data, stored in two opposite ways.

```text
Row by row  (PostgreSQL, MySQL)            Column by column  (ClickHouse, BigQuery, Redshift)

 id  customer  amount  region               id:        1     2     3    ...
[ 1      7       500     EU  ]              customer:  7     9     7    ...
[ 2      9       120     US  ]              amount:    500   120   910  ...
[ 3      7       910     EU  ]              region:    EU    US    EU   ...
```

- A **transactional** workload (the usual name is OLTP, online transaction processing) reads and writes whole records, a few at a time: *show order 2*, *insert an order*. Row by row is ideal: everything about order 2 is in one place.
- An **analytical** workload (OLAP, online analytical processing) reads a few fields of very many records: *total amount per region, this year*. Row by row, that drags every field of every order through memory to use two of them. Column by column, it reads two arrays and nothing else. And because a column holds values of one kind, often repeated, it compresses far better than rows do.

The same layout that makes the analytical query fast makes "show order 2" slow: its fields are now scattered across as many places as there are columns. The first example measures both effects. Neither layout is better; they are built for opposite questions, which is why companies that need both run two systems and copy data from one to the other.

### 4. Start with one relational database

With so many specialised stores, which do you start with? For almost every new product: one relational database. The reasons are practical.

**You do not know your access patterns yet.** The specialised stores must be designed around the queries. Amazon's own DynamoDB guidance says so plainly: "you shouldn't start designing your schema for DynamoDB until you know the questions it will need to answer" — whereas "For an RDBMS, you can go ahead and create a normalized data model without thinking about access patterns. You can then extend it later when new questions and query requirements arise" ([DynamoDB docs, *NoSQL design*](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/bp-general-nosql-design.html)). A young product's questions change monthly. The relational model is the one built for that.

**It imitates the others well, at modest scale.** A table with a primary key is a key-value store. A `jsonb` column is a document store (Chapter 2.5). `SELECT … FOR UPDATE SKIP LOCKED` is a job queue (Chapter 2.4). A full-text index is a search engine — the second example gets from a second to a fifth of a millisecond with one. None of these matches the specialist at the specialist's scale. Most products are not at that scale, and do not know yet which one they will need.

**One store is one of everything.** One backup to test (Chapter 2.9), one failover to rehearse, one place where a transaction can cover all the data. Every later chapter on keeping two systems consistent exists because of a second store.

**One machine goes a long way.** The lookups in Chapter 2.2 took hundredths of a millisecond on five million rows. Estimate your real numbers the way Chapter 1.1 did before concluding that they need something exotic.

### 5. When to add a second store, and what it costs

Add one when you can point at the evidence:

| Evidence | Then consider |
|---|---|
| You are storing files | Object storage — from the first day. Large objects in database rows make everything about the database slower to back up and restore. |
| The same expensive reads repeat, and the database is busy serving them | A cache (Chapter 3.3) |
| Reports scan large tables and compete with user traffic (Chapter 2.6) | First a read replica (Chapter 3.6); then a columnar store |
| Search needs what the built-in cannot do well — tolerance for typing mistakes, tuned ranking, very large indexes | A search engine, fed from the database |
| One kind of record outgrows a single machine, and is always read by one key | Partitioning (Chapter 5.3), or a key-value or wide-column store |

What it costs is easy to underestimate. Martin Fowler, writing in favour of using several stores, was direct about the price: "This will come at a cost in complexity. Each data storage mechanism introduces a new interface to be learned" ([*PolyglotPersistence*](https://martinfowler.com/bliki/PolyglotPersistence.html)). In practice:

- **Two copies that must agree.** The moment an order is in the database *and* in a search index, there is a window in which they differ, and a failure mode in which they differ for ever. Writing to two systems cannot be made atomic; Chapter 4.5 is about what to do instead.
- **Another system to run.** Its own backups, upgrades, monitoring, access control, capacity planning, and failure behaviour for the person on call to know at 3 a.m.
- **No transactions and no joins across the boundary.** The application takes over both.

One rule keeps this manageable: for every piece of data, exactly one store is the **system of record** — the place where it is written first and is authoritative. Every other copy is **derived** from it, and can be thrown away and rebuilt. (The terms are from Martin Kleppmann's [*Designing Data-Intensive Applications*](https://dataintensive.net/), whose third part is called *Derived Data*.) A search index or a cache that could not be rebuilt from the system of record has quietly become a second source of truth, and the two will disagree.

---

## A real example

### Run it: rows and columns

[`code/ch08-layouts/main.go`](code/ch08-layouts/main.go) needs no database. It holds five million orders in memory twice — as an array of records, and as one array per field — and asks both the same two questions. Nothing differs but the arrangement.

```bash
go run ./phase-2-data/code/ch08-layouts
```

```text
5000000 orders, 8 fields each. Row by row: 320 MB. Column by column: 8 arrays of 40 MB.

1. Show me order N (all eight fields; average over 2,000,000 orders picked at random)
     row by row            23 ns per order
     column by column     106 ns per order   (4.5 times slower)

2. Total amount in each region (two fields of every order)
     row by row                       38.0 ms   reads 320 MB
     column by column                  9.4 ms   reads 80 MB   (4.1 times faster)
     columns in the smallest types     4.6 ms   reads 25 MB   (8.3 times faster)

3. The 'day' column: orders arrive in date order, so the same day repeats many times in a row
     as it is            5000000 values, 40 MB
     run-length encoded  3650 runs, 43 KB   (value, how many times)
```

(Timings are from one machine; the ratios are what matter.)

1. **One whole order.** Row by row, its eight fields sit next to each other, and one trip to memory fetches them all. Column by column, they are in eight places. Four and a half times slower — in memory. On disk, where each of those places may be a separate read, the gap can be wider.
2. **A total over everything.** Now the row layout is the slow one: to add up `amount` by `region` it must pull all 320 MB through the processor to use a quarter of it. The column layout reads only the two arrays it needs, and is four times faster. Stored in the smallest types that fit — a region number needs one byte, not eight — it reads 25 MB and is eight times faster. The speed-up roughly follows the bytes read. That is the whole idea: *an analytical query is limited by how much data it has to look at.*
3. **Compression.** The `day` column, 40 MB of mostly repeated values, becomes 3,650 pairs of *(value, how many times)*: 43 KB. Rows cannot be compressed like this, because neighbouring values in a row are of different kinds. Real columnar stores add a good deal of engineering on top, but layout and compression are the foundation.

This is why a report that takes seconds on your transactional database can take milliseconds in an analytical one, and why you would never run checkout on the analytical one.

### Run it: an inverted index

[`code/ch08-search/search.sql`](code/ch08-search/search.sql) builds half a million product descriptions in PostgreSQL and looks for the hundred that mention a rare word.

```bash
psql -f phase-2-data/code/ch08-search/search.sql
```

By scanning — which is what `LIKE` with a leading `%` must do (Chapter 2.2):

```text
 Aggregate (actual rows=1 loops=1)
   ->  Seq Scan on s_products (actual rows=100 loops=1)
         Filter: (description ~~* '%waterproof%'::text)
         Rows Removed by Filter: 499900
 Planning Time: 0.286 ms
 Execution Time: 988.747 ms
```

A second, to read 114 MB of text looking for ten characters. Full-text search starts by turning each description into a sorted list of word stems with their positions:

```text
==== 3. What full-text search stores for one description ====
                 tsvector                  
-------------------------------------------
 'boot':3 'hike':2 'line':4 'waterproof':1
```

An **inverted index** turns those lists inside out: for every word, the list of documents that contain it. In PostgreSQL it is a GIN index — "GIN stands for Generalized Inverted Index", designed for when "the items could be documents, and the queries could be searches for documents containing specific words" ([PostgreSQL docs](https://www.postgresql.org/docs/current/gin.html)). It took 14 seconds to build and occupies 12 MB. Then:

```text
 Aggregate (actual rows=1 loops=1)
   ->  Bitmap Heap Scan on s_products (actual rows=100 loops=1)
         Recheck Cond: (to_tsvector('english'::regconfig, description) @@ '''waterproof'''::tsquery)
         Heap Blocks: exact=100
         ->  Bitmap Index Scan on s_products_search (actual rows=100 loops=1)
               Index Cond: (to_tsvector('english'::regconfig, description) @@ '''waterproof'''::tsquery)
 Planning Time: 0.558 ms
 Execution Time: 0.197 ms
```

Five thousand times faster: one lookup of "waterproof" in the index, then the hundred rows it lists. And it is a different *kind* of matching:

```text
(a user searches for "hike"; the descriptions say "hiking")
 found_by_like | found_by_search 
---------------+-----------------
             0 |             100
```

`LIKE` compares characters; search compares words, reduced to their stems. The manual lists what plain pattern matching lacks: "There is no linguistic support, even for English", "They provide no ordering (ranking) of search results", and "They tend to be slow because there is no index support, so they must process all documents for every search" ([PostgreSQL docs, *Full Text Search*](https://www.postgresql.org/docs/current/textsearch-intro.html)).

Two lessons. A search engine is, at its core, this data structure — so search is a *different access pattern*, not a slow query to be tuned. And your relational database already has one, which is where a product's first search feature should usually live.

### How Discord chose

Back to the messages from the top of the chapter. Discord's post is a model of choosing by access pattern.

They began by measuring: "Before choosing a new database, we had to understand our read/write patterns and why we were having problems with our current solution. It quickly became clear that our reads were extremely random and our read/write ratio was about 50/50" ([Discord, 2017](https://discord.com/blog/how-discord-stores-billions-of-messages)).

Then they shaped the table around the one question that matters for chat — *the recent messages in this channel*. In [the post's](https://discord.com/blog/how-discord-stores-billions-of-messages) words, "channel_id became the partition key since all queries operate on a channel", and within a channel, messages are kept sorted by an ID that increases with time. Every read touches one partition and reads a contiguous run of it. That is the wide-column bet from section 2, taken deliberately.

And they paid the price that the table in section 2 lists. With no read-before-write and no transactions, concurrent edits and deletes produced corrupt rows that the application had to detect and clean up. Deletes turned out to be writes of markers that must later be skipped: in one channel where millions of messages had been deleted, "Cassandra had to effectively scan millions of message tombstones" to show a single message ([same post](https://discord.com/blog/how-discord-stores-billions-of-messages)).

In 2022 they changed engines again, to ScyllaDB — "we're going from running 177 Cassandra nodes to just 72 ScyllaDB nodes" — and the later post still describes the same shape: "We partition our messages by the channel they're sent in, along with a bucket, which is a static time window" ([Discord, *How Discord Stores Trillions of Messages*](https://discord.com/blog/how-discord-stores-trillions-of-messages), 2023). The product underneath changed. The design that came from the access pattern did not.

---

## Trade-offs

**Specialised means fast inside the pattern and helpless outside it.** A wide-column table answers its one query at any scale and cannot answer a second one without a second table. Choose a specialised store when the access pattern is known, stable and large. That is rarely true of a new product and often true of one mature feature.

**"Flexible schema" moves the schema; it does not remove it.** A store that accepts any document still has code that expects certain fields. The rules that a relational database would enforce (Chapter 2.5) are now enforced by every application that writes — or by none.

**A general store is never the fastest at anything.** PostgreSQL's full-text search is not Elasticsearch; its `jsonb` is not MongoDB; its tables are not a warehouse. The argument for it is not that it wins each comparison, but that one adequate system is cheaper than five excellent ones until scale proves otherwise.

**Every additional store buys performance with consistency and operations.** The reads get faster; the data gets a second home that can be stale or wrong; the team gets another system to keep alive.

**Changing your mind is expensive, so decide late.** Moving the system of record is about the hardest migration there is — both Infisical's and Discord's posts spend much of their length on how the move was made safely. The cheapest protection is to keep data access behind one layer of your own code, so that the store underneath can change without the rest of the application knowing.

---

## Practice

### Check yourself

Answer before reading on.

1. A product has **sessions** — looked up by session ID on every request, expiring after 30 minutes, and losing one costs the user a login — and **payments**, which must never be lost and are reported on together with orders and refunds. Which family suits each, and why?
2. `SELECT region, sum(amount) FROM orders GROUP BY region` over a billion rows is slow in a row store and fast in a columnar one. `SELECT * FROM orders WHERE id = 42` is the opposite. Explain both with one idea.
3. A colleague proposes Elasticsearch as the only database for a product catalogue, *because we need search anyway*. What do you ask?

<details>
<summary>Answers</summary>

1. **Sessions: key-value.** The only access is by key; the volume is high; expiry is built in; and the data is disposable — the worst case is a login. An in-memory store such as Redis fits, and so, at first, does a table in the main database. **Payments: relational.** They need durability, constraints (a refund must refer to a payment), transactions with orders, and reports that join across tables — questions that will keep changing. Putting payments in a key-value store gives up exactly what they need; putting sessions in the relational database is merely more than they need.

2. The cost of a query is roughly the amount of data it must read. The aggregate needs two fields of every row: a column layout stores those two fields contiguously (and compressed), so it reads a small fraction of the table, while a row layout must read every field of every row. The single-row fetch needs every field of one row: a row layout has them side by side, found through an index, while a column layout must visit one place per column. Each layout puts together what its workload reads together.

3. *What is the system of record?* A search index trails the data it indexes and is normally derived from a primary store. Then: how are prices and stock updated, and can a customer be shown a stale one? Is anything transactional — reserving stock, changing a price and a promotion together? What happens when the index must be rebuilt after a mapping change — from what? If the catalogue is ever written, the usual answer is a relational database as the system of record and the search engine as a derived copy; or, to begin with, the database's own full-text search.

</details>

### Exercise: choose the stores for a marketplace

You are designing the data layer for an online marketplace where independent sellers list handmade goods. Expected a year after launch:

| Data | Volume | How it is used |
|---|---|---|
| Users, shops, listings, orders, payments | 5 million listings; 20,000 orders a day | Every page; checkout; refunds; support tools that ask new questions weekly |
| Listing photos | 5 per listing, about 2 MB each | Shown on every listing page |
| Search over listings | 200 searches a second at peak | Words, filtered by category and price, best matches first |
| Shopping carts | 1 million active | Read and written by the owner only; abandoned after days |
| Page-view events | 50 million a day | Dashboards for the business; "trending" lists |
| Messages between buyers and sellers | 2 million a day | *The latest messages in this conversation* |

1. Do the arithmetic first: storage per year for photos, events and messages.
2. For each kind of data, name the access pattern and choose a store **for launch day**, with a team of three engineers.
3. Say what evidence would make you change each choice, and to what.

<details>
<summary>Worked solution</summary>

**1. Arithmetic.**

- Photos: 5,000,000 listings × 5 × 2 MB = **50 TB**.
- Events: 50,000,000 a day ÷ 86,400 ≈ **580 a second** on average. At about 200 bytes each, 10 GB a day: **3.6 TB a year**, and 18 billion rows.
- Messages: 2,000,000 a day × 365 = **730 million rows a year**; at about 300 bytes, some 220 GB.
- Orders: 20,000 a day is 7.3 million a year — small.

The numbers already sort the data. Photos and events are far too large for a transactional database. Everything else is modest.

**2. Launch day: two stores.**

| Data | Access pattern | Launch-day store | Why |
|---|---|---|---|
| Users, shops, listings, orders, payments | Varied and changing; joins; transactions; constraints | **PostgreSQL** — the system of record | The relational case exactly, and small. |
| Photos | Whole objects by key; large; written once | **Object storage**, with the key in a `listing_photos` row | 50 TB of files does not belong in database rows. A CDN goes in front (Chapter 3.5). |
| Search | Words, filters, ranking | **PostgreSQL full-text search**, with an index like the one in the example | 5 million listings is ten times the example's table: a reasonable place to start, to be checked with real queries. One store, no synchronisation. |
| Carts | By owner; disposable | **A PostgreSQL table** | A key-value pattern, at a volume one table handles easily. |
| Page views | Append-only; huge; read only in aggregate | **Files in object storage**, written in batches by the application — not rows in PostgreSQL | 18 billion rows a year would swamp the transactional database for the benefit of a dashboard. Stored as files, they can be loaded into any analytical tool later. |
| Messages | By conversation, newest first | **A PostgreSQL table** with an index on `(conversation_id, id)` | 730 million rows a year is large but workable with the right index (Chapter 2.2), and every read uses it. |

Two systems to run: one database and one object store. Three engineers can operate that and still build the product.

**3. What would change each choice.**

| Data | Evidence | Then |
|---|---|---|
| Core data | Reads dominate and the primary is busy | Read replicas and a cache (Phase 3) — still relational |
| Search | Sellers and buyers complain about relevance or misspellings; or search load visibly competes with checkout | A search engine, fed from PostgreSQL by a change stream (Chapter 4.5) |
| Carts | Cart reads and writes are a noticeable share of database load | A key-value store with expiry |
| Page views | The business wants interactive dashboards over months of data | A columnar store loaded from the files; "trending" computed there and written back as a small table |
| Messages | The table passes a few billion rows, or its writes compete with orders | Partition it by conversation (Chapter 5.3), or move it to a wide-column store — the Discord design: partition by conversation, sort by message ID |

Notice what the evidence is in each case: a measurement, or a user complaint — never "we might need it". And notice that every later store is *derived from* or *partitioned out of* the first, which stays the system of record for everything money touches.

</details>

---

## Production tips and common mistakes

**Tips**

- **Write down the access patterns first** — the five questions in section 1 — and choose from them. If you cannot answer them yet, choose the store that does not need the answers.
- **Default to one relational database**, and make it earn its replacement with measurements.
- **Put files in object storage from the start**, and their keys in the database.
- **Name the system of record for every kind of data.** Everything else must be rebuildable from it — and rebuild it once, to prove it.
- **Keep analytics off the transactional database**: a replica first, a columnar store when that is not enough.
- **Try the built-in feature before the dedicated product**: full-text search, `jsonb`, `SKIP LOCKED` queues.
- **Hide the store behind your own data-access layer**, so that changing it later is one layer's problem.
- **Count the whole cost of a new store**: backups, upgrades, monitoring, on-call knowledge, and the consistency work between it and the system of record.

**Common mistakes**

- **Choosing by popularity, or by what the team wants to learn.**
- **Choosing for a scale the product does not have**, and paying for the constraints of a distributed store on a few gigabytes of data.
- **Choosing a document store "for flexibility"** for data that is full of relationships.
- **Using a search engine or a cache as the source of truth.**
- **Running reports on the primary database** until they take it down (Chapter 2.6).
- **Storing images and files in database rows.**
- **A table-per-query store with queries that keep changing.**
- **Adding a store without deciding how it is kept consistent** with the one that already holds the data.
- **Comparing products by benchmark** instead of by your own access patterns.

---

## Key takeaways

- A datastore is organised to make certain reads and writes cheap, and is poor at the rest. Choose by how the data will be read and written, not by reputation.
- Relational stores answer unplanned questions and keep data correct; key-value, document and wide-column stores trade that for speed and scale on a pattern known in advance.
- Row layout suits transactions; column layout suits analytics. Each stores together what its workload reads together, and the difference is measured in multiples.
- A search engine is an inverted index — a different access pattern, and one your relational database can provide at modest scale.
- Start with one relational database, plus object storage for files. Add a store when measurements demand it.
- Every additional store is a copy that can disagree, and a system to operate. Keep one system of record, and make everything else derivable from it.

**Next:** 2.9 Durability, backups, and restore *(planned)* — what "saved" really means, and how to get the data back when the worst happens.

---

## Sources

- Tony Dang, [*The Great Migration from MongoDB to PostgreSQL*](https://infisical.com/blog/postgresql-migration-technical), Infisical blog, 2024.
- Stanislav Vishnevskiy, [*How Discord Stores Billions of Messages*](https://discord.com/blog/how-discord-stores-billions-of-messages), Discord blog, 2017; and [*How Discord Stores Trillions of Messages*](https://discord.com/blog/how-discord-stores-trillions-of-messages), Discord blog, 2023.
- Michael Stonebraker and Uğur Çetintemel, [*"One Size Fits All": An Idea Whose Time Has Come and Gone*](https://cs.brown.edu/~ugur/fits_all.pdf), ICDE 2005.
- Giuseppe DeCandia et al., [*Dynamo: Amazon's Highly Available Key-value Store*](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf), SOSP 2007.
- Martin Fowler, [*PolyglotPersistence*](https://martinfowler.com/bliki/PolyglotPersistence.html), 2011.
- Martin Kleppmann, [*Designing Data-Intensive Applications*](https://dataintensive.net/), first edition, O'Reilly, 2017 — chapters 2 and 3 (data models; storage, including column-oriented storage) and Part III (*Derived Data*). Cited for its terms and as further reading; it is not quoted here.
- Vendor documentation: [MongoDB, *Data Modeling*](https://www.mongodb.com/docs/manual/data-modeling/) and [*Transactions*](https://www.mongodb.com/docs/manual/core/transactions/); [Apache Cassandra, *RDBMS design*](https://cassandra.apache.org/doc/latest/cassandra/developing/data-modeling/data-modeling_rdbms.html); [Amazon DynamoDB, *NoSQL design*](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/bp-general-nosql-design.html); [Amazon Redshift, *Columnar storage*](https://docs.aws.amazon.com/redshift/latest/dg/c_columnar_storage_disk_mem_mgmnt.html); [Amazon S3, *What is Amazon S3?*](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html); [Elastic, *Near real-time search*](https://www.elastic.co/docs/manage-data/data-store/near-real-time-search); [Redis, *Get started*](https://redis.io/docs/latest/get-started/).
- PostgreSQL documentation: [GIN Indexes](https://www.postgresql.org/docs/current/gin.html); [Full Text Search — Introduction](https://www.postgresql.org/docs/current/textsearch-intro.html).
