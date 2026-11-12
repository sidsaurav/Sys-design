# 5.4 Consistency models and CAP

[Syllabus](../SYLLABUS.md) · Phase 5 · Previous: [5.3 Partitioning](03-partitioning.md) · Next: 5.5 Time and order *(planned)*

> **Time:** about 30 minutes to read, 35 minutes of practice.
> **You need:** [Chapters 5.1](01-leader-follower-replication.md) and [5.2](02-multi-leader-and-leaderless.md); read-your-writes and monotonic reads from [Chapter 3.6](../phase-3-scaling-out/06-read-replicas.md). The example needs only Go.

**By the end of this chapter you can**

- say what *linearizable*, *causal* and *eventual* consistency each promise a reader, in terms of what they can and cannot see;
- say what the CAP theorem does state, and three things it is often taken to state and does not;
- explain the trade that exists when nothing is broken, between consistency and latency;
- choose a consistency level per operation, and say what it costs.

---

## The problem

*An illustrative scenario; the example reproduces what users see.* A design review. The proposal stores user data in a replicated database, and the slide says: *eventually consistent — acceptable for our use case*. Everyone nods.

Nobody in the room can answer the questions that follow from it. After a user changes their password, for how long does the old one still work? If a user blocks someone and then posts, can the blocked person see the post? If the network between two data centres fails, does the product stop taking orders, or take orders it may later have to cancel? *Eventually consistent* answers none of these. It says only that if everything stops changing, the copies will one day agree.

Chapter 2.3 had the same problem with *transactions are isolated*, and solved it by naming the anomalies each level allows. This chapter does that for replicated data: a short vocabulary for what a reader may observe, and one theorem about what a network fault forces.

