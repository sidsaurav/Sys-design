# 5.1 Leader–follower replication

[Syllabus](../SYLLABUS.md) · Phase 5 · Previous: [Capstone 4](../phase-4-async/capstone-notification-platform.md) · Next: 5.2 Multi-leader and leaderless replication *(planned)*

> **Time:** about 35 minutes to read, 40 minutes of practice.
> **You need:** read replicas and lag from [Chapter 3.6](../phase-3-scaling-out/06-read-replicas.md); recovery objectives from [Chapter 2.9](../phase-2-data/09-durability-backups-restore.md). The example runs on Chapter 3.6's three PostgreSQL servers.

**By the end of this chapter you can**

- say what is lost when a leader fails, under asynchronous and under synchronous replication;
- say what synchronous replication costs, in time and in availability;
- list the steps of a failover and how each goes wrong;
- explain split brain, and what *fencing* means;
- decide whether a failover should be automatic.

---

## The problem

*An illustrative scenario; the example reproduces it on real servers.* At 02:10 a network fault separates the primary database from most of the data centre. The failover tool, which can no longer reach the primary, does what it was built to do: it promotes a replica and points the application at it. Service is restored in forty seconds. The on-call engineer goes back to sleep.

In the morning, customers are asking about orders they placed in the evening. The replica that was promoted had been falling behind since 21:00 — a long report was running on it (Chapter 3.6) — and nobody had looked at its lag. Five hours of orders were confirmed to customers by the old primary and are not in the new one.

And the old primary was never dead. It was unreachable, from where the tool stood. A batch job on the far side of the fault could reach it, and went on writing to it all night.

Chapter 3.6 used replicas to scale reads, where the worst outcome was a stale page. This chapter uses them for what they are more often bought for: surviving the loss of the leader. Here the worst outcome is data that was confirmed and is gone — and two servers that each believe they are in charge.

---

## The concept

### 1. One leader

All writes go to one server, the **leader** (PostgreSQL: the *primary*). It sends its log to **followers** (*standbys*, *replicas*), which apply it in the same order. Because one server decides the order of all writes, there is nothing to reconcile: followers can only be behind, never different.

That holds for as long as there is exactly one leader. Everything hard in this chapter is about the moments when there might be none, or two.

### 2. Asynchronous: the leader does not wait

