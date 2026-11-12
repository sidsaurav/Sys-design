# Phase 7 — Observability and operations

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Reliability](../phase-6-reliability/README.md)

Phase 6 kept asking for things to be measured: the work that somebody was still waiting for, the age of the oldest unsettled payment, how full the servers would be with a zone gone. It asked for alerts on some of them, and for people to practise what they would do. This phase is about all of that.

Its subject is what a team knows about its own system while it is running, and what the team does with the knowledge. What to collect, and what collecting costs. How to turn *is it working?* into a number that can be argued about. When to wake someone, and when not to. How to find out where one slow request spent its time. How to investigate without guessing. And what to do, as a group of people, when it is on fire.

**After this phase you can** instrument a service so that its failures are visible, set objectives that match what users feel, get paged only for things that matter, and run an incident.

> **Status: in progress.** Chapter 7.1 is written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-7--observability-and-operations).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 7.1 | [Telemetry: metrics, logs, and traces](01-telemetry.md) | The dashboard is green and users are complaining. | [`ch01-telemetry`](code/ch01-telemetry/main.go) — a real HTTP service with one fault, seen through totals, through a labelled metric and through structured logs; what a label costs; and what sampling keeps and loses |
| 7.2 | SLIs, SLOs, and error budgets in practice | "Five nines" on a slide, no measurement behind it. | *planned* |
| 7.3 | Alerting | Two hundred alerts a week, all ignored — including the real one. | *planned* |
| 7.4 | Distributed tracing and context propagation | A slow request crosses nine services and nobody knows which one was slow. | *planned* |
| 7.5 | Debugging production | Guessing, restarting, and hoping. | *planned* |
| 7.6 | Incidents | An outage with twelve people in a call and nobody in charge. | *planned* |
| C7 | Capstone: make a service operable | — | *planned* |

## What you need

**Go 1.22 or newer.** Chapter 7.1's example starts an HTTP server inside its own process and needs nothing else:

```bash
go run ./phase-7-observability/code/ch01-telemetry
```

Later chapters say at the top what they need.
