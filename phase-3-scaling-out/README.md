# Phase 3 — Scaling out: load balancing, caching, and read scaling

[Course home](../README.md) · [Syllabus](../SYLLABUS.md) · Previous phase: [Data](../phase-2-data/README.md)

Phases 1 and 2 were about one server and one database, and ended — twice — with a list of what breaks first. This phase works through the top of that list: running the application as many interchangeable copies, spreading requests across them, keeping the same work from being done again and again, and taking read traffic off the primary database.

Every technique here trades something for capacity, and what it trades is usually *freshness*: a cache, a CDN or a replica serves an answer that was true a moment ago. The skill this phase builds is knowing, for each copy of the data, how stale it may be and what a user sees when it is.

**After this phase you can** take a service from one server to many, put the right cache in the right place, and reason about what a user sees when the cache or a replica is stale.

> **Status: in progress.** Chapters 3.1 to 3.4 are written. The rest are planned; see the [syllabus](../SYLLABUS.md#phase-3--scaling-out-load-balancing-caching-and-read-scaling).

## Chapters

| # | Chapter | The failure it prevents | Runnable example |
|---|---|---|---|
| 3.1 | [Scaling up vs scaling out: stateless services](01-stateless-services.md) | A second server is added and half the users are logged out on every other request. | [`ch01-stateless`](code/ch01-stateless/main.go) — one application, two instances, four places to keep its state |
| 3.2 | [Load balancing](02-load-balancing.md) | A dead server keeps receiving a third of the traffic; a deploy drops in-flight requests. | [`ch02-loadbalancer`](code/ch02-loadbalancer/main.go) — a slow instance, a hung one, a deploy, and long-lived connections; [`ch02-graceful`](code/ch02-graceful/main.go) — a server that stops without dropping a request |
| 3.3 | [Caching patterns](03-caching-patterns.md) | The database is at 95% CPU serving the same 1,000 rows. | [`ch03-cache`](code/ch03-cache/main.go) — what a cache saves, how stale readers get, and two races step by step |
| 3.4 | [Cache failure modes](04-cache-failure-modes.md) | The cache restarts and the database dies under the stampede. | [`ch04-stampede`](code/ch04-stampede/main.go) — an expiring hot entry, an emptied cache, missing keys, and cache size |
| 3.5 | HTTP caching and CDNs | Users in another continent wait two seconds for a logo; a "private" page is served to the wrong user. | *planned* |
| 3.6 | Read replicas and replication lag | A user saves their profile, the page reloads, and the old value is back. | *planned* |
| 3.7 | Long-lived connections: WebSockets, SSE, and long polling | A feature that needs push updates is built on a one-second polling loop. | *planned* |
| C3 | Capstone: the read path of a photo-sharing app | — | *planned* |

## What you need

**Go 1.22 or newer.** The examples in this phase are Go programs that start everything they need — several instances of an application, a load balancer, a store — inside one process, on local ports, so that each runs with one command:

```bash
go run ./phase-3-scaling-out/code/ch01-stateless
```

Chapters that need more than that say so at the top.
