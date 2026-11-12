# Phase 8 — Security

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Observability and operations](../phase-7-observability/README.md)

Every phase so far has assumed that whoever reached the service meant well. Failures were accidents: a slow bank, a lost zone, a careless release. This phase drops the assumption. Some of the people who call an API are looking for the request that it should have refused.

Its subject is trust: who is asking, how the service knows, what they are allowed, and what happens when a check is missing. It starts with the design, because most security faults are decisions about trust that nobody noticed making. Then it takes the checks one at a time: who you are, what you may do, how data is protected on the way and at rest, the faults in code that attackers actually use, and what to do about abuse and about your own team's access.

**After this phase you can** find the trust boundaries in a design, get authentication and authorization right, protect data in transit and at rest, and review an API for the vulnerabilities that actually get exploited.

> **Status: in progress.** Chapter 8.1 is written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-8--security).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 8.1 | [Threat modelling and trust boundaries](01-threat-modelling.md) | Security is "added" after the design is finished. | [`ch01-boundaries`](code/ch01-boundaries/main.go) — a gateway, a service and a store as real HTTP servers, five controls, and four attackers: which attacks succeed when each control fails, and how many controls must fail for each. |
| 8.2 | Authentication | Passwords stored with a fast hash; tokens that cannot be revoked. | *planned* |
| 8.3 | Authorization | Changing an ID in the URL shows another customer's invoice. | *planned* |
| 8.4 | Protecting data | A database snapshot is copied to a public bucket. | *planned* |
| 8.5 | Backend vulnerabilities | A "fetch this URL" feature reads the cloud metadata service. | *planned* |
| 8.6 | Abuse and operational security | A leaked CI token with admin rights. | *planned* |
| C8 | Capstone: security review of a multi-tenant SaaS API | — | *planned* |

## What you need

**Go 1.22 or newer.** The examples start their servers inside their own process and need nothing else:

```bash
go run ./phase-8-security/code/ch01-boundaries
```

The examples include attacks. Each attacks only the servers that the same program starts, on your own machine. Do not point them at anything else.
