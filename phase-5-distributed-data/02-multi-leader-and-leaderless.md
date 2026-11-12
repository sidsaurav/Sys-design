# 5.2 Multi-leader and leaderless replication

[Syllabus](../SYLLABUS.md) · Phase 5 · Previous: [5.1 Leader–follower replication](01-leader-follower-replication.md) · Next: 5.3 Partitioning *(planned)*

> **Time:** about 35 minutes to read, 40 minutes of practice.
> **You need:** [Chapter 5.1](01-leader-follower-replication.md); lost updates from [Chapter 2.4](../phase-2-data/04-concurrency-control.md); versions for late messages from [Chapter 4.3](../phase-4-async/03-delivery-semantics.md). The example needs only Go.

**By the end of this chapter you can**

- say why a system would let more than one server accept writes, and what it takes on by doing so;
- explain what *last write wins* keeps, and the two different things it destroys;
- explain how version vectors tell *after* from *at the same time*, and what must then be merged;
- choose read and write quorums for a leaderless store, and say what each choice gives up;
- recognise conflicts that no merge can resolve.

---

## The problem

*An illustrative scenario; the example reproduces it.* To make writes fast for users in Europe and America, and to survive the loss of either region, a company runs a database in each. Both accept writes. Each sends its changes to the other, a fraction of a second later.

- A customer adds a pen to their basket on a laptop, and a lamp on a phone that happens to reach the other region. At checkout the basket holds the lamp. The pen was never there, as far as any log can show.
- A customer corrects their delivery address. The correction is accepted, shown, and an hour later the parcel goes to the old address.
- Two people register the username `ana` in the same second, one in each region. Both are told it is theirs.

In Chapter 5.1 the danger was having two leaders by accident. Here there are two on purpose, and the question that single leadership answered for free — *in what order did the writes happen?* — has no answer that every server agrees on.

---

## The concept

### 1. Why let more than one server accept writes

- **Distance.** A write that must cross an ocean to reach the one leader is slow for half the world.
- **Availability of writes.** With one leader, losing it — or losing the path to it — stops writes until a failover (Chapter 5.1). If every region can accept writes, none has to wait for another. Amazon's Dynamo was built for exactly that. Its paper calls the aim an *always writeable* store: "a data store that is highly available for writes" ([DeCandia et al., *Dynamo: Amazon's Highly Available Key-value Store*](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)).
- **Disconnected clients.** A phone that lets you edit a note with no signal is a leader for that note until it reconnects.

Two families of design allow it. In **multi-leader** replication, a few servers — typically one per region — each act as a leader and replicate to each other. In **leaderless** replication, any replica accepts any write, and the client talks to several at once.

Both give up the same thing: one agreed order of writes.

### 2. Concurrent writes

Two writes are **concurrent** when neither was made with knowledge of the other. It has nothing to do with clocks: the pen and the lamp might have been added minutes apart, if the regions were out of touch for those minutes.

When concurrent writes to the same thing meet, the system must do something. There are three questions: can it *tell* that they were concurrent; what does it *keep*; and do all replicas end up with the *same* result?

### 3. Last write wins, and what it destroys

