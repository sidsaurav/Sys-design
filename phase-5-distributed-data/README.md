# Phase 5 — Distributed data: replication, partitioning, and consistency

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Asynchronous systems](../phase-4-async/README.md)

Four capstones have now ended with the same items on their list of *what breaks first*: the one primary that every write passes through, the table that outgrows one machine, the region that everything lives in. This phase is about those.

Its subject is data that lives on more than one machine *as the record* — not as a cache or a read copy that can be thrown away. That changes the questions. When the machine that takes writes dies, what was lost? When two machines both accept a write, which one wins, and what happens to the other? When data is split across machines, where does a given row live, and what does a query that needs two of them cost? And underneath all of those: when machines cannot agree on what time it is, or whether another machine is alive, what can be promised at all?

**After this phase you can** say precisely what a replicated or sharded store guarantees, choose a partition key, and recognise the designs that need consensus and the ones that only think they do.

> **Status: in progress.** Chapters 5.1 to 5.6 are written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-5--distributed-data-replication-partitioning-and-consistency).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 5.1 | [Leader–follower replication](01-leader-follower-replication.md) | A failover promotes a stale follower and an hour of writes disappears. | [`ch01-failover`](code/ch01-failover/main.go) — on a real primary and two replicas: what a failover loses, two primaries at once, and the same failover with synchronous replication |
| 5.2 | [Multi-leader and leaderless replication](02-multi-leader-and-leaderless.md) | Two regions accept conflicting writes and one silently wins. | [`ch02-conflicts`](code/ch02-conflicts/main.go) — a model of two regions: three stories under last-write-wins and under version vectors; and read and write quorums on three replicas |
| 5.3 | [Partitioning (sharding)](03-partitioning.md) | One shard holds the celebrity account and melts while the others idle. | [`ch03-partitions`](code/ch03-partitions/main.go) — a million keys placed three ways; what moves when a ninth shard is added; a customer who is a fifth of the traffic; a query that must ask every shard |
| 5.4 | [Consistency models and CAP](04-consistency-models-and-cap.md) | "Eventually consistent" is accepted in a design review and nobody can say what users will see. | [`ch04-consistency`](code/ch04-consistency/main.go) — a model: what a reader observes under three ways of reading, and the two choices when the network splits |
| 5.5 | [Time and order](05-time-and-order.md) | A lock "held" by a paused process corrupts data. | [`ch05-time`](code/ch05-time/main.go) — a lease, a paused client and a fencing token; a duration measured across a clock correction; a merged log ordered by wall clocks and by a counter |
| 5.6 | [Consensus and coordination](06-consensus-and-coordination.md) | A home-made leader election elects two leaders. | [`ch06-election`](code/ch06-election/main.go) — a model of five nodes and a network that splits: a home-made election against terms, votes and majorities, in one split and in 500 random histories |
| 5.7 | Distributed transactions | Money leaves one service and never arrives in the other. | *planned* |
| C5 | Capstone: a chat system | — | *planned* |

## What you need

**Go 1.22 or newer.** Chapter 5.1 runs against a real PostgreSQL primary with two replicas — the three servers that Chapter 3.6's script starts:

```bash
bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start
go run ./phase-5-distributed-data/code/ch01-failover
bash phase-3-scaling-out/code/ch06-replicas/cluster.sh stop
```

The examples of Chapters 5.2 to 5.6 need only Go. Later chapters say at the top what they need.
