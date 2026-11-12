# Phase 9 — Shipping and running: deployment, architecture, and cost

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Security](../phase-8-security/README.md)

Eight phases have produced a design that is correct, fast enough, reliable, observable and defended. This phase is about the rest of its life. A system that is in use is changed every day, and most outages begin with a change: a deploy, a migration, a line of configuration.

So the phase starts with shipping: how a new version replaces an old one without dropping a request, how it is shown to a few users before all of them, how old and new code run side by side, and how configuration is changed as carefully as code. Then it steps back to the shape of the system: how many services, how they find and call each other, how much of each to run, and what all of it costs. It ends with the skill that every earlier chapter has been practising: writing a design down so that other people can find what is wrong with it.

**After this phase you can** ship changes to a running system without downtime, decide how to split (or not split) a system into services, plan capacity, and explain what a design costs.

> **Status: in progress.** Chapters 9.1 to 9.5 are written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-9--shipping-and-running-deployment-architecture-and-cost).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 9.1 | [Containers and orchestration](01-containers-and-orchestration.md) | Every deploy drops requests because the process is killed mid-flight. | [`ch01-deploy`](code/ch01-deploy/main.go) — a very small orchestrator: three copies of a server, as real processes, are replaced one at a time under steady traffic, four ways, and the requests that each way drops are counted. |
| 9.2 | [CI/CD and progressive delivery](02-cicd-and-progressive-delivery.md) | A bad build reaches 100% of users in four minutes. | [`ch02-release`](code/ch02-release/main.go) — a model of one bad build released by a rolling deploy, by blue-green and by a canary; four hundred releases of four builds past four automatic judges, to see what each catches and how often it is wrong; and a feature flag opened in steps, by coin and by hash. |
| 9.3 | [Backward compatibility across deploys](03-backward-compatibility.md) | Version N+1 writes data that version N cannot read, mid-rollout. | [`ch03-compat`](code/ch03-compat/main.go) — a field is renamed while ten copies are replaced under traffic: in one deploy, and as expand, migrate and contract, with wrong reads, destroyed amounts and the cost of turning back counted; and a new value for an old field, writers first and readers first. |
| 9.4 | [Configuration and infrastructure as code](04-configuration-and-infrastructure.md) | A one-line config change causes a global outage. | [`ch04-config`](code/ch04-config/main.go) — three mistakes in a configuration file, delivered to sixty servers three ways, round a real loader that checks a file and keeps the last good one; and a small plan-and-apply engine that shows an empty plan, drift, and a change that it refuses. |
| 9.5 | [Service architecture](05-service-architecture.md) | A monolith is split into thirty services that must all deploy together. | [`ch05-boundaries`](code/ch05-boundaries/main.go) — one operation run as function calls and as chains of services over real HTTP, with the time and the failures of each arrangement; and twenty-four changes to a product counted against three ways of cutting it into services, to see how many could ship alone. |
| 9.6 | Service-to-service communication | Hard-coded hostnames and a retry policy copied into forty codebases. | *planned* |
| 9.7 | Capacity planning and autoscaling | Autoscaling is "on" and the site still falls over at the daily peak. | *planned* |
| 9.8 | Cost | The bill triples and nobody can say which feature did it. | *planned* |
| 9.9 | The design process | A design is approved because nobody could follow it. | *planned* |
| C9 | Final capstone: a ride-hailing backend, end to end | — | *planned* |

## What you need

**Go 1.22 or newer.** No container runtime and no cluster: the examples build each mechanism in small, as ordinary processes on your machine, so that it can be read and changed.

```bash
go run ./phase-9-shipping/code/ch01-deploy
```

The example of Chapter 9.1 stops its own child processes with a signal, so it runs on Linux and macOS and not on Windows.
