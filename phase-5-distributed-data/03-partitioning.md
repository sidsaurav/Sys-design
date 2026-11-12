# 5.3 Partitioning (sharding)

[Syllabus](../SYLLABUS.md) · Phase 5 · Previous: [5.2 Multi-leader and leaderless replication](02-multi-leader-and-leaderless.md) · Next: [5.4 Consistency models and CAP](04-consistency-models-and-cap.md)

> **Time:** about 35 minutes to read, 40 minutes of practice.
> **You need:** indexes from [Chapter 2.2](../phase-2-data/02-indexes-and-query-plans.md); access patterns from [Chapter 2.8](../phase-2-data/08-choosing-a-datastore.md); partitions and keys in a log from [Chapter 4.4](../phase-4-async/04-logs-and-streams.md). The example needs only Go.

**By the end of this chapter you can**

- say when partitioning is the answer, and what it takes away;
- compare partitioning by range and by hash, and choose a partition key from the queries;
- explain why `hash mod N` makes adding a machine expensive, and two schemes that do not;
- recognise a hot partition, and say why more shards do not cure it;
- say what a query that crosses shards costs, and how secondary indexes work on partitioned data.

---

## The problem

*An illustrative scenario; the example reproduces its numbers.* The orders table has outgrown its server. Replicas spread the reads (Chapter 3.6), but every write still goes through one primary, whose disk is nearly full and whose CPU is not far behind. The team splits the table across eight servers — eight **shards** — by taking the order's ID modulo eight. Writes spread evenly. For a month, everything is better.

Then three things happen.

- *Show me my orders* has become slow and erratic. A customer's orders are on all eight shards, so the page asks all eight, and is as slow as the slowest.
- A ninth server is added. Changing `mod 8` to `mod 9` moves nearly every order to a different shard. The migration takes two weeks.
- One customer — a marketplace reseller — places a fifth of all orders. Whichever shard holds their data is the busiest by far, however many shards there are.

Partitioning is the only thing in this course that scales *writes*. It is also the change that is hardest to undo, because the scheme decides — permanently, and for every query — which questions are cheap.

---

## The concept

### 1. What partitioning is, and when

**Partitioning** divides one logical table into pieces, each on its own machine, so that each row lives in exactly one piece. (*Shard* and *partition* mean the same here.) Each piece is usually replicated as well (Chapter 5.1): partitioning spreads the *data*, replication keeps *copies* of it, and they are separate decisions.

It is the right step when one machine cannot hold the data or take the writes — after a bigger machine, replicas, caches and archiving old rows have been considered. It is a large step. What a single database gave for nothing now has to be worked for:

- **Queries that span partitions** must ask several machines and combine the answers.
- **Transactions that span partitions** are no longer one transaction (Chapter 5.7).
- **Constraints that span partitions** — a unique email across all users — are no longer enforced by an index.
- **Operations** multiply: eight databases to back up, migrate and fail over.

### 2. The partition key

Each row is placed by the value of one column, its **partition key**. Everything follows from that choice.

Notion's engineers state the two requirements together: "Good partition keys ensure that tuples are uniformly distributed across shards. The choice of partition key also depends on application structure, since distributed joins are expensive and transactionality guarantees are typically limited to a single host" ([Notion, *Herding elephants: Lessons learned from sharding Postgres at Notion*](https://www.notion.com/blog/sharding-postgres-at-notion)).

So choose the key from the queries (Chapter 2.8). Ask what a typical request touches, and make that the unit:

