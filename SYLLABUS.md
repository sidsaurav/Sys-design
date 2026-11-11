# Syllabus

Nine phases, 65 chapters, nine capstones. The order is the order in which the problems show up in a real backend as it grows: first one server, then its data, then more servers, then work that happens off the request path, then data on many machines, then failure, then visibility, then attackers, then shipping and paying for all of it.

Each phase assumes every phase before it. Each chapter is sized for one to three sittings (roughly 45–60 minutes of reading and 30–90 minutes of practice).

Every chapter is anchored on **one production failure** — the thing that goes wrong if you don't know the material. That failure is listed below next to each chapter so you can see why the chapter exists before you read it.

| Phase | Theme | Chapters | Status |
|---|---|---|---|
| 1 | [Foundations: one server, one request](#phase-1--foundations-one-server-one-request) | 7 + capstone | **Written** |
| 2 | [Data: correct and fast on one node](#phase-2--data-correct-and-fast-on-one-node) | 9 + capstone | Planned |
| 3 | [Scaling out: load balancing, caching, read scaling](#phase-3--scaling-out-load-balancing-caching-and-read-scaling) | 7 + capstone | Planned |
| 4 | [Asynchronous systems: queues, streams, events](#phase-4--asynchronous-systems-queues-streams-and-events) | 7 + capstone | Planned |
| 5 | [Distributed data: replication, partitioning, consistency](#phase-5--distributed-data-replication-partitioning-and-consistency) | 7 + capstone | Planned |
| 6 | [Reliability: designing for failure](#phase-6--reliability-designing-for-failure) | 7 + capstone | Planned |
| 7 | [Observability and operations](#phase-7--observability-and-operations) | 6 + capstone | Planned |
| 8 | [Security](#phase-8--security) | 6 + capstone | Planned |
| 9 | [Shipping and running: deployment, architecture, cost](#phase-9--shipping-and-running-deployment-architecture-and-cost) | 9 + final capstone | Planned |

---

## Phase 1 — Foundations: one server, one request

**After this phase you can** follow a request from the client's keyboard to your handler and back, say where each millisecond goes, design an HTTP API that stays correct when clients retry, and state with numbers what one server can handle and what breaks first.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 1.1 | [What production demands: requirements, SLOs, and the numbers](phase-1-foundations/01-what-production-demands.md) | The app passes every test and still falls over on launch day, because nobody wrote down how much load, how fast, or how available. | Functional vs non-functional requirements; SLI, SLO, SLA; availability arithmetic; latency percentiles and why the tail matters; back-of-envelope estimation. |
| 1.2 | [The life of a request: DNS, TCP, and TLS](phase-1-foundations/02-life-of-a-request.md) | The server's logs say 20 ms; the user's phone says 470 ms. | What happens before your code runs; round trips as the unit of network cost; DNS caching and TTLs; TCP and TLS handshakes; why connection reuse is the cheapest speed-up you will ever ship. |
| 1.3 | [HTTP for backend engineers](phase-1-foundations/03-http-for-backend-engineers.md) | Proxies, caches, crawlers and client libraries do the wrong thing because the API lies about what its methods and status codes mean. | Method semantics (safe, idempotent, cacheable); status codes that carry real meaning; conditional requests and ETags; HTTP/1.1 vs HTTP/2 vs HTTP/3. |
| 1.4 | [API design: resources, errors, pagination, and versioning](phase-1-foundations/04-api-design.md) | A list endpoint that worked with 50 rows times out at 5 million; a renamed field breaks every installed mobile app. | Resource modelling; a consistent error format; offset vs cursor pagination; what counts as a breaking change; versioning strategies; limits on everything. |
| 1.5 | [Timeouts, retries, and idempotency](phase-1-foundations/05-timeouts-retries-idempotency.md) | The network drops one response and a customer is charged twice. | The three outcomes of a remote call; choosing timeouts; retrying with backoff and jitter; idempotency keys and how to implement them without races. |
| 1.6 | [Inside the server: concurrency, pools, and queues](phase-1-foundations/06-concurrency-pools-queues.md) | Latency goes from 20 ms to 10 s while CPU sits at 15%, because every request is waiting for one of ten database connections. | Threads, event loops, and goroutines; pools as concurrency limits; Little's law; why latency explodes near full utilisation; bounded queues. |
| 1.7 | [Measuring performance: percentiles, load tests, and bottlenecks](phase-1-foundations/07-measuring-performance.md) | The load test said 40 ms; production says 4 s. | Latency distributions; open vs closed load models and coordinated omission; load, stress, soak and spike tests; the USE method; profiling. |
| C1 | [Capstone: a URL shortener on one server](phase-1-foundations/capstone-url-shortener.md) | — | Requirements → estimates → API → single-server design → failure analysis → the list of what breaks first, which is the agenda for Phases 2 and 3. |

---

## Phase 2 — Data: correct and fast on one node

**After this phase you can** explain why a query is slow and fix it, pick an isolation level on purpose, prevent lost updates and double-bookings, change a schema while the system is serving traffic, and restore from backup with confidence.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 2.1 | How a database stores and finds data | You tune by superstition because the database is a black box. | Pages and the buffer cache; B-trees; the write-ahead log; LSM-trees and where they win. |
| 2.2 | Indexes and query plans | A query takes 30 ms in staging and 30 s in production. | Reading `EXPLAIN`; composite, partial and covering indexes; selectivity; the N+1 query; what indexes cost on writes. |
| 2.3 | Transactions and isolation levels | Two requests each read a balance of 100, each withdraw 80, and both succeed. | What ACID does and does not promise; read committed, repeatable read, serializable; the anomalies each allows; MVCC; deadlocks. |
| 2.4 | Concurrency control in application code | "Check then act" code that is correct in every test and wrong under load. | Lost updates; `SELECT … FOR UPDATE`; optimistic locking with version columns; unique constraints as the simplest lock; retrying serialization failures. |
| 2.5 | Data modelling: keys, constraints, and denormalisation | A schema that makes every new feature a migration project, or lets bad data in. | Primary key choices (sequences, UUIDs, time-ordered IDs); constraints as the last line of defence; normalisation and when to break it; JSON columns; soft deletes. |
| 2.6 | The database as a shared resource | One slow report query takes the whole product down. | Connection limits and poolers; long transactions; lock queues; statement and lock timeouts; separating workloads. |
| 2.7 | Changing schemas without downtime | `ALTER TABLE` takes a lock and the site is down for six minutes. | Which changes lock and for how long; expand → migrate → contract; backfills; building indexes concurrently; deploy ordering. |
| 2.8 | Choosing a datastore | A document store chosen for "flexibility" that now needs joins and transactions. | Relational, key-value, document, wide-column, search, object storage, analytical (columnar) stores: the access pattern each is built for. |
| 2.9 | Durability, backups, and restore | The backups existed; nobody had ever tried to restore one. | `fsync` and what "committed" means; logical vs physical backups; point-in-time recovery; RPO and RTO; restore drills. |
| C2 | Capstone: ticket booking — never sell the same seat twice | — | Schema, indexes, transactions and isolation for a seat-reservation system, with a zero-downtime migration and a restore plan. |

---

## Phase 3 — Scaling out: load balancing, caching, and read scaling

**After this phase you can** take a service from one server to many, put the right cache in the right place, and reason about what a user sees when the cache or a replica is stale.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 3.1 | Scaling up vs scaling out: stateless services | A second server is added and half the users are logged out on every other request. | Vertical vs horizontal scaling; where session and upload state must live; what "stateless" actually requires. |
| 3.2 | Load balancing | A dead server keeps receiving a third of the traffic; a deploy drops in-flight requests. | Layer 4 vs layer 7; balancing algorithms; health checks; connection draining; why long-lived connections unbalance. |
| 3.3 | Caching patterns | The database is at 95% CPU serving the same 1,000 rows. | Cache-aside, read-through, write-through, write-behind; TTLs; invalidation; what is safe to cache. |
| 3.4 | Cache failure modes | The cache restarts and the database dies under the stampede. | Stampedes and how to stop them; hot keys; negative caching; stale-while-revalidate; eviction policies; running Redis in production. |
| 3.5 | HTTP caching and CDNs | Users in another continent wait two seconds for a logo; a "private" page is served to the wrong user from a shared cache. | `Cache-Control`, validators, `Vary`; shared vs private caches; edge caching; purging; signed URLs. |
| 3.6 | Read replicas and replication lag | A user saves their profile, the page reloads, and the old value is back. | Scaling reads with replicas; replication lag; read-your-writes and monotonic reads; routing reads safely. |
| 3.7 | Long-lived connections: WebSockets, SSE, and long polling | A feature that needs push updates is built on a one-second polling loop. | Polling vs long polling vs server-sent events vs WebSockets; what stateful connections do to load balancing and deploys. |
| C3 | Capstone: the read path of a photo-sharing app | — | Load balancing, caches, CDN, object storage and replicas for a read-heavy product, with a staleness budget for every hop. |

---

## Phase 4 — Asynchronous systems: queues, streams, and events

**After this phase you can** move work off the request path without losing or duplicating it, choose between a queue and a log, and keep a database and a message stream consistent with each other.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 4.1 | Why go asynchronous | Sign-up takes eight seconds because it sends three emails and resizes an avatar inline. | Background jobs; decoupling; load levelling; what you give up (immediacy, simple error handling). |
| 4.2 | Message queues | A worker crashes mid-job and the job is gone — or runs forever. | Acknowledgements; visibility timeouts; redelivery; dead-letter queues; ordering guarantees and their price. |
| 4.3 | Delivery semantics and idempotent consumers | "Exactly once" in the brochure, two welcome emails in the inbox. | At-most-once vs at-least-once; why exactly-once delivery is not on offer and exactly-once *effect* is; deduplication. |
| 4.4 | Logs and streams: Kafka's model | A queue is used where replay and multiple independent readers were needed. | The append-only log; partitions and ordering; offsets and consumer groups; retention and compaction; queue vs log. |
| 4.5 | The dual-write problem: outbox and change data capture | The order is saved but the "order placed" event never arrives. | Why writing to two systems cannot be atomic; the transactional outbox; change data capture. |
| 4.6 | Operating queues | The queue grows for six hours before anyone notices; one bad message blocks a partition. | Backlog and lag as signals; backpressure; poison messages; priorities; delayed and scheduled work. |
| 4.7 | Event-driven design | Forty services coupled through events nobody can change. | Events vs commands; schemas and their evolution; choreography vs orchestration; stream processing in one sitting. |
| C4 | Capstone: a notification platform | — | Multi-channel delivery with retries, deduplication, per-provider rate limits and user preferences. |

---

## Phase 5 — Distributed data: replication, partitioning, and consistency

**After this phase you can** say precisely what a replicated or sharded store guarantees, choose a partition key, and recognise the designs that need consensus and the ones that only think they do.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 5.1 | Leader–follower replication | A failover promotes a stale follower and an hour of writes disappears. | Synchronous vs asynchronous replication; failover; split brain; what is lost and when. |
| 5.2 | Multi-leader and leaderless replication | Two regions accept conflicting writes and one silently wins. | Quorums; conflict detection and resolution; last-write-wins and what it destroys. |
| 5.3 | Partitioning (sharding) | One shard holds the celebrity account and melts while the others idle. | Range vs hash partitioning; consistent hashing; hot partitions; rebalancing; cross-shard queries and secondary indexes. |
| 5.4 | Consistency models and CAP | "Eventually consistent" is accepted in a design review and nobody can say what users will see. | Linearizability, causal and eventual consistency; what CAP does and does not say; PACELC. |
| 5.5 | Time and order | A lock "held" by a paused process corrupts data. | Clock skew; logical clocks; leases; fencing tokens; why distributed locks are harder than they look. |
| 5.6 | Consensus and coordination | A home-made leader election elects two leaders. | What Raft provides; leader election and configuration stores; when to use etcd or ZooKeeper and when not to. |
| 5.7 | Distributed transactions | Money leaves one service and never arrives in the other. | Two-phase commit and its failure modes; sagas and compensation; designing so you need neither. |
| C5 | Capstone: a chat system | — | Message ordering, delivery guarantees, storage partitioning and presence. |

---

## Phase 6 — Reliability: designing for failure

**After this phase you can** predict how a system fails before it does, stop one slow dependency from taking everything down, and plan for the loss of a zone or a region.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 6.1 | How distributed systems fail | A 30-second blip becomes a four-hour outage. | Partial and grey failure; cascading failure; retry storms; metastable states. |
| 6.2 | Timeouts and retries at scale | Every layer retries three times and the database sees 27× the load. | Deadline propagation; retry budgets; hedged requests; where in the stack to retry. |
| 6.3 | Circuit breakers, bulkheads, and graceful degradation | The recommendations service is slow, so checkout is down. | Failing fast; isolating resources per dependency; serving a degraded answer on purpose. |
| 6.4 | Overload: rate limiting, load shedding, and admission control | The service accepts every request and completes none. | Token bucket and friends; per-client limits; shedding by priority; concurrency limits. |
| 6.5 | Redundancy and failure domains | Three replicas, one rack, one power failure. | Zones and regions; correlated failure; static stability; cells and blast radius. |
| 6.6 | Multi-region architectures | The failover runbook is tried for the first time during the outage. | Active–passive vs active–active; what each costs in data consistency; failing over and failing back. |
| 6.7 | Disaster recovery and resilience testing | A recovery plan that exists only as a document. | RPO and RTO as design inputs; restore drills; fault injection and game days. |
| C6 | Capstone: a payments API that survives dependency and zone failures | — | Timeouts, idempotency, breakers, shedding and redundancy applied to one money-moving service. |

---

## Phase 7 — Observability and operations

**After this phase you can** instrument a service so that its failures are visible, set objectives that match what users feel, get paged only for things that matter, and run an incident.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 7.1 | Telemetry: metrics, logs, and traces | The dashboard is green and users are complaining. | What each signal answers; structured logs; cardinality; sampling. |
| 7.2 | SLIs, SLOs, and error budgets in practice | "Five nines" on a slide, no measurement behind it. | Choosing indicators; windows; turning an objective into an error budget and a release policy. |
| 7.3 | Alerting | Two hundred alerts a week, all ignored — including the real one. | Paging on symptoms; burn-rate alerts; alert fatigue; what belongs on a dashboard instead. |
| 7.4 | Distributed tracing and context propagation | A slow request crosses nine services and nobody knows which one was slow. | Spans and traces; propagation; sampling strategies; OpenTelemetry. |
| 7.5 | Debugging production | Guessing, restarting, and hoping. | A repeatable method for latency, error and saturation problems; profiling live systems. |
| 7.6 | Incidents | An outage with twelve people in a call and nobody in charge. | Roles; communication; mitigation before diagnosis; blameless postmortems; sustainable on-call. |
| C7 | Capstone: make a service operable | — | SLOs, dashboards, alerts and a runbook for an earlier capstone, then a written incident walkthrough. |

---

## Phase 8 — Security

**After this phase you can** find the trust boundaries in a design, get authentication and authorization right, protect data in transit and at rest, and review an API for the vulnerabilities that actually get exploited.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 8.1 | Threat modelling and trust boundaries | Security is "added" after the design is finished. | Assets, actors and boundaries; a lightweight threat model; defence in depth. |
| 8.2 | Authentication | Passwords stored with a fast hash; tokens that cannot be revoked. | Password storage; sessions vs tokens; JWT pitfalls; OAuth 2.0 and OpenID Connect. |
| 8.3 | Authorization | Changing an ID in the URL shows another customer's invoice. | Roles and attributes; object-level checks; tenant isolation. |
| 8.4 | Protecting data | A database snapshot is copied to a public bucket. | TLS and mutual TLS; encryption at rest; key management; secrets handling. |
| 8.5 | Backend vulnerabilities | A "fetch this URL" feature reads the cloud metadata service. | Injection; SSRF; unsafe deserialisation; mass assignment; the OWASP API Security Top 10. |
| 8.6 | Abuse and operational security | A leaked CI token with admin rights. | Abuse limits; audit logs; least privilege; dependency and supply-chain hygiene. |
| C8 | Capstone: security review of a multi-tenant SaaS API | — | A full review: threat model, authn/authz design, data protection and abuse controls. |

---

## Phase 9 — Shipping and running: deployment, architecture, and cost

**After this phase you can** ship changes to a running system without downtime, decide how to split (or not split) a system into services, plan capacity, and explain what a design costs.

| # | Chapter | The failure it prevents | What you learn |
|---|---|---|---|
| 9.1 | Containers and orchestration | Every deploy drops requests because the process is killed mid-flight. | What an orchestrator does for you; health probes; graceful shutdown; resource requests and limits. |
| 9.2 | CI/CD and progressive delivery | A bad build reaches 100% of users in four minutes. | Rolling, blue-green and canary releases; feature flags; automated rollback. |
| 9.3 | Backward compatibility across deploys | Version N+1 writes data that version N cannot read, mid-rollout. | Evolving APIs, events and schemas when old and new code run side by side. |
| 9.4 | Configuration and infrastructure as code | A one-line config change causes a global outage. | Treating config as code; staged rollout of configuration; reproducible infrastructure. |
| 9.5 | Service architecture | A monolith is split into thirty services that must all deploy together. | Monolith, modular monolith, microservices; finding service boundaries; what the split costs. |
| 9.6 | Service-to-service communication | Hard-coded hostnames and a retry policy copied into forty codebases. | Service discovery; gRPC; API gateways; what a service mesh moves out of your code. |
| 9.7 | Capacity planning and autoscaling | Autoscaling is "on" and the site still falls over at the daily peak. | Forecasting; headroom; what autoscaling can and cannot react to; load testing as a planning tool. |
| 9.8 | Cost | The bill triples and nobody can say which feature did it. | The anatomy of a cloud bill; unit economics; the price of each extra nine; cost-aware design. |
| 9.9 | The design process | A design is approved because nobody could follow it. | Writing a design doc; stating trade-offs; running and surviving a design review. |
| C9 | Final capstone: a ride-hailing backend, end to end | — | Requirements, APIs, data, scaling, async, consistency, reliability, observability, security, rollout and cost — one system, every phase. |
