# Backend System Design: From CRUD to Production

A self-paced course that starts where a new graduate is — you have built CRUD apps and done an internship — and builds, one step at a time, to what a senior engineer needs in order to design and run a production backend.

It does not skip steps and it does not hand-wave. Each chapter starts from a concrete production failure, explains the idea that prevents it, shows it working in runnable code or in a named company's published design, and makes you practise it.

**Start here:** [Chapter 1.1 — What production demands](phase-1-foundations/01-what-production-demands.md) · **Full plan:** [SYLLABUS.md](SYLLABUS.md)

---

## Who this is for

You can build an application that creates, reads, updates and deletes rows in a database, and you can write basic SQL. You have probably never had to care how many requests per second it handles, what happens when the network drops a response, or how to change a schema while the system is serving traffic.

By the end you should be able to take a product requirement and produce a design you can defend — with numbers, with the failure modes named, and with the trade-offs stated — and then operate what you designed.

## The course at a glance

| Phase | Theme | You come out able to… | Status |
|---|---|---|---|
| 1 | [Foundations: one server, one request](phase-1-foundations/README.md) | Follow a request end to end, design an API that survives retries, and say with numbers what one server can do. | **Written** |
| 2 | [Data: correct and fast on one node](phase-2-data/README.md) | Make queries fast, pick isolation levels on purpose, migrate schemas live, restore from backup. | **Written** |
| 3 | [Scaling out: load balancing, caching, read scaling](phase-3-scaling-out/README.md) | Go from one server to many and put the right cache in the right place. | **Written** |
| 4 | [Asynchronous systems: queues, streams, events](phase-4-async/README.md) | Move work off the request path without losing or duplicating it. | **Written** |
| 5 | [Distributed data: replication, partitioning, consistency](phase-5-distributed-data/README.md) | Say precisely what a replicated or sharded store guarantees. | **Written** |
| 6 | [Reliability: designing for failure](phase-6-reliability/README.md) | Stop one failure from becoming an outage. | **Written** |
| 7 | [Observability and operations](phase-7-observability/README.md) | See what the system is doing, alert on what matters, run an incident. | **Written** |
| 8 | [Security](phase-8-security/README.md) | Find the trust boundaries and get authentication, authorization and data protection right. | **Written** |
| 9 | [Shipping and running: deployment, architecture, cost](phase-9-shipping/README.md) | Ship without downtime, choose service boundaries, plan capacity, explain the bill. | In progress |

Nine phases, 65 chapters, nine capstones. The chapter-by-chapter plan, with the production failure each chapter exists to prevent, is in [SYLLABUS.md](SYLLABUS.md).

## How every chapter is built

Every chapter has the same parts, in the same order, so you always know where you are:

1. **The problem** — a concrete failure or bottleneck that makes the idea necessary.
2. **The concept** — in plain language, with a diagram where a diagram helps.
3. **A real example** — runnable Go code, or how a named company solved it, with a link to the source.
4. **Trade-offs** — when to use it, when not to, and what it costs.
5. **Practice** — two or three questions to answer *before reading on*, then a hands-on exercise or a small design problem with a worked solution.
6. **Production tips and common mistakes.**
7. **Key takeaways** and **sources.**

Each phase ends with a **capstone**: one real system designed end to end using everything covered so far.

## How to study

- **Go in order.** Later chapters assume earlier ones. Phase 1 is short on purpose; do not skip it because it looks familiar.
- **Answer the questions before you open the answers.** They are collapsed for a reason. Trying to recall an answer, even wrongly, is what makes the material stick; reading the answer feels like learning and mostly is not.
- **Run the code and change it.** Every program prints numbers. Predict them first, then change a constant and predict again.
- **Do the capstone on paper before reading the worked design.** The gap between your design and the worked one is the most useful thing in the phase.

## Running the code

The examples are written in **Go** and use only the standard library unless a chapter says otherwise. You need Go 1.22 or newer. From Phase 2 on, many examples are SQL scripts and Go programs that run against PostgreSQL; the [Phase 2 overview](phase-2-data/README.md) shows how to start one with a single command.

```bash
git clone https://github.com/sidsaurav/Sys-design.git
cd Sys-design
go run ./phase-1-foundations/code/ch01-tail
```

Each chapter gives the exact command for its example and shows the output you should expect. To check that everything builds:

```bash
go vet ./... && go build ./...
```

Why Go: every example fits in one short file, runs with one command, and the language's built-in HTTP server, timeouts and concurrency make the systems ideas visible without a framework in the way. You do not need to know Go already — the examples are commented, and none of the ideas are Go-specific.

## How claims are sourced

- Technical claims are based on primary or standard sources — RFCs, official documentation, original papers, company engineering blogs, standard textbooks — and **the link is placed next to the claim**, not in a distant bibliography.
- Text in quotation marks is quoted from the linked source.
- Scenarios used to motivate a chapter are labelled *illustrative* when they are not a documented real incident.
- Numbers printed by the example programs come from actually running them. Timing-based output varies between machines; the chapters say so where it matters.
- Where something could not be verified, the chapter says so instead of guessing.

Two scripts in [`tools/`](tools) keep this honest. `check_links.py` confirms that every link resolves. `check_quotes.py` downloads each linked page and confirms that the words quoted from it are really there. A few sources cannot be checked automatically — pages that refuse automated clients, and PDFs set in two columns — and those were checked by hand.

If you find a dead link or a claim that the linked source does not support, please open an issue.

## Repository layout

```text
README.md                 this file
SYLLABUS.md               every phase and chapter, in learning order
go.mod                    one Go module for all example code
phase-1-foundations/
  README.md               phase overview
  01-…md … 07-…md         chapters
  capstone-….md           the phase capstone
  code/                   runnable examples, one directory per program
phase-2-data/             the same layout: nine chapters, a capstone, and code/
                          (Go programs, SQL scripts and two shell scripts)
phase-3-scaling-out/      the same layout
phase-4-async/            the same layout
phase-5-distributed-data/ the same layout
phase-6-reliability/       the same layout
phase-7-observability/     the same layout
phase-8-security/          the same layout
phase-9-shipping/          the same layout; in progress
tools/
  check_links.py          checks that every link in every chapter resolves
  check_quotes.py         checks that quoted text appears in the page it cites
```