- If nearly every request is about one **customer** (or tenant, or workspace), partition by customer. A request then goes to one shard, and a transaction about one customer is an ordinary transaction. Notion chose the workspace: "Since users typically query data within a single workspace at a time, we avoid most cross-shard joins" ([same post](https://www.notion.com/blog/sharding-postgres-at-notion)).
- If requests are about one **conversation**, one **device**, one **document**: that.

The key must also be one whose values are many and roughly even in size. A key with a few huge values is the reseller of the opening scenario.

### 3. By range, or by hash

**By range.** Each shard holds a contiguous range of the key: A to F, G to M. Keys that are close together are stored together, so a query for a range — *names beginning with An*, *this week's readings* — goes to one shard. The danger is that load is as uneven as the keys are. If the key increases with time, every new row goes to the last shard.

**By hash.** Each shard holds the keys whose *hash* falls in its share. Hashing scatters neighbouring keys, so load is even — and a range of the key is now everywhere, so a range query must ask every shard.

A common combination uses both: hash the first part of the key to choose the shard, and keep rows sorted by the second part within it. Discord's messages are an example: "channel_id became the partition key since all queries operate on a channel", with messages sorted by ID inside each channel ([Discord, *How Discord Stores Billions of Messages*](https://discord.com/blog/how-discord-stores-billions-of-messages)). One channel's recent messages are then one sorted read on one machine.

### 4. Adding a machine

The simplest placement is `hash(key) mod N`. It works until N changes. With a ninth shard, almost every key's remainder is different, so almost every row has to move — and while it moves, every reader must know where to look.

Two established schemes move only what must.

**Consistent hashing.** Imagine the hash values arranged in a circle. Each shard is placed at points on the circle, and owns the keys between its point and the previous one. Adding a shard takes a slice from its neighbours and leaves everything else alone: "The principle advantage of consistent hashing is that departure or arrival of a node only affects its immediate neighbors and other nodes remain unaffected" ([DeCandia et al., *Dynamo*](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)). With one point per shard the slices are very uneven, so each shard is given many: "instead of mapping a node to a single point in the circle, each node gets assigned to multiple points in the ring".

**A fixed number of logical partitions.** Divide the keys, once, into many more partitions than there are machines — a few hundred or a few thousand — by `hash(key) mod P`, where P never changes. A table says which machine each partition is on. Adding a machine moves some whole partitions to it and updates the table. Notion did this: "we settled on an architecture consisting of 480 logical shards evenly distributed across 32 physical databases" ([Notion](https://www.notion.com/blog/sharding-postgres-at-notion)). It is the same idea as the partitions of a Kafka topic (Chapter 4.4), and its limit is the same: P is chosen once.

Either way, moving data while serving traffic is Chapter 2.7's expand–migrate–contract: copy, keep both in step, switch reads, remove the old.

### 5. Hot partitions

A partition is **hot** when it has far more than its share of the load. Two causes look alike and are not.

- **A skewed scheme**: too few key values, or a range scheme with a key that increases. More shards, or a better key, fixes it.
- **A hot key**: one value of the key that is itself a large part of the traffic. Every row with that key is on one shard *by design*. More shards do not help: the key still lands on exactly one of them.

For a hot key the remedies are specific to it. Split it — add a second component to the key, so that one customer's data is spread over several partitions, at the price that reading all of it now asks several. Give it a shard of its own. Or take load off it with a cache (Chapter 3.3).

Size is the same problem in another form. A partition that grows without limit eventually cannot be moved or rebuilt. Discord met this with its largest channels: "It became clear we had to somehow bound the size of partitions because a single Discord channel can exist for years and perpetually grow in size. We decided to bucket our messages by time" ([Discord](https://discord.com/blog/how-discord-stores-billions-of-messages)). The key became the channel *and* a period of about ten days.

### 6. Queries that cross partitions

A query that includes the partition key goes to one shard. Any other query is a **scatter-gather**: sent to every shard, with the answers merged.

Scatter-gather is expensive in two ways. It uses every shard's capacity for one query. And it is as slow as the *slowest* shard to answer: if each shard is slow one time in a hundred, a query that needs eight of them is slow about eight times in a hundred, and one that needs sixty-four is slow nearly half the time.

Looking rows up by something other than the partition key is a **secondary index**, and there are two ways to keep one.

| | A local index, on each shard | A global index, partitioned by the indexed column |
|---|---|---|
| A write | Updates one shard: the row and its index entry together | Updates two places — the row's shard and the index's — which is a dual write (Chapter 4.5), usually done asynchronously |
| A read by that column | Scatter-gather: every shard's index is asked | One lookup in the index, then one shard |
| Freshness | Always exact | May be slightly behind |

So *find the order by its ID alone*, on a table partitioned by customer, has three answers: ask every shard; keep a second table that maps order to customer; or put the customer's shard into the order's ID, so that the ID itself says where to look. The third is often the cheapest and has to be decided before the first ID is issued.

---

## A real example

### Run it: a million keys, placed

[`code/ch03-partitions/main.go`](code/ch03-partitions/main.go) — about 220 lines, no dependencies — is not a database. Placing data is hashing and counting, and the program does that for a million keys and reports where they land.

```bash
go run ./phase-5-distributed-data/code/ch03-partitions
```

```text
PART 1 — a table of orders on 8 shards. Order IDs increase with time; each order belongs to one of
         10,000 customers. Three ways to decide which shard an order is on.

                                      the fullest shard holds     of 100,000 new orders,      one customer's orders
                                        (of 1,000,000 orders)    the busiest shard takes    are on this many shards
  by ranges of the order ID                             12.5%                     100.0%                     8 of 8
  by a hash of the order ID                             12.6%                      12.8%                     8 of 8
  by a hash of the customer ID                          13.3%                      13.3%                     1 of 8
```

- **By ranges of the order ID**, storage is perfectly even, and *every new order goes to one shard*. Seven shards are archives. The eighth takes all the writes that the split was meant to spread.
- **By a hash of the order ID**, writes are even — and one customer's orders are on all eight shards. This is the opening scenario's slow page.
- **By a hash of the customer**, a customer's orders are on one shard. Storage is slightly less even, because customers are not all the same size.

```text
PART 2 — 1,000,000 keys on 8 shards. A ninth is added. Ideally one key in nine moves — to the new
         shard — and every shard then holds 11.1%.

  a key's shard is found by...                           keys that move     the fullest shard afterwards
  hash modulo the number of shards                                  89%                            11.2%
  a ring, each shard at one point                                    5%                            20.0%
  a ring, each shard at 200 points                                  12%                            12.2%
  1,024 fixed partitions, assigned to shards by a table              11%                            11.2%
```

`mod 9` in place of `mod 8` moves 89% of the data: the two-week migration. A ring with one point per shard moves very little — and leaves one shard with 20% where 11% was fair, because nine random points do not divide a circle evenly. With 200 points each, 12% moves and the shards are nearly level. Fixed partitions move exactly the ninth that had to move.

```text
PART 3 — 1,000,000 requests, partitioned by a hash of the customer. One customer makes a fifth of them.

                                                          requests on the busiest        an even share
                                                                            shard             would be
  8 shards                                                                  29.9%                12.5%
  16 shards                                                                 25.0%                 6.2%
  64 shards                                                                 21.2%                 1.6%
  8 shards, the busy customer's key split in 8                              17.4%                12.5%
```

With eight shards, the one that holds the busy customer has 30% of the traffic. Doubling to sixteen takes it to 25%; sixty-four shards, to 21%. It can never go below 20%, because that is one key, and one key is on one shard. Sixty-three shards are nearly idle.

Splitting the customer's own key into eight spreads their traffic: 17.4%. (Not 12.5%: eight sub-keys hashed onto eight shards do not land one on each.)

```text
PART 4 — each shard answers in 5 ms, except one time in a hundred, when it takes 100 ms.
         A query that needs every shard is as slow as the slowest. 100,000 queries each.

  the query asks...                    queries that took 100 ms
  one shard                                                1.0%
  all 8 shards                                             7.9%
  all 64 shards                                           47.2%
```

The same shards, the same rare slowness. Asked one at a time, 1% of queries are slow. Asked all together, 8% — and with sixty-four shards, nearly half. Adding shards makes every scatter-gather query worse.

---

## Trade-offs

**Partitioning scales writes and storage, and takes away joins, transactions and constraints across partitions.** Put it off while a bigger machine, replicas, caches or archiving will do. Design for it early: choosing the key is far cheaper before there is data.

**Range keeps neighbours together and concentrates load; hash spreads load and scatters neighbours.** Choose by whether the common query is a range or a single key — or hash one part of the key and sort by another.

**The partition key makes one kind of query cheap and every other kind expensive.** There is one key. Choose it for the commonest request, and decide in advance how the others will be served.

**More logical partitions make rebalancing finer and bookkeeping heavier.** Hundreds or thousands is typical. The number is fixed for life, so choose it with growth in mind.

**A local index is exact and makes reads scatter; a global index makes reads direct and writes double.** Use a global index for lookups that are frequent, and accept that it can be behind.

**Splitting a hot key spreads its load and scatters its reads.** It is a remedy for particular keys, applied knowingly, not a general scheme.

---

## Practice

### Check yourself

Answer before reading on.

1. A table of sensor readings is partitioned by ranges of the reading's timestamp, one shard per month. What will the load look like? Suggest a key that keeps *one sensor's readings for a day* cheap to fetch without that problem.
2. A system places keys with `hash(key) mod 12` and must grow to 16 shards. Roughly what fraction of keys moves? What would it have been with 1,024 fixed partitions spread over the shards?
3. A table is partitioned by customer. Support staff look orders up by order number, about fifty times a second. Give three ways to serve that, with the cost of each.

<details>
<summary>Answers</summary>

1. **All writes go to the current month's shard**, and all the others sit idle: the key increases with time, so new rows always fall in the last range. Partition instead by **the sensor's ID (hashed), with readings sorted by time inside each partition**. Writes spread across shards as sensors do; one sensor's day is one sorted read on one shard. If a single sensor can produce unbounded data, add the month to the key — sensor and month — so that no partition grows for ever. What becomes expensive is *all sensors' readings for the last minute*, which now asks every shard: decide whether anything needs that, and serve it another way if so (a stream, Chapter 4.4).

2. With `mod`, a key stays only if its hash has the same remainder modulo 12 and modulo 16: that is so for a quarter of keys, and **about three quarters move**. With 1,024 fixed partitions, the four new shards must end up with a quarter of the partitions, so **a quarter of the keys move** — the minimum possible — and they move as whole partitions, which can be copied one at a time.

3. **(a) Ask every shard.** No extra data; each lookup costs every shard a query and is as slow as the slowest. At fifty a second that is tolerable on eight shards and poor on a hundred. **(b) A global index**: a small table, order number → customer, partitioned by order number. One extra read finds the customer and so the shard. Costs a second write for every order, kept in step asynchronously (an outbox, Chapter 4.5), so a brand-new order may not be findable for a moment. **(c) Put the location in the number**: issue order numbers that include the customer's logical partition, so that the number says where to look. No extra table and no scatter; it must be designed before order numbers exist, and it fixes the number of logical partitions for ever. For a new system, (c). For an existing one, (b).

</details>

### Exercise: partition a messaging product

A team-chat product stores messages. It has 200,000 workspaces — most with tens of users, a few with tens of thousands — each with many channels. One PostgreSQL primary can no longer take the writes. The queries, by frequency:

| # | Query | Share |
|---|---|---|
| 1 | The latest 50 messages in a channel | 70% |
| 2 | Post a message to a channel | 20% |
| 3 | Search a workspace's messages for a word | 5% |
| 4 | All of one user's messages in a workspace, for an export | rare |
| 5 | How many messages were sent yesterday, across everything | a daily report |

Choose the partition key and scheme; say how each query is served; say what you do about the largest workspaces and the largest channels; and how you would add capacity.

<details>
<summary>Worked solution</summary>

**The key: the workspace**, hashed into **4,096 logical partitions**, assigned to database servers by a table. Within a partition, messages are indexed by `(channel_id, message_id)`, and message IDs increase with time.

Why the workspace and not the channel: a workspace is the unit of nearly every request, of permissions, and of everything else the product stores — members, channels, settings. With all of a workspace on one shard, posting a message and updating the channel's *last message* marker is one ordinary transaction, and membership checks need no second machine. Partitioning by channel would spread a large workspace's load better, and would make every one of those a cross-shard operation.

| # | Query | How it is served |
|---|---|---|
| 1 | Latest 50 in a channel | One shard, one index range read: `WHERE channel_id = $1 ORDER BY message_id DESC LIMIT 50`. The request carries the workspace, so the router knows the shard |
| 2 | Post a message | One shard, one transaction |
| 3 | Search a workspace | Not this database's job (Chapter 2.8): a search index, fed by the outbox (Chapter 4.5), itself partitioned by workspace |
| 4 | One user's messages | One shard — the workspace's — with a secondary index on `(user_id, message_id)`. A local index is enough, because the query includes the partition key |
| 5 | Yesterday's count, everywhere | A scatter-gather across all shards, run once a day against replicas. Slow, and nobody is waiting. Better: count from the event stream as messages are posted (Chapter 4.7) |

**The largest workspaces** are hot keys: a workspace with 50,000 users may have a thousand times the traffic of the median. Because placement is by a table and not by arithmetic, a logical partition can be moved: the few giants are given partitions on servers of their own, with more capacity. That is the advantage of a table of assignments — exceptions are possible.

**The largest channels** never stop growing. Nothing has to be split yet, because a channel is not a partition; but old messages are read rarely, so messages older than a year move to cheaper storage, by month (Chapter 2.8).

**Adding capacity**: bring up new servers; copy some logical partitions to them while they stay live on the old ones; keep both in step; switch the table's entry for each partition; remove the old copies (Chapter 2.7). One partition at a time, each small enough to copy in minutes — which is why there are 4,096 and not 16.

**What this design gives up**: anything that joins two workspaces, and a unique constraint across all of them. A user's identity — one email, one account, many workspaces — therefore lives in a separate, small, unpartitioned database, with its uniqueness enforced there (Chapter 5.2).

</details>

---

## Production tips and common mistakes

**Tips**

- **Put off partitioning while simpler things work** — and choose the key early, so that IDs and tables are ready for it.
- **Choose the partition key from the commonest request.** Carry it in every request, so that the router never has to ask.
- **Use many fixed logical partitions and a table that maps them to machines.**
- **Keep related tables together**: everything about one customer on that customer's shard.
- **Measure load and size per partition and per key**, and alert on the busiest against the average.
- **Bound the size of a partition**: add a time component to the key where data grows without limit.
- **Put the location in the ID** where rows must be found without the partition key.
- **Keep scatter-gather for reports**, on replicas — never on a path a user waits for.
- **Rehearse moving a partition** before it is urgent.

**Common mistakes**

- **Partitioning by a key that increases with time.**
- **`hash mod N` with N equal to the number of machines.**
- **A key chosen to spread writes**, with the commonest read left to ask every shard.
- **A partition key with a few enormous values.**
- **Adding shards to cure a hot key.**
- **A secondary index assumed to work as it did on one machine.**
- **Transactions quietly spanning shards** after the split.
- **No way to move a single tenant.**
- **Expecting uniqueness across shards from an index on each.**

---

## Key takeaways

- Partitioning puts each row on exactly one of several machines. It is what scales writes and storage, and it removes cross-partition joins, transactions and constraints.
- The partition key decides which queries go to one shard. Choose it from the commonest request.
- Range partitioning keeps neighbouring keys together and can put all new writes on one shard. Hash partitioning spreads load and makes range queries ask everyone.
- `hash mod N` moves nearly everything when N changes. Consistent hashing with many points per node, or a fixed number of logical partitions mapped by a table, moves only what must move.
- A hot key lands on one shard however many there are. It needs its own remedy: a split key, its own shard, a cache.
- A query without the partition key asks every shard and is as slow as the slowest; that worsens as shards are added. Global indexes and IDs that carry their location avoid it.

**Next:** [5.4 Consistency models and CAP](04-consistency-models-and-cap.md) — the words for what a reader may see, and what a network fault forces you to choose.

---

## Sources

- Garrett Fidalgo, [*Herding elephants: Lessons learned from sharding Postgres at Notion*](https://www.notion.com/blog/sharding-postgres-at-notion), Notion, 2021.
- Stanislav Vishnevskiy, [*How Discord Stores Billions of Messages*](https://discord.com/blog/how-discord-stores-billions-of-messages), Discord, 2017.
- Giuseppe DeCandia et al., [*Dynamo: Amazon's Highly Available Key-value Store*](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf), SOSP 2007 (consistent hashing, virtual nodes).
