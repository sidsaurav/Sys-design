# Phase 4 — Asynchronous systems: queues, streams, and events

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Scaling out](../phase-3-scaling-out/README.md)

Everything so far happened inside a request: a user asked, the system did the work, the user was answered. This phase is about work that happens *after* the answer — a welcome email, a resized picture, a message to another system — and about the machinery that holds such work between the moment it is accepted and the moment it is done.

That machinery buys a great deal: faster answers, independence from other systems' outages, and a load that arrives in bursts and is worked off steadily. It also changes what can go wrong. Work can be lost, done twice, done in the wrong order, or done so late that it no longer matters — and none of those raises an error by itself.

**After this phase you can** move work off the request path without losing or duplicating it, choose between a queue and a log, and keep a database and a message stream consistent with each other.

> **Status: in progress.** Chapters 4.1 to 4.7 are written. The capstone is planned; see the [syllabus](../SYLLABUS.md#phase-4--asynchronous-systems-queues-streams-and-events).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 4.1 | [Why go asynchronous](01-why-asynchronous.md) | Sign-up takes eight seconds because it sends three emails and resizes an avatar inline. | [`ch01-background`](code/ch01-background/main.go) — one sign-up done inline and done later: a provider outage, a burst against a rate limit, and a restart that loses the queue |
| 4.2 | [Message queues](02-message-queues.md) | A worker crashes mid-job and the job is gone — or runs forever. | [`ch02-queue`](code/ch02-queue/main.go) — a job queue in one PostgreSQL table: workers that die, a time limit that is too short, a job that can never succeed, and jobs that must be done in order |
| 4.3 | [Delivery semantics and idempotent consumers](03-delivery-semantics.md) | "Exactly once" in the brochure, two welcome emails in the inbox. | [`ch03-idempotent`](code/ch03-idempotent/main.go) — 1,000 messages delivered 1,213 times to five consumers; which ID to recognise duplicates by; an email that cannot join a transaction |
| 4.4 | [Logs and streams: Kafka's model](04-logs-and-streams.md) | A queue is used where replay and multiple independent readers were needed. | [`ch04-log`](code/ch04-log/main.go) — a model of a partitioned log: independent readers and replay, keys and order, committing offsets, retention and compaction |
| 4.5 | [The dual-write problem: outbox and change data capture](05-dual-writes-outbox-cdc.md) | The order is saved but the "order placed" event never arrives. | [`ch05-outbox`](code/ch05-outbox/main.go) — 1,000 orders saved and announced three ways, with failures between the two writes; then the changes read from PostgreSQL's own log |
| 4.6 | [Operating queues](06-operating-queues.md) | The queue grows for six hours before anyone notices; one bad message blocks a partition. | [`ch06-backlog`](code/ch06-backlog/main.go) — the arithmetic of a backlog, stepped through: a burst, four ways to recover, a limit, a poison record, and urgent work behind bulk |
| 4.7 | [Event-driven design](07-event-driven-design.md) | Forty services coupled through events nobody can change. | [`ch07-events`](code/ch07-events/main.go) — a renamed field that zeroes a revenue total, a declined payment under choreography and orchestration, and counting per minute when events arrive late |
| C4 | Capstone: a notification platform | — | *planned* |

## What you need

**Go 1.22 or newer.** Chapter 4.1's example runs with one command and nothing else:

```bash
go run ./phase-4-async/code/ch01-background
```

Chapters 4.2, 4.3 and 4.5 use PostgreSQL, so they need the database of Phase 2 ([setup](../phase-2-data/README.md)). Later chapters say at the top what else they need.
