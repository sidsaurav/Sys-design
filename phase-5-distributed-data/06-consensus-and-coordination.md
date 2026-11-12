# 5.6 Consensus and coordination

[Syllabus](../SYLLABUS.md) · Phase 5 · Previous: [5.5 Time and order](05-time-and-order.md) · Next: [5.7 Distributed transactions](07-distributed-transactions.md)

> **Time:** about 35 minutes to read, 40 minutes of practice.
> **You need:** failover and split brain from [Chapter 5.1](01-leader-follower-replication.md); linearizability and CAP from [Chapter 5.4](04-consistency-models-and-cap.md); leases and fencing tokens from [Chapter 5.5](05-time-and-order.md). The example needs only Go.

**By the end of this chapter you can**

- say what problem consensus solves, and recognise it in disguise;
- explain why majorities make two leaders impossible, and what a cluster of three or five survives;
- describe, in outline, how Raft elects a leader and decides that a write is safe;
- say what a coordination service such as etcd or ZooKeeper offers, and build leader election on one;
- decide when a design needs consensus, and when it only thinks it does.

---

## The problem

*An illustrative scenario; the example reproduces it.* A billing service runs on five machines, and exactly one of them must run the nightly invoicing. The team writes a leader election: every node sends a heartbeat each second; a node that hears no leader for ten seconds declares itself the leader. It is forty lines of code, and it passes its tests, in which a leader is killed and another takes over.

One night a switch fails and the five machines are split, two and three. The two include the leader, which carries on. The three hear nothing for ten seconds, and one of them declares itself leader — correctly, by the rule. Both leaders run the invoicing. Each records what it did in its own copy of the state. When the network is repaired, there are two sets of invoices and one set of customers.

Chapters 5.1, 5.2 and 5.5 each ended at the same place: *this needs one decision-maker*. A single leader for a database. One authority for a unique username. A lock that only one client holds. This chapter is about how several machines can be that one decision-maker — and about the fact that almost nobody should write the algorithm themselves.

---

## The concept

### 1. One decision, among several machines

**Consensus** is getting several machines to agree on a value, such that the agreement survives some of them failing, and is never changed once made. The Raft paper's opening: "Consensus algorithms allow a collection of machines to work as a coherent group that can survive the failures of some of its members" ([Ongaro and Ousterhout, *In Search of an Understandable Consensus Algorithm*](https://raft.github.io/raft.pdf)).

Many problems are this one in disguise:

- **Who is the leader?** All must agree, and there must be only one.
- **What is the next entry in the log?** All must apply the same writes in the same order.
- **Who holds the lock?** And which number is its fencing token (Chapter 5.5)?
- **Is this username taken?** Two claims, one winner.

Each asks several machines to make *one* decision. The home-made election fails because each side of the split makes its own.

### 2. Majorities

The idea that makes agreement possible is small: **a decision counts only if more than half of all the machines take part in it.**

Any two majorities of the same group have at least one member in common. So two different decisions cannot both have a majority, as long as no member supports both — and each member is made to support only one.

Two consequences follow.

- **The smaller side of a split cannot decide anything.** It waits. That is the CP side of Chapter 5.4: correct, and unavailable for the minority.
- **The size of the cluster sets what it survives.** A cluster works while a majority can talk to each other. etcd's documentation gives the table and the advice: "It is recommended to have an odd number of members in a cluster. An odd-size cluster tolerates the same number of failures as an even-size cluster but with fewer nodes" ([etcd docs, *FAQ*](https://etcd.io/docs/v3.5/faq/)).

| Members | Majority | Failures survived |
|---|---|---|
| 3 | 2 | 1 |
| 4 | 3 | 1 |
| 5 | 3 | 2 |
| 7 | 4 | 3 |

Four is no better than three, and adds a machine that can fail. Three and five are the usual choices.

### 3. Raft, in outline

