# Phase 7 — Observability and operations

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Reliability](../phase-6-reliability/README.md)

Phase 6 kept asking for things to be measured: the work that somebody was still waiting for, the age of the oldest unsettled payment, how full the servers would be with a zone gone. It asked for alerts on some of them, and for people to practise what they would do. This phase is about all of that.

Its subject is what a team knows about its own system while it is running, and what the team does with the knowledge. What to collect, and what collecting costs. How to turn *is it working?* into a number that can be argued about. When to wake someone, and when not to. How to find out where one slow request spent its time. How to investigate without guessing. And what to do, as a group of people, when it is on fire.

**After this phase you can** instrument a service so that its failures are visible, set objectives that match what users feel, get paged only for things that matter, and run an incident.

> **Status: in progress.** Chapters 7.1 to 7.6 are written. The capstone is planned; see the [syllabus](../SYLLABUS.md#phase-7--observability-and-operations).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 7.1 | [Telemetry: metrics, logs, and traces](01-telemetry.md) | The dashboard is green and users are complaining. | [`ch01-telemetry`](code/ch01-telemetry/main.go) — a real HTTP service with one fault, seen through totals, through a labelled metric and through structured logs; what a label costs; and what sampling keeps and loses |
| 7.2 | [SLIs, SLOs, and error budgets in practice](02-slis-slos-error-budgets.md) | "Five nines" on a slide, no measurement behind it. | [`ch02-slo`](code/ch02-slo/main.go) — a model of eight weeks of a service: one month counted from four places, an error budget spent incident by incident, a rolling window against a fixed one, and the same month against six targets. |
| 7.3 | [Alerting](03-alerting.md) | Two hundred alerts a week, all ignored — including the real one. | [`ch03-alerts`](code/ch03-alerts/main.go) — the same eight weeks watched by seven alert rules: which incidents each one reported, how soon, how often it fired for nothing, and what the page cost in budget. |
| 7.4 | [Distributed tracing and context propagation](04-distributed-tracing.md) | A slow request crosses nine services and nobody knows which one was slow. | [`ch04-tracing`](code/ch04-tracing/main.go) — nine real HTTP services and a tracer of a hundred lines: nine dashboards against one trace, a service that drops the header, and two ways to sample. [`ch04-otel`](code/ch04-otel/main.go) — one traced call with OpenTelemetry. |
| 7.5 | [Debugging production](05-debugging-production.md) | Guessing, restarting, and hoping. | [`ch05-debug`](code/ch05-debug/main.go) — a service process with one of three faults, and an engineer process that questions it over HTTP: the dashboard, a CPU profile read with `go tool pprof`, a goroutine dump, and a leak watched through a restart. |
| 7.6 | [Incidents](06-incidents.md) | An outage with twelve people in a call and nobody in charge. | [`ch06-oncall`](code/ch06-oncall/main.go) — a calculator for a rota: the share of the year each person is on call, what a rate of incidents costs in hours, and the two pagers of Chapter 7.3 in broken nights. The chapter's main example is Cloudflare's account of its outage of 2 July 2019. |
| C7 | Capstone: make a service operable | — | *planned* |

## What you need

**Go 1.22 or newer.** The examples start their HTTP servers inside their own process, or are models, and need nothing else:

```bash
go run ./phase-7-observability/code/ch01-telemetry
```

One example is different. Chapter 7.4's second program uses OpenTelemetry, and is a Go module of its own so that the rest of the course does not depend on it. It needs Go 1.23 or newer, and the network the first time, to fetch its modules:

```bash
cd phase-7-observability/code/ch04-otel && go run .
```

Later chapters say at the top what they need.