The simplest rule: stamp every write with the time, and when two meet, keep the later. Every replica applies the same rule and so reaches the same value. Cassandra does this: "Updates resolve according to the conflict resolution rule of last write wins. Cassandra's correctness does depend on these clocks, so make sure a proper time synchronization process is running such as NTP" ([Apache Cassandra documentation, *Dynamo*](https://cassandra.apache.org/doc/latest/cassandra/architecture/dynamo.html)).

It converges, and it destroys two different things.

- **One of every pair of concurrent writes, silently.** Both were confirmed to their users. One is discarded, with no error and no record. This is Chapter 2.4's lost update, with no lock that could have prevented it.
- **The newer of two writes, when clocks disagree.** *Last* means *highest timestamp*, and timestamps come from different machines' clocks. A correction made a second later, on a machine whose clock is three seconds slow, carries the earlier timestamp — and loses, on every replica, for ever (Chapter 5.5).

Last write wins is acceptable where losing a write does not matter — a cache, a *last seen* time — or where concurrent writes to the same key cannot happen, because each key is written once.

### 4. Telling *after* from *at the same time*

To do better, a replica must know which writes a new write had *seen*. That is what a **version vector** records: for each replica, how many of its writes this version includes. Dynamo's description: "Dynamo uses vector clocks [12] in order to capture causality between different versions of the same object. A vector clock is effectively a list of (node, counter) pairs" ([Dynamo paper](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)).

When two versions meet:

- If one's vector includes everything in the other's, it came **after**: it replaces the other. No clock is consulted, so a slow clock cannot make a correction lose.
- If neither includes the other, they are **concurrent**. Both are kept — as *siblings* — until something merges them.

Who merges? The paper is clear that the store alone cannot do it well: "If conflict resolution is done by the data store, its choices are rather limited… On the other hand, since the application is aware of the data schema it can decide on the conflict resolution method that is best suited for its client's experience" ([Dynamo paper](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)). For a shopping basket, the application takes the union of the siblings.

Merging has edges of its own. With that union, the same paper says, adding to the cart is never lost — "However, deleted items can resurface" ([Dynamo paper](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)). A removal is the *absence* of an item, and the union of a basket without the book and a basket with it contains the book. Getting removals right means recording them as facts in their own right — *tombstones* — and data types designed so that every merge is well defined exist under the name CRDTs ([crdt.tech](https://crdt.tech/) collects the literature).

### 5. Better: do not have the conflict

Every scheme above repairs a conflict after it has happened. The cheaper course is to arrange that it cannot.

- **Give each record one home.** All writes to a given user's data go to that user's region. There are several leaders, but only one for any given record, and Chapter 5.1 applies to each.
- **Make writes commute.** *Add 1 to the counter* from two regions can be applied in either order with the same result. *Set the counter to 5* cannot.
- **Write new facts, not new values.** Append an entry to a ledger; do not overwrite a balance. Two appended entries do not conflict.

And some conflicts cannot be resolved at all, only prevented. Two people who were each told that the username `ana` is theirs cannot be merged. Uniqueness, a balance that must not go below zero, the last seat: these need a *single decision*, made in one place — a single leader for that fact, or agreement among a majority (Chapter 5.6).

### 6. Leaderless replication, and quorums

In a leaderless store, a client writes to several replicas directly and reads from several. With **N** replicas of each item, a write is confirmed when **W** have accepted it, and a read asks **R** and takes the newest answer. Dynamo's rule: "R is the minimum number of nodes that must participate in a successful read operation. W is the minimum number of nodes that must participate in a successful write operation. Setting R and W such that R + W > N yields a quorum-like system" ([Dynamo paper](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)).

The arithmetic is an overlap. If W + R > N, the replicas that answered a read and the replicas that took the latest write must have at least one member in common, so the read sees that write. Cassandra's documentation puts it the same way: "at least one of the replicas is guaranteed to participate in both the write and the read request" ([Cassandra documentation](https://cassandra.apache.org/doc/latest/cassandra/architecture/dynamo.html)).

W and R are a dial with three markings.

- **Smaller W and R** are faster — "the latency of a get (or put) operation is dictated by the slowest of the R (or W) replicas" ([Dynamo paper](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)) — and survive more failures.
- **W + R > N** makes a read see the latest confirmed write.
- **W = N or R = N** means that one unreachable replica stops that operation entirely.

Replicas that missed a write are brought up to date afterwards: when a read notices a stale answer and writes the newer value back (*read repair*), and by a background comparison of replicas (*anti-entropy*).

A quorum is not a transaction. It does not order two concurrent writes — section 3 or 4 still decides that — and a write that reached fewer than W replicas has failed as far as its client knows, yet may still be there to be read later.

---

## A real example

### Run it: two regions, and three replicas

[`code/ch02-conflicts/main.go`](code/ch02-conflicts/main.go) — about 270 lines, no dependencies — is a model in one process. Replicas are structs and replication is a function call, made at a moment the program chooses, so every outcome is exact and repeatable.

```bash
go run ./phase-5-distributed-data/code/ch02-conflicts
```

```text
PARTS 1 and 2 — two regions, each accepting writes and sending them to the other afterwards.

  1. A cart holds a book. At the same moment, a pen is added in the EU and a lamp in the US.
     what the user expects                        [book, lamp, pen]
     the latest timestamp wins                    [book, lamp]
     version vectors; concurrent versions merged  [book, lamp, pen]

  2. An address is set to "Elm St" in the EU. A second later — replication has arrived — it is corrected to "Oak St" in the US, whose clock is 3 seconds slow.
     what the user expects                        [Oak St]
     the latest timestamp wins                    [Elm St]
     version vectors; concurrent versions merged  [Oak St]

  3. A cart holds a book and a pen. At the same moment, the book is removed in the EU and a lamp is added in the US.
     what the user expects                        [lamp, pen]
     the latest timestamp wins                    [book, lamp, pen]
     version vectors; concurrent versions merged  [book, lamp, pen]
```

- **Story 1** is the lost pen. Each region wrote the basket as it knew it — *book and pen*, *book and lamp* — and the later timestamp replaced the earlier. With version vectors, neither version had seen the other, so both were kept and merged.
- **Story 2** is the address. These writes were *not* concurrent: the US region had received *Elm St* before the user corrected it. But its clock was slow, the correction carried the earlier time, and the old address won — in both regions. With vectors, the correction is known to have come after, whatever the clocks say.
- **Story 3** is the limit of merging. The user removed the book. Last-write-wins brought it back, because the other region's whole basket had the later time. The union of two baskets brought it back too. Neither result is what the user did; this is the case that needs tombstones.

```text
PART 3 — three replicas and no leader. A write is confirmed when W replicas have it; the third gets it
         a little later. A read asks R replicas and takes the newest answer. 10,000 times: write, then
         read at once.

  W    R    W + R > 3       reads that were         with one replica down,         with one replica down,
                                out of date                  can it write?                   can it read?
  1    1    no                          67%                            yes                            yes
  2    1    no                          33%                            yes                            yes
  1    2    no                          33%                            yes                            yes
  2    2    yes                          0%                            yes                            yes
  3    1    yes                          0%                             NO                            yes
  1    3    yes                          0%                            yes                             NO
```

A client writes, is told the write succeeded, and reads immediately. With W = 1 and R = 1, two reads in three miss the write: the one replica asked is usually not the one replica that had it. Every row with W + R > 3 has no stale reads at all. And the last two rows show why *2 and 2* is the usual choice for three replicas: it is the only overlapping setting that still works, for both reads and writes, with one replica down.

---

## Trade-offs

**One leader gives an order and a bottleneck; many leaders give neither.** A single leader is simple to reason about and is a single place for writes to wait. Several leaders accept writes anywhere, and every pair of concurrent writes becomes the application's problem.

**Last write wins is simple and lossy.** It needs no application code, always converges, and silently discards confirmed writes. Use it knowingly, for data where that is acceptable.

**Version vectors lose nothing and hand you the merge.** The store keeps both versions; someone has to write the function that combines them, for every kind of data, including what removal means.

**Avoiding conflicts is cheaper than resolving them.** A home region per record, or operations that commute, removes most of the problem before any of this machinery is needed.

**Quorums trade freshness, speed and availability against one another.** Larger W and R see newer data and tolerate fewer failures. There is no setting that is best at all three.

**Some guarantees cannot be had this way at all.** Uniqueness and *never below zero* need one decision-maker. A design that needs them needs a leader, or consensus, for those facts.

---

## Practice

### Check yourself

Answer before reading on.

1. Two regions resolve conflicts by last-write-wins, and their clocks are kept within 100 ms of each other. A user's two devices send two different updates to the same record, 20 ms apart, to different regions. Which survives?
2. A leaderless store keeps five replicas of each item. Choose W and R for (a) data where a read must see the latest confirmed write, and reads and writes must both survive two replicas being down; (b) data where writes must be as fast as possible and stale reads are acceptable.
3. Two regions both accept sign-ups. In the same second, each accepts a new user with the username `ana`. Can version vectors and a merge function fix this? What can?

<details>
<summary>Answers</summary>

1. **It cannot be said.** The updates are 20 ms apart in real time, and the clocks may differ by up to 100 ms, so either may carry the later timestamp. Whichever does, wins on both regions; the other is discarded, though it was confirmed. The outcome is consistent between replicas and arbitrary with respect to what the user did — and the same pair of updates, sent again tomorrow, may resolve the other way.

2. (a) **W = 3 and R = 3.** 3 + 3 > 5, so every read overlaps the latest write, and each operation needs only three of the five replicas, so two may be down. (W = 4, R = 2 also overlaps, but a write then fails with two replicas down.) (b) **W = 1 and R = 1.** One replica's answer is enough for either; a read may be served by a replica the latest write has not reached. If the data matters at all, W = 2 costs little and survives the loss of the one replica that took the write.

3. **No.** Vectors can show that the two registrations were concurrent, and that is all. There is no merge of two people into one username: one of them must be told that the thing they were promised is not theirs. Uniqueness is a guarantee about *all* writes, so it needs every claim to pass through one place that says yes or no — a single leader for usernames, or a majority agreeing (Chapter 5.6). In practice: route every username claim to one region; or give each name a home region by its hash, so that both claims for `ana` arrive at the same server and the second is refused.

</details>

### Exercise: which replication for which data

A product runs in two regions and wants to accept writes in both where it safely can. For each kind of data, choose: a single leader; several leaders with a named rule for conflicts; or a design that avoids the conflict. Say what is lost in the worst case.

| # | Data | Written |
|---|---|---|
| 1 | *Last seen at* for each user | On every request |
| 2 | The shopping basket | By the user, sometimes from two devices |
| 3 | An account balance, which must not go below zero | By purchases and top-ups |
| 4 | The user's profile: name, address, preferences | Rarely, by the user |
| 5 | Usernames, which must be unique | At sign-up |
| 6 | *Likes* on a post | By millions of users |

<details>
<summary>Worked solution</summary>

| # | Choice | The rule | Worst case |
|---|---|---|---|
| 1. *Last seen* | Several leaders, **last write wins** | The highest timestamp | A time that is off by the clock skew. Nobody will know |
| 2. Basket | Several leaders, **version vectors, merged by the application** | Union of items, with removals recorded as tombstones so that they survive the merge | An item removed on one device as it was changed on another may need the user's eye at checkout — which shows the basket anyway |
| 3. Balance | **A single leader per account**, in the account's home region | One place decides each debit, with Chapter 2.4's check-and-change statement | A user far from their home region waits for an ocean on each purchase. That is the price of *never below zero* |
| 4. Profile | **A home region per user**; the other region forwards writes there | No conflict arises: one leader per record | Slower writes for a user who is travelling; none lost |
| 5. Usernames | **A single decision point**: one region owns the table of names, or each name's home is fixed by its hash | A unique constraint, in one database | Sign-up depends on reaching that region. A username cannot be promised without it |
| 6. Likes | Several leaders, **an operation that commutes** | Record each like as its own row, keyed by user and post; the count is the number of rows. The same like arriving twice is one row; two different likes are two | A count that is briefly behind in one region |

The pattern across the six:

- **Where a lost write does not matter (1), take the simplest rule.**
- **Where writes can be combined (2, 6), design the data so that combining is well defined** — a set with tombstones, a row per fact — and let both regions write.
- **Where the data has a constraint that spans writes (3, 5), there must be one decision-maker**, and being multi-region does not change that. It only decides where the decision-maker lives.
- **Where conflicts are merely possible (4), remove the possibility** by giving each record a home.

Most data in most products is of the fourth kind, which is why *one leader per record, in its home region* is the commonest multi-region design — and why it is worth exhausting before reaching for anything cleverer.

</details>

---

## Production tips and common mistakes

**Tips**

- **Before adding a second writer, ask whether each record could have one home.** Usually it can.
- **Know which conflict rule your database applies by default.** It is often last write wins, and it is often not said loudly.
- **If you use last write wins, keep clocks synchronised and monitored** (Chapter 5.5) — and accept that confirmed writes will be discarded.
- **Model data so that concurrent writes combine**: sets with tombstones, a row per fact, counters that are sums.
- **Keep constraints that span writes — unique, non-negative, limited stock — behind a single decision-maker.**
- **For three replicas, W = 2 and R = 2**; for five, 3 and 3. Depart from a majority only with a reason.
- **Measure conflicts.** Count siblings created and versions discarded. A number that is never looked at is how the lost pen stays lost.
- **Test with clocks deliberately wrong, and with the regions disconnected for a minute.**

**Common mistakes**

- **Two regions accepting writes, with nobody having chosen a conflict rule.**
- **Trusting last write wins with data that matters.**
- **Assuming that *last* means *most recent*.**
- **Merging by union and forgetting removals.**
- **Expecting a merge to enforce uniqueness, or a balance.**
- **W = 1 and R = 1 "for performance"**, and stale reads immediately after writes.
- **W = N**, and writes that stop when one replica restarts.
- **Believing that a quorum makes two concurrent writes one.**

---

## Key takeaways

- Letting several servers accept writes buys speed near the user and writes that survive a lost region. It gives up a single agreed order of writes.
- Concurrent writes are those made without knowledge of each other. When they meet, something must decide.
- Last write wins converges and discards: one of every concurrent pair, and — when clocks disagree — the newer of two writes that were not concurrent at all.
- Version vectors tell *after* from *concurrent* without clocks. Concurrent versions are kept and must be merged by something that understands the data, removals included.
- The cheapest conflict is the one that cannot occur: one home per record, operations that commute, facts appended and not overwritten.
- In a leaderless store, W + R > N makes a read overlap the latest confirmed write. Majorities are the setting that also survives a failure.
- Uniqueness and other constraints across writes need one decision-maker, however many regions there are.

**Next:** 5.3 Partitioning *(planned)* — when the data no longer fits on one machine, and has to be divided.

---

## Sources

- Giuseppe DeCandia et al., [*Dynamo: Amazon's Highly Available Key-value Store*](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf), SOSP 2007.
- Apache Cassandra documentation: [*Dynamo*](https://cassandra.apache.org/doc/latest/cassandra/architecture/dynamo.html) (versioning, consistency levels).
- [crdt.tech](https://crdt.tech/): papers and resources on conflict-free replicated data types.
