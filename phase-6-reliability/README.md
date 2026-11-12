# Phase 6 — Reliability: designing for failure

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Distributed data](../phase-5-distributed-data/README.md)

Every phase so far has dealt with failures one at a time, as each topic met them: a request that got no answer, a cache that restarted, a worker that died half-way, a leader that was lost. In each case something failed, and the question was what to do about it.

This phase is about failures that the system makes for itself. A dependency slows down, and the services that call it stop answering too. A server fails, and the others follow it. A fault lasts thirty seconds, and the outage four hours. The fault is seldom the interesting part. What matters is what the rest of the system did next — and that can be designed.

**After this phase you can** predict how a system fails before it does, stop one slow dependency from taking everything down, and plan for the loss of a zone or a region.

> **Status: in progress.** Chapter 6.1 is written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-6--reliability-designing-for-failure).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 6.1 | [How distributed systems fail](01-how-distributed-systems-fail.md) | A 30-second blip becomes a four-hour outage. | [`ch01-failures`](code/ch01-failures/main.go) — a model: a slow server that passes its health check, the loss of one server in five at three loads, and a 30-second stall with and without retries |
| 6.2 | Timeouts and retries at scale | Every layer retries three times and the database sees 27× the load. | *planned* |
| 6.3 | Circuit breakers, bulkheads, and graceful degradation | The recommendations service is slow, so checkout is down. | *planned* |
| 6.4 | Overload: rate limiting, load shedding, and admission control | The service accepts every request and completes none. | *planned* |
| 6.5 | Redundancy and failure domains | Three replicas, one rack, one power failure. | *planned* |
| 6.6 | Multi-region architectures | The failover runbook is tried for the first time during the outage. | *planned* |
| 6.7 | Disaster recovery and resilience testing | A recovery plan that exists only as a document. | *planned* |
| C6 | Capstone: a payments API that survives dependency and zone failures | — | *planned* |

## What you need

**Go 1.22 or newer.** Chapter 6.1's example is a model with no network and no database, and runs with one command:

```bash
go run ./phase-6-reliability/code/ch01-failures
```

Later chapters say at the top what they need.