("Consistency" here means agreement between copies, as seen by readers. It is not the C of ACID, which is about a database's own rules.)

---

## The concept

### 1. Three promises to a reader

From strongest to weakest.

**Linearizable.** The system behaves as if there were one copy, and every operation happened at a single instant somewhere between its start and its finish. In Jepsen's words, "every operation appears to take place atomically, in some order, consistent with the real-time ordering of those operations: e.g., if operation A completes before operation B begins, then B should logically take effect after A" ([Jepsen, *Linearizability*](https://jepsen.io/consistency/models/linearizable)). What a reader can rely on: once any write has been confirmed, every later read, by anyone, anywhere, sees it. This is what a lock, a unique username or *the last seat* requires.

**Causal.** Operations that depend on one another are seen in order by everyone; unrelated ones may be seen in different orders by different readers. "Causal consistency captures the notion that causally-related operations should appear in the same order on all processes—though processes may disagree about the order of causally independent operations" ([Jepsen, *Causal Consistency*](https://jepsen.io/consistency/models/causal)). A reply is never shown before the message it answers. A user sees their own writes, and never sees time run backwards (Chapter 3.6's two guarantees are parts of this). But two readers may, for a moment, disagree about which of two unrelated posts came first.

**Eventual.** In Werner Vogels's definition, "the storage system guarantees that if no new updates are made to the object, eventually all accesses will return the last updated value" ([Vogels, *Eventually Consistent*](https://www.allthingsdistributed.com/2008/12/eventually_consistent.html)). Note what is absent: any bound on *eventually*, and any promise about what is seen meanwhile. A reader may miss a write that finished a minute ago, may see a newer value and then an older one, and may not see their own write.

| A reader may… | Linearizable | Causal | Eventual |
|---|---|---|---|
| miss a write that someone else finished earlier | No | Yes | Yes |
| miss their own write | No | No | Yes |
| see a value, then an older one | No | No | Yes |
| see an effect before its cause | No | No | Yes |

The right question in the design review is not *is it consistent?* but *which of these rows may our users experience, and for how long?*

### 2. What it costs to be strong

Each stronger promise requires more coordination before answering.

- An **eventual** read is answered by whichever replica is nearest, from what it has.
- A **causal** read is answered by a replica that has at least what this client has already seen — which needs the client to carry a marker of that (Chapter 3.6's position token), and sometimes means asking a second server.
- A **linearizable** read must be sure that nothing newer exists anywhere. That means going to the leader, and the leader checking that it still is the leader — or asking a majority.

Coordination takes time, and it needs the other machines to be reachable. Those two facts are the next two sections.

### 3. CAP: what a partition forces

A **network partition** is a fault that leaves machines running and unable to talk to each other. The theorem, as its author states it: "any networked shared-data system can have at most two of three desirable properties: consistency (C) equivalent to having a single up-to-date copy of the data; high availability (A) of that data (for updates); and tolerance to network partitions (P)" ([Brewer, *CAP Twelve Years Later*](https://www.infoq.com/articles/cap-twelve-years-later-how-the-rules-have-changed/)).

The argument is short. Two replicas cannot communicate, and a client writes to one. The other is then asked to read. It can answer with what it has — which may be out of date — or it can decline to answer. There is no third option.

- Systems that decline are called **CP**: they stay consistent and are unavailable on one side of the split. A leader that needs a majority (Chapter 5.1) is CP: Jepsen's page says of linearizability, "in the event of a network partition, some or all nodes will be unable to make progress" ([Jepsen](https://jepsen.io/consistency/models/linearizable)).
- Systems that answer are called **AP**: every replica keeps working, and they must reconcile afterwards (Chapter 5.2).

Three things the theorem does not say:

- **It is not a choice of two from three.** Partitions happen whether or not they are chosen. Brewer himself: the two-of-three "formulation was always misleading because it tended to oversimplify the tensions among properties" ([same article](https://www.infoq.com/articles/cap-twelve-years-later-how-the-rules-have-changed/)). The choice is what to do *during* a partition.
- **It is not about normal operation.** In the same article: "because partitions are rare, there is little reason to forfeit C or A when the system is not partitioned".
- **It is not one choice per system.** And: "the choice between C and A can occur many times within the same system at very fine granularity; not only can subsystems make different choices, but the choice can change according to the operation or even the specific data or user involved". A shop can refuse to sell the last seat during a partition and still let people browse.

And its definitions are narrow. *Consistency* in CAP means linearizability; *availability* means that every working node answers. Most real systems are neither perfectly one nor the other.

### 4. When nothing is broken: latency

CAP is silent about the other 99.9% of the time. Daniel Abadi's extension names the trade that applies then: "if there is a partition (P) how does the system tradeoff between availability and consistency (A and C); else (E) when the system is running as normal in the absence of partitions, how does the system tradeoff between latency (L) and consistency (C)?" ([Abadi, *Problems with CAP*](https://dbmsmusings.blogspot.com/2010/04/problems-with-cap-and-yahoos-little.html)). It is known as PACELC.

The *else* is the one met every day. A write that waits for a replica in another region is consistent there and slow here. With illustrative round trips — 1 ms within a data centre, 70 ms to another continent:

| A write is confirmed when… | It takes about | A reader on the other continent |
|---|---|---|
| the leader has it | 1 ms | may read stale data locally, in 1 ms |
| a replica in the same region has it too | 2 ms | the same |
| the replica on the other continent has it | 70 ms | reads the latest locally, in 1 ms |

And a reader far from the leader who insists on a linearizable read pays the ocean on every read. This is why so much data is served eventually consistent: not for fear of partitions, but because the alternative is slower all day.

### 5. Choosing, per operation

Consistency is not a property to pick once for a system. Many databases let each request choose. DynamoDB's reads are a clear example. By default "the responses might not reflect the results of a recently completed write operation"; with one parameter, "DynamoDB returns a response with the most up-to-date data, reflecting the updates from all prior write operations that were successful" — at a price that is stated: "Eventually consistent reads are half the cost of strongly consistent reads" ([AWS docs, *DynamoDB read consistency*](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/HowItWorks.ReadConsistency.html)).

So go through the operations, as Chapter 3.6 went through the reads:

- **Needs a single truth** — claiming something unique, spending a balance, taking a lock: linearizable. Accept that it is slower and that it stops on the minority side of a partition.
- **Needs to make sense to one user** — their own profile, a conversation: causal, or its two session guarantees.
- **Needs only to be roughly right, soon** — counts, feeds, recommendations: eventual, with a stated bound on how stale.

---

## A real example

### Run it: what a reader sees

[`code/ch04-consistency/main.go`](code/ch04-consistency/main.go) — about 190 lines, no dependencies — is a model in one process: a leader, two followers, and a counter of ticks for time.

```bash
go run ./phase-5-distributed-data/code/ch04-consistency
```

```text
PART 1 — one value, a leader and two followers that are 1 to 6 ticks behind. Four clients write and
         read for 50,000 ticks. Nothing fails. What can a client observe?

  a read is answered by...                             missed a      went back     missed the         needed a
                                                 finished write        in time   reader's own    second server
                                                                                        write
  any replica                                             58.4%          20.5%          32.5%             0.0%
  any replica that has what this client has seen           5.5%           0.0%           0.0%            52.9%
  the leader                                               0.0%           0.0%           0.0%             0.0%
```

Writes are far more frequent here than in real use, to make the differences large. The three rows are the three promises.

- **Any replica** is eventual consistency. More than half the reads missed a write that had already been confirmed; one in five returned something *older* than the same client's previous read; a third missed the client's own last write. Nothing was broken while this happened.
- **A replica that has what this client has seen** is causal consistency, from one client's point of view. Each client carries the newest version it has seen or written, and a replica that is behind that is not used. Reads never go backwards and never miss the client's own write. They do still miss other people's recent writes, 5.5% of the time — that is allowed. The price is the last column: half of these reads had to go to a second server.
- **The leader** is linearizable — given that it is certain of being the leader, which in a real system is a round of messages on every read. Every column is zero, and every read went to one machine.

```text
PART 2 — three replicas, each with a client beside it, and 50 keys. For 400 of 1,000 ticks the network
         separates replica 1 from replicas 2 and 3.

  during the split...                                        requests    confirmed writes that      reads that missed a
                                                              refused     were later discarded          confirmed write
  the side without a majority refuses requests                    13%                        0                        0
  every replica goes on answering                                  0%                      158                      300
```

The theorem, as two rows. Refusing on the minority side lost 13% of requests — every request from the one client beside replica 1, for as long as the split lasted — and nothing else. Answering everywhere refused nobody. It also confirmed 158 writes that were thrown away when the two sides met again, and gave 300 answers that were missing a write the other side had already confirmed.

Neither row is *correct*. For a shopping basket, the second is plainly better. For the last seat on a plane, the first is.

---

## Trade-offs

**Stronger consistency is easier to program against and costs latency and availability.** With linearizable data, code can be written as if there were one machine. With eventual data, every reader must cope with being behind. The first is paid for on every request; the second in every feature.

**During a partition, choose which side of the theorem each operation is on.** Refuse, and some users see errors. Answer, and some confirmed writes will not survive. Decide per operation, in advance, and tell the user honestly.

**Most of the time, the real trade is with latency.** Waiting for a distant replica buys consistency there at the price of speed here. That is the choice being made every millisecond of an ordinary day.

**Causal consistency is the practical middle.** It is achievable without a single leader, survives partitions, and removes the anomalies that users actually notice — their own writes missing, time running backwards. It needs clients, or sessions, to carry a marker.

***Eventually* needs a number.** A design that relies on it should say how stale a read may be, measure it, and alarm on it (Chapter 3.6). Without that it is not a guarantee of anything.

---

## Practice

### Check yourself

Answer before reading on.

1. A user changes their password. The login service reads user records from the nearest replica. Which consistency model is this, what can go wrong, and which model does this read need?
2. A team says: *we chose AP, so our system is always available*. Give two reasons to push back.
3. A database has a leader in Frankfurt and a replica in Sydney, 250 ms apart. Users in Sydney read their own shopping basket. Give three ways to serve that read, with what each costs and what the user may see.

<details>
<summary>Answers</summary>

1. **Eventual.** After the change, a login attempt that reaches a replica which has not yet applied it is checked against the *old* password. The user's new password is refused, and — worse — the old one, which they changed because it was stolen, still works for as long as that replica is behind. A password check needs to see the latest confirmed write whoever made it: **linearizable**, which here means reading from the leader. If that is too slow for every login, at least make the *change* wait until every replica has it before it is confirmed — moving the cost from the many reads to the rare write.

2. **First, AP is about a partition, and says nothing of everything else that makes a system unavailable**: a bad deploy, an overloaded database, an expired certificate. Partitions are a small part of real downtime. **Second, *available* in the theorem means only that every node gives *an* answer** — not a correct one, and not a fast one. The team should say what users will see when the two sides disagree: which writes are discarded when they rejoin, and how anyone is told. And a third: no system is AP throughout. If any operation in it must be unique or must not be done twice, that operation cannot be on the AP side, whatever the database is called.

3. **(a) Read from the Sydney replica**: a few milliseconds, and the user may not see an item they added a moment ago, or may see it and then not — eventual. **(b) Read from the leader in Frankfurt**: always right, and 250 ms slower, on every view of the basket, for every Sydney user — and it fails if the link to Frankfurt does. **(c) Read from Sydney, but only if it has what this user last wrote; otherwise from Frankfurt**: the user carries a position (Chapter 3.6). Nearly always a few milliseconds; 250 ms just after their own changes; never missing their own write. That is causal consistency for the session, and for a basket it is the one to choose. Better still, if most of the user's requests are in Sydney: make Sydney the home of that user's data (Chapter 5.2).

</details>

### Exercise: a consistency level for each operation

An online bank's app has these operations. For each, choose linearizable, causal (per session) or eventual; say what the user sees during a partition between its two data centres; and what the choice costs on an ordinary day.

| # | Operation |
|---|---|
| 1 | Show the balance on the home screen |
| 2 | Make a payment from the account |
| 3 | Show the list of recent transactions |
| 4 | Change the daily payment limit |
| 5 | Show the branch-finder map |
| 6 | Log in |

<details>
<summary>Worked solution</summary>

| # | Operation | Level | During a partition | Ordinary cost |
|---|---|---|---|---|
| 1 | Show the balance | **Causal**, per session | Shown, marked *as of 10:42* if it cannot be confirmed fresh | A local read; the leader only just after the user's own payment |
| 2 | Make a payment | **Linearizable** | **Refused** on the side without a majority: *we cannot take payments right now* | A round trip to the leader and its synchronous replica on every payment. The balance check and the debit are one decision in one place (Chapter 2.4) |
| 3 | Recent transactions | **Causal**, per session | Shown; may lack the last few minutes of other channels' activity | A local read. The user's own payment must appear at once, or they will make it again |
| 4 | Change the limit | **Linearizable** for the write; and the payment path reads it linearizably | Refused on the minority side | Rare, so the cost does not matter. What matters is that a *lowered* limit takes effect before the next payment, everywhere |
| 5 | Branch finder | **Eventual** | Works | Served from caches and a CDN (Chapter 3.5). A day stale is fine |
| 6 | Log in | **Linearizable** read of the credentials and of whether the account is locked | On the minority side: refused, or allowed with a session that can only read | A read at the leader for each login. A revoked credential that still works on one replica is a security incident, not a stale read |

What the table shows:

- **One product, three levels**, chosen by what a wrong answer costs. Nothing here is *an AP system* or *a CP system*.
- **The linearizable operations are the ones that spend, restrict or admit.** They are few, they are the slow ones, and they are the ones that stop during a partition — which is correct: a bank that cannot be sure of a balance should not pay from it.
- **Everything the user merely looks at is causal or eventual**, and stays up.
- **Honesty is part of the design.** *As of 10:42* turns a stale read from a defect into information.

</details>

---

## Production tips and common mistakes

**Tips**

- **Replace *consistent* with a row of the table**: which anomalies may this reader see, and for how long.
- **Decide per operation.** Linearizable for the few that claim, spend or admit; causal for what a user does and then looks at; eventual, with a bound, for the rest.
- **Decide in advance what each operation does during a partition**, and what the user is told.
- **Measure staleness** where you rely on it being small.
- **Read the database's documentation for its default**, and for what its strong option costs and excludes.
- **Carry a position in the session** to get causal behaviour from eventually consistent replicas.
- **Test the partition**: block traffic between two replicas in a test environment and watch what each side does.

**Common mistakes**

- ***Eventually consistent*, with no bound and no list of what users may see.**
- **Treating CAP as a menu**: choosing two, or choosing once for a whole system.
- **Calling a system AP and then needing a unique constraint from it.**
- **Assuming strong consistency because the test environment has one node.**
- **Linearizable reads everywhere**, and an ocean's latency on every page.
- **Checking a permission, a password or a balance against a replica.**
- **Forgetting that the choice is mostly about latency**, on days when nothing is broken.

---

## Key takeaways

- A consistency model is a statement of what a reader may observe. Linearizable: as if there were one copy. Causal: causes before effects, own writes visible, no going backwards. Eventual: agreement some time after writes stop, and nothing promised meanwhile.
- Stronger models need more coordination before answering, which costs time and requires other machines to be reachable.
- CAP says that during a network partition, a replica that cannot reach the others must either answer with possibly stale data or decline. It is a statement about partitions, not a menu, and the choice can differ per operation.
- When there is no partition, the trade is between consistency and latency.
- Choose the level for each operation by what a wrong answer costs, and decide beforehand what it does when the network splits.

**Next:** 5.5 Time and order *(planned)* — why *the latest write* and *I still hold the lock* are harder to know than they sound.

---

## Sources

- Jepsen: [*Linearizability*](https://jepsen.io/consistency/models/linearizable); [*Causal Consistency*](https://jepsen.io/consistency/models/causal).
- Werner Vogels, [*Eventually Consistent*](https://www.allthingsdistributed.com/2008/12/eventually_consistent.html), 2008.
- Eric Brewer, [*CAP Twelve Years Later: How the "Rules" Have Changed*](https://www.infoq.com/articles/cap-twelve-years-later-how-the-rules-have-changed/), 2012.
- Daniel Abadi, [*Problems with CAP, and Yahoo's little known NoSQL system*](https://dbmsmusings.blogspot.com/2010/04/problems-with-cap-and-yahoos-little.html), 2010 (PACELC).
- AWS documentation: [*DynamoDB read consistency*](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/HowItWorks.ReadConsistency.html).