Raft is the consensus algorithm that most engineers meet first, because it was designed to be explained: "Raft is a consensus algorithm for managing a replicated log" ([Raft paper](https://raft.github.io/raft.pdf)). In outline:

**Terms.** Time is divided into numbered **terms**. Each term begins with an election. Every message carries its sender's term, and a node that sees a higher term than its own adopts it — and, if it was a leader, stops being one.

**Elections.** A follower that hears nothing from a leader for a while becomes a candidate: it starts a new term, votes for itself, and asks the others. Each node gives one vote per term. "A candidate that receives votes from a majority of the full cluster becomes the new leader" ([Raft paper](https://raft.github.io/raft.pdf)). Since two candidates cannot both collect a majority of single votes, the paper can state as a guarantee: "Election Safety: at most one leader can be elected in a given term."

Two candidates standing at once could split the vote and elect nobody, again and again. The remedy is a little randomness: "Raft uses randomized election timeouts to ensure that split votes are rare and that they are resolved quickly" ([Raft paper](https://raft.github.io/raft.pdf)).

**The log.** The leader takes each write, appends it to its log, and sends it to the followers. When a majority has stored the entry it is **committed**: only then is the client told that the write succeeded, and only then is it applied. A committed entry is never lost, because of one more rule in the election — a node refuses its vote to a candidate whose log is behind its own. Any new leader therefore already has every committed entry.

**The old leader.** After a split, a leader on the smaller side may go on believing it leads. That cannot be prevented: it has not heard otherwise. What matters is that it can do nothing with the belief. It cannot gather a majority, so it cannot commit, so it cannot confirm a write. And as soon as it meets a node with a higher term, it steps down. The term is a fencing token (Chapter 5.5), checked by every node on every message.

### 4. What it gives, and what it costs

What comes out is a log — and so a small database — that behaves as one copy (linearizable, Chapter 5.4) and keeps working while a majority of its members can reach each other.

The costs:

- **Every write waits for a majority**, each of which must store it durably. That is a network round trip and a disk flush on the path of every write.
- **All writes go through one leader.** Adding members does not add capacity — the reverse: "Although larger clusters provide better fault tolerance, the write performance suffers because data must be replicated across more machines" ([etcd FAQ](https://etcd.io/docs/v3.5/faq/)).
- **Without a majority there is no service.** "An etcd cluster operates so long as a member quorum can be established."
- **It is exacting to implement.** The outline above omits most of what makes it correct: membership changes, log compaction, what a restarted node may forget. Use an implementation that has been tested by people whose job that is.

### 5. Coordination services

Because consensus is hard to build and needed by many systems, it is packaged. **etcd**, **ZooKeeper** and **Consul** are each a small, strongly consistent store run on three or five machines, kept in agreement by a consensus algorithm. They hold little data, and offer a few primitives that everything else is built from:

- **Keys with versions**, and a write that succeeds only if the version is what the writer expects (Chapter 2.4's check-and-change, across machines).
- **Leases**: a key that disappears if its owner stops renewing it.
- **Watches**: being told when a key changes, instead of polling it.

(etcd's [API documentation](https://etcd.io/docs/v3.5/learning/api/) describes all three.) From those:

- **Leader election.** Each candidate tries to create the same key, attached to its lease. The store's consensus decides who succeeds. If the leader dies, its lease expires, the key goes, and the others — who were watching — try again. The key's version is the fencing token.
- **Locks**, the same way.
- **Configuration and membership** that every instance must see identically: which database is the primary, which shard is where (Chapter 5.3), which feature is on.

Kubernetes is built this way. Its state lives in etcd, and its own components elect leaders with it: "Kubernetes also uses Leases to ensure only one instance of a component is running at any given time" ([Kubernetes docs, *Leases*](https://kubernetes.io/docs/concepts/architecture/leases/)).

### 6. When to use one, and when not

**Use a coordination service for** deciding who leads; locks that come with fencing tokens; and small, critical facts that must be the same everywhere.

**Do not use one for** application data, anything written on every request, large values, or queues. Every write is a consensus round. These are stores for a few thousand keys that change occasionally, not for a million sessions that change constantly.

**And first ask whether you need one at all.**

- If the system already has a single-leader database, that database *is* a place where one decision is made. A unique constraint decides who gets the username. A row lock, or an advisory lock, decides who runs the job — with a version column as the token. That is sound for as long as the database's own failover is (Chapter 5.1), and it adds nothing new to run.
- If work can be divided so that each piece has one owner — by partition (Chapter 4.4) — there is nothing to elect.
- If the work is idempotent (Chapter 4.3), two leaders for a moment do no harm.

A design that needs none of these is simpler than any that needs consensus.

---

## A real example

### Run it: five nodes and a network that splits

[`code/ch06-election/main.go`](code/ch06-election/main.go) — about 320 lines, no dependencies — is a model in one process. It implements the part of Raft described in section 3 — terms, votes, majorities, and confirming a write only when a majority has it — and, for comparison, the home-made rule. It is not a Raft implementation and must not be used as one.

```bash
go run ./phase-5-distributed-data/code/ch06-election
```

```text
PART 1 — five nodes. The rule: a node that hears from no leader for 10 ticks becomes the leader.
         After 60 ticks the network splits, with the leader and one other node on the small side,
         for 100 ticks.

  ticks on which two nodes believed they were the leader         92
  writes confirmed to clients during the split                  191
  confirmed writes missing once the network was whole again      90
```

The opening scenario. For nearly the whole split there were two leaders. Both took writes from the clients on their side and confirmed them: 191 in all. When the network was mended one leader's log replaced the other's, and 90 writes that clients had been told were safe no longer existed.

```text
PART 2 — the same five nodes and the same split. The rule: a node that hears from no leader starts
         an election in a new term, and leads only if a majority of all five vote for it. A leader
         confirms a write only when a majority has it.

  ticks on which two nodes believed they were the leader         89
  ticks on which two nodes led in the SAME term                  0
  writes confirmed to clients during the split                   88
  confirmed writes missing once the network was whole again       0
```

Read the first line carefully: for 89 ticks, two nodes *still* believed they were the leader. The old one, on the small side, had no way to know that it had been replaced. But it was leader of an old term, could reach only one other node, and so confirmed nothing. Only the majority side's 88 writes were confirmed, and all 88 survived.

The client on the small side got no confirmations for the whole split. That is the price, and it is the right behaviour: *I cannot tell you this is safe*.

```text
PART 3 — 500 runs of 400 ticks. Every 20 to 60 ticks the network is cut in two at random, or mended.

                                    runs in which two nodes      runs with two leaders         runs that lost a
                                          believed they led           in the same term          confirmed write
  the home-made rule                                    500          (it has no terms)                      488
  terms, votes and majorities                           421                          0                        0
```

Five hundred different histories of splits. The home-made rule lost confirmed writes in 488 of them. With votes and majorities, two nodes believed they led in 421 runs — and in none were there two leaders of one term, and in none was a confirmed write lost.

That middle number is the lesson of Chapter 5.5 again. Consensus does not stop a stale leader from *existing*. It makes the stale leader powerless, by having every other node check its term.

---

## Trade-offs

**Consensus gives one copy's behaviour from several machines, and charges a majority round trip for every write.** It is the right tool for decisions that must be single, and far too expensive for ordinary data.

**A majority tolerates failures and refuses the minority.** Three members survive one failure; five survive two. The side of a split without a majority stops. That is not a defect to be engineered away: it is what prevents two decisions.

**More members mean more failures survived and slower writes.** Five is usually the most that is worth it.

**Building it is a research project; running it is an operational commitment.** Use an existing coordination service, give it fast disks and a quiet network, and keep it small.

**A coordination service is a dependency for everything that uses it.** If leader election needs etcd, then without etcd there is no leader. Keep it off the path of ordinary requests: read what it says, keep it in memory, and watch for changes.

**Often the database you already have will do.** A unique constraint or a locked row is a single decision, made by a system you already operate.

---

## Practice

### Check yourself

Answer before reading on.

1. A cluster of four nodes is split two and two. What happens? What would have happened with three nodes split two and one? With five split three and two?
2. In a Raft cluster, the old leader is on the smaller side of a split and still believes it leads. A client sends it a write, and then a read. What should happen to each?
3. A team proposes to keep user sessions — two million of them, each updated on every request — in etcd, *because it is strongly consistent*. What is wrong with the plan?

<details>
<summary>Answers</summary>

1. **Four, split two and two: neither side has a majority** (three of four), so neither can elect a leader or confirm a write. The whole cluster is unavailable until the split heals. **Three, split two and one**: the two have a majority and continue; the one waits. **Five, split three and two**: the three continue. An even number of members adds a way to be completely stopped, and no extra failure survived.

2. **The write is appended to the old leader's log and never confirmed**: it cannot reach a majority. The client gets no answer and times out, not knowing — and must retry, idempotently, against whoever the leader really is. When the split heals, the old leader sees a higher term, steps down, and discards that entry. **The read is the subtle one.** If the old leader answers from its own state, the answer may be stale: the real leader may have confirmed newer writes. To be linearizable, a leader must confirm that it still leads — by reaching a majority — before answering a read. A system that skips that check for speed is serving reads that may be out of date, and should say so.

3. **Every write to etcd is a consensus round**: sent to a majority, written durably by each, on one leader. Two million sessions touched on every request is thousands of such writes a second, into a store designed to hold a small amount of data that changes occasionally. It would be slow, and — worse — it would put the thing that elects leaders and holds configuration under constant heavy load, so that a traffic spike could cost the cluster its coordination. Sessions do not need consensus at all: they need a store that is fast and whose staleness is tolerable (Chapter 3.1). Strong consistency is a cost to be paid where it is needed, not a quality to collect.

</details>

### Exercise: exactly one scheduler

A scheduler triggers jobs: *send the weekly report*, *expire unpaid orders*. Three instances run, for availability, and each job must be triggered once. Design it twice: with a coordination service, and with only the PostgreSQL database the system already has. For each, say how a leader is chosen, what the leader does when it cannot confirm that it still leads, and what stops a stale leader's work from taking effect.

<details>
<summary>Worked solution</summary>

**With a coordination service**

- Each instance tries to create the key `scheduler/leader`, attached to a lease of 15 seconds that it renews every 5. The store decides who succeeds. The others watch the key.
- The store returns a **revision number** for that key. It increases every time the leadership changes. This is the fencing token.
- The leader measures its lease with its monotonic clock, from when it sent each renewal (Chapter 5.5). **If a renewal fails, or the lease would end within a margin, it stops triggering jobs at once** — before the lease can be granted to another — and goes back to waiting.
- **Every trigger carries the revision number.** A trigger is a row inserted into the jobs table (which is also the queue, Chapter 4.2), and the insert is conditional: `… WHERE NOT EXISTS (a run of this job for this scheduled time)`. A stale leader that wakes after a pause, and tries to trigger *weekly report for 2026-03-09*, finds the row already there.

**With only PostgreSQL**

- No election at all. Each instance, every few seconds, tries to claim the job runs that are due:

```sql
INSERT INTO job_runs (job, scheduled_for)
SELECT job, next_due FROM schedules WHERE next_due <= now()
ON CONFLICT (job, scheduled_for) DO NOTHING
RETURNING job, scheduled_for;
```

- The **unique constraint** on `(job, scheduled_for)` is the single decision. Three instances may all try; one insert succeeds for each run, and the others get nothing back. Whoever inserted the row does the triggering — or better, the row itself is the job that workers take (Chapter 4.2).
- There is no leader to be stale. An instance that was paused for a minute wakes, tries to insert, and conflicts.

**Comparing them**

| | Coordination service | The database's constraint |
|---|---|---|
| New things to run | A three-member cluster | None |
| What makes *once* true | The conditional insert — the election only reduces wasted attempts | The unique constraint |
| When the coordinator is down | No leader; nothing is triggered | The database is down, and so is everything else |
| A paused instance | Stopped by the lease margin, usually; by the conditional insert, always | Stopped by the constraint |

The second design is the one to build. It is worth noticing why: in *both* designs, the thing that guarantees *once* is a conditional write in the place where the result is recorded. The election in the first design is an optimisation. Wherever the effect lands in a store that can check a condition, start there, and add an election only if the wasted work matters.

A coordination service earns its place when there is no such store — when the leader's work is to *be* something, a primary or a router or the owner of a shard, and not to write rows.

</details>

---

## Production tips and common mistakes

**Tips**

- **Recognise a single decision when you need one**: a leader, a lock, a unique claim, an order.
- **Do not write a consensus algorithm.** Use etcd, ZooKeeper, Consul, or a database built on one.
- **Run three or five members**, on separate machines in separate failure zones, with fast disks.
- **Keep coordination data small and slow-changing.**
- **Fence everything a leader does** with the term or revision that made it leader.
- **Have a leader stop itself** when it cannot confirm its lease, before the lease expires.
- **Keep the coordination service off the request path**: cache what it says, and watch for changes.
- **Check whether a constraint in your database already makes the decision.**
- **Test the split, not only the crash.**

**Common mistakes**

- **Home-made leader election** by heartbeat and timeout.
- **Testing failover only by killing the leader.**
- **An even number of members.**
- **Assuming that there is only one leader at any instant**, rather than only one that can commit.
- **A leader that acts on its belief without a token.**
- **Using a coordination service as a database, a queue or a session store.**
- **Reading from a possibly stale leader** and calling the result consistent.
- **Putting consensus where idempotence, or a single owner per key, would have done.**

---

## Key takeaways

- Consensus is several machines making one decision that survives failures and is never undone. Leader election, locks, unique claims and ordered logs are all instances of it.
- It rests on majorities: two majorities always overlap, so two conflicting decisions cannot both be made. The smaller side of a split cannot decide, and waits.
- Raft elects one leader per numbered term, by majority vote, and treats a write as safe only when a majority has stored it. A stale leader may still believe it leads; it cannot confirm anything, and steps down when it sees a higher term.
- Consensus costs a majority round trip per write and gives no service without a majority. It is for a small number of critical decisions.
- Use a coordination service rather than an algorithm of your own — and first check whether a conditional write in a database you already run makes the decision for you.

**Next:** [5.7 Distributed transactions](07-distributed-transactions.md) — when one change must happen in two places, or in neither.

---

## Sources

- Diego Ongaro and John Ousterhout, [*In Search of an Understandable Consensus Algorithm (Extended Version)*](https://raft.github.io/raft.pdf), 2014.
- etcd documentation: [*Frequently Asked Questions*](https://etcd.io/docs/v3.5/faq/).
- Kubernetes documentation: [*Leases*](https://kubernetes.io/docs/concepts/architecture/leases/).
