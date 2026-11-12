# Phase 6 — Reliability: designing for failure

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Distributed data](../phase-5-distributed-data/README.md)

Every phase so far has dealt with failures one at a time, as each topic met them: a request that got no answer, a cache that restarted, a worker that died half-way, a leader that was lost. In each case something failed, and the question was what to do about it.

This phase is about failures that the system makes for itself. A dependency slows down, and the services that call it stop answering too. A server fails, and the others follow it. A fault lasts thirty seconds, and the outage four hours. The fault is seldom the interesting part. What matters is what the rest of the system did next — and that can be designed.

**After this phase you can** predict how a system fails before it does, stop one slow dependency from taking everything down, and plan for the loss of a zone or a region.

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 6.1 | [How distributed systems fail](01-how-distributed-systems-fail.md) | A 30-second blip becomes a four-hour outage. | [`ch01-failures`](code/ch01-failures/main.go) — a model: a slow server that passes its health check, the loss of one server in five at three loads, and a 30-second stall with and without retries |
| 6.2 | [Timeouts and retries at scale](02-timeouts-and-retries-at-scale.md) | Every layer retries three times and the database sees 27× the load. | [`ch02-deadlines`](code/ch02-deadlines/main.go) — three services over real HTTP: who retries, and what a deadline passed down saves; then two models: the stall of 6.1 with backoff, a retry budget and deadlines, and a page that needs 100 servers, with and without hedging |
| 6.3 | [Circuit breakers, bulkheads, and graceful degradation](03-circuit-breakers-bulkheads-degradation.md) | The recommendations service is slow, so checkout is down. | [`ch03-breaker`](code/ch03-breaker/main.go) — three real HTTP services: a checkout page whose optional dependency becomes slow, with nothing, a timeout, a bulkhead and a circuit breaker |
| 6.4 | [Overload: rate limiting, load shedding, and admission control](04-overload.md) | The service accepts every request and completes none. | [`ch04-overload`](code/ch04-overload/main.go) — a model of a service sent more than it can do: queueing, dropping and refusing; a limit for each client; shedding by priority; a rate limit against a limit on requests in progress; and that limit as Go middleware |
| 6.5 | [Redundancy and failure domains](05-redundancy-and-failure-domains.md) | Three replicas, one rack, one power failure. | [`ch05-domains`](code/ch05-domains/main.go) — a model: three copies in one rack, three racks and three zones over fifty simulated years; the capacity to lose a zone, with and without having to ask for it; and how far one customer's fault reaches with cells and with shuffle shards |
| 6.6 | [Multi-region architectures](06-multi-region.md) | The failover runbook is tried for the first time during the outage. | [`ch06-regions`](code/ch06-regions/main.go) — a model: what a failover to another region loses and what losing nothing costs; how long users take to move, by DNS and by a balancer, and what is waiting for them; and the stranded writes of a failback |
| 6.7 | [Disaster recovery and resilience testing](07-disaster-recovery-and-resilience-testing.md) | A recovery plan that exists only as a document. | [`ch07-gameday`](code/ch07-gameday/main.go) — a game day over real HTTP: four fault-injection experiments on a small shop, each with a hypothesis and a guard; two are disproved, the shop is changed, and they are run again |
| C6 | [Capstone: a payments API that survives dependency and zone failures](capstone-payments-api.md) | — | [`capstone-payments`](code/capstone-payments/main.go) — six instances in three zones over real HTTP, a PostgreSQL ledger and a stand-in bank that is slow, refuses and loses answers; a zone lost and a sale; then what shops were told is checked against what the bank charged. Each safeguard can be switched off |

## What you need

**Go 1.22 or newer.** The chapters' examples are models, or start HTTP servers inside their own process, and need nothing else. Each runs with one command:

```bash
go run ./phase-6-reliability/code/ch01-failures
```

The capstone's reference implementation needs the PostgreSQL database of Phase 2 ([setup](../phase-2-data/README.md)).