By default the leader commits, answers the client, and ships the log afterwards. A follower is therefore always a little behind (Chapter 3.6), and if the leader is lost, so is whatever the follower had not yet received. The manual is direct about it: "If the primary server crashes then some transactions that were committed may not have been replicated to the standby server, causing data loss. The amount of data loss is proportional to the replication delay at the time of failover" ([PostgreSQL docs, *Log-Shipping Standby Servers*](https://www.postgresql.org/docs/current/warm-standby.html)).

So an asynchronous failover is a restore from a copy that is slightly old — or, if nobody was watching the lag, very old. In the language of Chapter 2.9, its recovery point is not zero.

### 3. Synchronous: the leader waits

The leader can instead wait, before answering the client, until a follower has the write: "Synchronous replication offers the ability to confirm that all changes made by a transaction have been transferred to one or more synchronous standby servers" ([same page](https://www.postgresql.org/docs/current/warm-standby.html)). A write that was confirmed then exists on two machines, and losing the leader loses nothing that anyone was told had succeeded.

It has two prices.

- **Every write takes a round trip to the follower longer.** A fraction of a millisecond in the same data centre; tens of milliseconds to another region.
- **The leader now depends on the follower.** If the follower cannot answer, the leader cannot confirm, and writes stop. One machine's failure has become two machines' outage — the multiplication of Chapter 1.1, applied to the database.

The usual answer to the second is to wait for *any one of several* followers. PostgreSQL calls it quorum-based: "The keyword ANY, coupled with num_sync, specifies a quorum-based synchronous replication and makes transaction commits wait until their WAL records are replicated to at least num_sync listed standbys" ([PostgreSQL docs, replication settings](https://www.postgresql.org/docs/current/runtime-config-replication.html)). With two followers and `ANY 1`, either may fail without stopping writes, and every confirmed write is still on two machines.

### 4. Failover, step by step

Replacing a failed leader is five steps, and each has its own way to go wrong.

| Step | What it is | How it goes wrong |
|---|---|---|
| **Detect** | Decide that the leader is dead | A leader that is slow, or unreachable *from where you are looking*, is not dead. Decide too quickly and you fail over a healthy system; too slowly and the outage is long |
| **Choose** | Pick the follower to promote | Picking one that is behind loses everything the others had. Compare how far each has got, and take the furthest |
| **Promote** | Make it accept writes | — |
| **Redirect** | Send clients to the new leader | Clients with the old address cached — a pool, a DNS entry, a configuration file — keep going to the old one |
| **Fence** | Make sure the old leader cannot accept writes | If it is alive and anything can reach it, there are now two leaders |

The last step is the one most often missing, because in the usual rehearsal — pull the plug on the leader — it is not needed.

### 5. Two leaders

A failed leader that is really dead is the easy case. The dangerous one is a leader that is alive and cut off. The failover promotes a follower; the old leader, unaware, continues to accept writes from whoever can still reach it. This is **split brain**: two servers, each confirming writes, each building a history the other does not have.

The histories cannot be merged afterwards. Both servers gave out the next order number. Both sold the last seat. PostgreSQL's manual describes the requirement: "you must have a mechanism for informing the old primary that it is no longer the primary. This is sometimes known as STONITH (Shoot The Other Node In The Head), which is necessary to avoid situations where both systems think they are the primary, which will lead to confusion and ultimately data loss" ([PostgreSQL docs, *Failover*](https://www.postgresql.org/docs/current/warm-standby-failover.html)).

**Fencing** is the general word for making the old leader harmless before the new one starts:

- **Cut it off**: power it down, or remove it from the network, through a channel that does not depend on the leader's cooperation.
- **Let only a majority elect a leader**: with three or more voters, the side of a partition that holds fewer than half cannot have a leader at all (Chapter 5.6).
- **Give the leader a lease** that it must keep renewing, and make storage refuse writes from a holder whose lease is out of date (Chapter 5.5).

Synchronous replication also acts as a brake here, as the example shows: a leader that must wait for a follower cannot confirm anything once its follower has been promoted away.

### 6. Automatic, or by hand?

**Automatic failover** recovers in seconds, at any hour, and will one day act on a false alarm — as in the opening scenario, where a healthy primary was replaced by a stale replica because of a network fault. **Manual failover** waits for a person, who can check the lag and the network before deciding; the outage lasts as long as that takes.

A common position between the two: automatic, but only with protections that make a wrong decision survivable — synchronous replication to at least one follower, a majority vote to decide, fencing before promotion, and a refusal to promote any follower that is further behind than a set limit.

---

## A real example

### Run it: one failover, three ways

[`code/ch01-failover/main.go`](code/ch01-failover/main.go) — about 210 lines — runs against a real PostgreSQL primary and two replicas: the three servers of Chapter 3.6. It promotes replicas, so start a fresh cluster for each run.

```bash
bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start
go run ./phase-5-distributed-data/code/ch01-failover
bash phase-3-scaling-out/code/ch06-replicas/cluster.sh stop
```

No server is killed. *The primary becomes unreachable* is all a failover tool ever knows, and it is all the program arranges.

```text
PART 1 — eight clients write orders to the primary. Replication is asynchronous, as it is by default.
         Replica B loses its connection to the primary. One and a half seconds later the primary
         becomes unreachable, and a replica must be promoted.

  writes that the primary confirmed to its clients: 18264

  if the new primary is...                                confirmed writes it lacks
  replica A, which was connected to the end                                       0
  replica B, cut off 1.5 seconds before                                       10569
```

Both replicas are "the replica". One has everything. The other is missing 10,569 orders, every one of which a client was told had been saved. A failover tool that promotes whichever replica answers first has an even chance of choosing the second. This is the opening scenario, with a second and a half in place of five hours.

(Replica A lacking nothing is the good case of asynchronous replication, on one machine with no network between the servers. It is a likelihood, not a promise.)

```text
PART 2 — the failover tool promotes replica B. But the old primary never stopped: it had only become
         unreachable from where the tool was looking. For one second, some clients write to each.

  orders accepted by the old primary:    6110
  orders accepted by the new primary:    6511
  of the new primary's order numbers, how many the old primary has given to other orders: 6511
  for example, order 7723: written by "the primary" on one server, and by "the new primary" on the other
```

Split brain, for one second. Both servers accepted writes and confirmed them. And every one of the 6,511 order numbers that the new primary handed out had already been given, by the old primary, to a different order: replica B's counter resumed from where B had been cut off. *Order 7723* now means two different things, depending on which server is asked. No tool can merge these. Somebody has to choose a survivor, and go through the other's orders by hand.

```text
PART 3 — the old primary and replica A, now with synchronous replication: a write is confirmed to
         the client only once A has it too.

  one write takes: 0.28 ms asynchronously, 0.51 ms synchronously (median of 200)

  the primary becomes unreachable while clients are writing, and A is promoted.
  writes confirmed to clients: 7458       of those, missing on A: 0

  a client that can still reach the old primary tries to write. After 2.0 s: timeout: context deadline exceeded
  The old primary will not confirm a write that its standby has not received — and its standby has left.
```

The same failover with synchronous replication. Each write took about twice as long — 0.23 ms more, on one machine; across a real network it is the network's round trip. Clients were writing at the instant A was promoted, and of 7,458 confirmed writes, A is missing none.

The last lines are the second price, and also the brake. Left alone, the old primary cannot confirm a write: the client waited two seconds and was told nothing. That is an outage for that client — and it is why this pair of servers cannot do what Part 2's did. (The client's write is in an unknown state: Chapter 1.5's third outcome. It must be retried, safely, against the new primary.)

### GitHub, 21 October 2018

GitHub published a detailed account of a failover that went this way. Maintenance on network equipment cut one data centre off: "Connectivity between these locations was restored in 43 seconds, but this brief outage triggered a chain of events that led to 24 hours and 11 minutes of service degradation." In those 43 seconds the automated tool promoted database primaries in another data centre. The old primaries had not been dead, and had accepted writes that never reached the new ones: "The database servers in the US East Coast data center contained a brief period of writes that had not been replicated to the US West Coast facility. Because the database clusters in both data centers now contained writes that were not present in the other data center, we were unable to fail the primary back over to the US East Coast data center safely" ([GitHub, *October 21 post-incident analysis*](https://github.blog/news-insights/company-news/oct21-post-incident-analysis/)).

That is Part 1 and Part 2 of the example, at the scale of a company: an automatic failover, asynchronous replication, two sets of writes — and a day spent reconciling them.

---

## Trade-offs

**Asynchronous replication is fast and available, and loses the last moments.** Usually milliseconds of writes; occasionally, when lag was not being watched, far more. Acceptable where a few lost writes can be tolerated or recovered some other way.

**Synchronous replication loses nothing confirmed, and makes writes slower and dependent on a second machine.** Use it for data that must not be lost, wait for any one of at least two followers, and keep them near enough that the round trip is bearable.

**Automatic failover shortens outages and sometimes causes them.** Its detector cannot tell *dead* from *unreachable*. Make its mistakes survivable before making it automatic.

**A quick detector fails over healthy leaders; a patient one prolongs real outages.** There is no correct timeout, only one chosen with both costs in mind (Chapter 1.5).

**Fencing costs machinery and removes the worst outcome.** Lost writes can be apologised for. Two diverged histories take days to untangle, and some of the damage — two customers holding the same order number — has already left the building.

---

## Practice

### Check yourself

Answer before reading on.

1. A primary handles 2,000 writes a second. Its only replica is asynchronous and 200 ms behind. The primary's disk dies, and the replica is promoted. What is lost, and what do customers notice?
2. A primary has one synchronous follower. The follower's machine fails. What happens to writes? Give two ways the setup could have been arranged so that this would not be an outage, and what each gives up.
3. After a failover, the old primary comes back online, and several application instances — which still have its address in a connection pool — write to it. What is this called, what is the damage, and what should have prevented it?

<details>
<summary>Answers</summary>

1. About **400 confirmed writes** — 0.2 seconds at 2,000 a second — existed only on the dead disk. Each was confirmed to its client. Customers notice orders, payments or messages that they saw succeed and that are now absent; some were charged by a payment provider for an order the database no longer has. And numbers given out in that fifth of a second — order numbers, invoice numbers — will be given out again by the new primary to different things. The loss is small and its consequences are not: it needs reconciliation against whatever else recorded those writes (the provider, the outbox's consumers, the logs).

2. **Writes stop.** The primary waits for a confirmation that cannot come. **One arrangement:** two followers, with the primary waiting for any one of them. Either can fail without an outage and every confirmed write is still on two machines; the cost is a third server. **Another:** fall back to asynchronous when the follower is lost. Writes continue, and for as long as that lasts, a failure of the primary would lose data — so the fallback must be a deliberate, alarmed, time-limited decision, not a default. Most teams that need the guarantee choose the first.

3. **Split brain.** Two servers accept writes; their histories diverge; sequences give out the same numbers to different rows; and nothing can merge them afterwards — someone must choose a winner and reapply or discard the other's writes by hand. It should have been prevented at two points. **Fencing:** the old primary should have been made unable to accept writes *before* the replica was promoted, and unable to start as a primary when it came back. **Redirection:** applications should find the primary by asking one authority that the failover updates — a proxy, or a service-discovery entry with a short lifetime — and not by remembering an address.

</details>

### Exercise: a failover policy for the ticket database

Capstone 2's booking system had this requirement: *a confirmed sale must survive the loss of a server.* Design the replication and failover for its PostgreSQL database: how many servers and where; synchronous or not; how a failure is detected and who decides; how the old primary is fenced; how the application finds the primary; and what you would test before trusting it.

<details>
<summary>Worked solution</summary>

| Decision | Choice | Why |
|---|---|---|
| Servers | A primary and **two followers, in three separate zones** of one region | One zone can be lost. The same region keeps the round trip to a millisecond or two |
| Replication | **Synchronous, to any one of the two** (`ANY 1`) | A confirmed sale is on two machines. One follower can fail, or be restarted for maintenance, without stopping sales |
| What a sale costs | One round trip to the nearer follower, per commit | Against a 300 ms objective, a millisecond or two |
| Detection | Three or more voters — the database servers' agents, or a small coordination cluster (Chapter 5.6) — must **agree** that the primary is gone, after missed heartbeats over about 10 seconds | One observer cannot tell dead from unreachable. A majority on one side of a partition can at least be sure that the other side has no majority |
| Choice | The follower that has received the most log. Never one that was not synchronous at the time | The synchronous follower has every confirmed sale |
| Fencing | The primary holds a lease that it renews through the same voters. If it cannot renew, **it stops accepting writes by itself**, before the lease can be granted to anyone else. A server coming back up asks who the primary is before accepting anything | The old primary fences itself, without needing to be reached |
| Finding the primary | Applications connect through a proxy that follows the voters' decision; pools reconnect on error | No address is remembered anywhere |
| Automatic? | Yes — given all of the above | With synchronous replication and fencing, a wrong failover costs seconds of unavailability, and no data |

**What is still lost in a failover**: nothing that was confirmed. Sales in flight at that instant get an error or a timeout, and their outcome is unknown to the client; because reserving and paying are idempotent (Capstone 2), the client retries against the new primary and gets the right answer.

**What to test, before trusting it** — on a copy of production, with sales running:

1. Kill the primary's process. Sales resume within the expected time; the invariants of Capstone 2 still hold; no confirmed sale is missing.
2. Cut the primary off from the network without killing it. It stops accepting writes *by itself* within the lease period. This is the test that most setups have never run.
3. Stop one follower. Sales continue.
4. Stop both followers. Sales stop, loudly — and resume when one returns.
5. Bring the old primary back. It rejoins as a follower, and does not accept a write at any moment.

Repeat them on a schedule. A failover that has not been exercised this quarter is a hypothesis.

</details>

---

## Production tips and common mistakes

**Tips**

- **Decide, for each database, how much confirmed data may be lost in a failover.** If the answer is *none*, replication must be synchronous.
- **With synchronous replication, have two followers** and wait for either.
- **Alert on replication lag** (Chapter 3.6). In a failover, lag is data loss.
- **Promote the follower that is furthest ahead**, and refuse to promote one that is behind by more than a limit.
- **Fence before promoting.**
- **Have applications find the leader through one authority**, not a remembered address.
- **Make clients retry safely**: writes in flight during a failover have unknown outcomes.
- **Rehearse failover, including the case where the old leader is alive.**
- **After any failover, reconcile**: compare what was confirmed — by payment providers, by published events — with what the new leader holds.

**Common mistakes**

- **Assuming a replica is a copy** without knowing how far behind it is.
- **Automatic failover over asynchronous replication, with no limit on lag.**
- **Testing failover only by killing the leader**, never by isolating it.
- **No fencing**, and an old leader that returns and accepts writes.
- **One synchronous follower**, so that either machine's failure stops writes.
- **A failure detector with a timeout of a few seconds** across a network that sometimes pauses for longer.
- **Addresses of the leader in configuration files.**
- **Calling a replica a backup** (Chapter 2.9).

---

## Key takeaways

- One leader orders all writes; followers copy it. The difficulty is in changing leader.
- Asynchronous replication loses, in a failover, whatever the promoted follower had not received. That is proportional to its lag — which may be far more than anyone assumed.
- Synchronous replication loses nothing that was confirmed. It makes each write wait for a follower, and stops writes when no follower can answer; waiting for any one of two is the usual arrangement.
- A failover must detect, choose, promote, redirect and fence. Fencing is the step that is forgotten.
- A leader that is unreachable is not necessarily dead. If it is alive and reachable by anyone after a promotion, there are two leaders, and their histories cannot be merged.
- Make automatic failover's mistakes survivable — synchronous replication, a majority to decide, fencing — before making it automatic.

**Next:** 5.2 Multi-leader and leaderless replication *(planned)* — what changes when more than one server is allowed to accept writes on purpose.

---

## Sources

- PostgreSQL documentation: [*Log-Shipping Standby Servers*](https://www.postgresql.org/docs/current/warm-standby.html) (asynchronous and synchronous replication); [*Failover*](https://www.postgresql.org/docs/current/warm-standby-failover.html); [replication settings](https://www.postgresql.org/docs/current/runtime-config-replication.html) (`synchronous_standby_names`).
- GitHub, [*October 21 post-incident analysis*](https://github.blog/news-insights/company-news/oct21-post-incident-analysis/), 2018.
