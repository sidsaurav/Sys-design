# 7.1 Telemetry: metrics, logs, and traces

[Syllabus](../SYLLABUS.md) · Phase 7 · Previous: [Capstone 6](../phase-6-reliability/capstone-payments-api.md) · Next: 7.2 SLIs, SLOs, and error budgets in practice *(planned)*

> **Time:** about 30 minutes to read, 40 minutes of practice.
> **You need:** the four golden signals, percentiles and histograms from [Chapter 1.7](../phase-1-foundations/07-measuring-performance.md); grey failure from [Chapter 6.1](../phase-6-reliability/01-how-distributed-systems-fail.md). The example needs only Go, and takes about 12 seconds to run.

**By the end of this chapter you can**

- say what question each of the three signals answers, and what each one costs;
- give a service the few metrics that matter, with labels that let a problem in one part of it show;
- write logs that a program can question;
- explain cardinality, and decide what may and may not be a label;
- sample logs and traces without losing the events you will want.

---

## The problem

*An illustrative scenario; the example reproduces it.* A shop's dashboard has three panels, and all three are green. Errors: 0.5%, under the line drawn at 1%. Mean response time: 24 ms. The 99th percentile: under 100 ms.

The support queue has a different view. Since the morning, some customers have been unable to pay. Not slowly: not at all. Every attempt fails, for them, and has done for hours.

Both are right. One customer in twenty cannot check out. Checkout is a tenth of the traffic. A twentieth of a tenth is half a per cent, which is what the dashboard says. The dashboard is answering *how is the average request?*, and no customer has ever made an average request. Thirty-nine of them, in the ten seconds the example runs, made requests that failed every time.

Chapter 6.1 called this grey failure: the detector does not see what the users see. The detector there was a health check. Here it is the whole of the team's knowledge of its own system. This phase is about that knowledge: what to collect, what to promise, when to wake someone, and what that person does. It starts with the collecting.

---

## The concept

### 1. Two questions

Google's SRE book states what monitoring is for: "Your monitoring system should address two questions: what's broken, and why?" ([Google SRE book, *Monitoring Distributed Systems*](https://sre.google/sre-book/monitoring-distributed-systems/)). The first is a **symptom**: something that a user would notice. The second is a **cause**.

The scenario's dashboard fails at the first question. Most of this chapter is about not doing so: measuring symptoms where users feel them, and in pieces small enough that one part's failure is not averaged away. The second question needs something else: detail about single requests, kept in a form that can be searched.

### 2. Three signals

Telemetry comes in three kinds. They are not alternatives. Each answers a question that the others cannot answer at a bearable cost.

| | What it is | What it answers | Its cost grows with | What it cannot say |
|---|---|---|---|---|
| **Metrics** | Numbers, added up as they happen: counts, and distributions | How many? How fast? Is it getting worse? | The number of separate series kept | Which request, which customer |
| **Logs** | A record of each event | What happened to *this* one? What do the failures have in common? | The number of events | Cheaply: how many |
| **Traces** | One request's path through every service, with times | Where did the time go? Which hop failed? | The number of requests traced | Trends: traces are sampled |

**Metrics.** Two kinds do most of the work. Prometheus's definitions: "A counter is a cumulative metric that represents a single monotonically increasing counter whose value can only increase or be reset to zero on restart", and "A histogram records observations (usually things like request durations or response sizes) by counting them in configurable buckets" ([Prometheus documentation, *Metric types*](https://prometheus.io/docs/concepts/metric_types/)). A metric is cheap because it forgets: ten thousand requests become a handful of numbers.

**Logs.** OpenTelemetry's definition: "A log is a timestamped text record, either structured (recommended) or unstructured, with optional metadata" ([OpenTelemetry, *Logs*](https://opentelemetry.io/docs/concepts/signals/logs/)). A log forgets nothing, and is priced accordingly.

**Traces.** "Traces give us the big picture of what happens when a request is made to an application", and they are made of spans: "A span represents a unit of work or operation" ([OpenTelemetry, *Traces*](https://opentelemetry.io/docs/concepts/signals/traces/)). Chapter 7.4 is about them.

In an investigation they are used in that order. A metric says that checkout is failing for some requests. The logs say which ones, and what they share. A trace of one of them says which call inside it took the time.

### 3. Which metrics, and which labels

**For every service: rate, errors, duration.** The RED method, as Grafana's account of it gives it: "Rate (the number of requests per second) Errors (the number of those requests that are failing) Duration (the amount of time those requests take)" ([Grafana Labs, *The RED Method*](https://grafana.com/blog/2018/08/02/the-red-method-how-to-instrument-your-services/)). These are symptoms: what the service's callers experience.

**For every resource: utilisation, saturation, errors.** Brendan Gregg's USE method: "For every resource, check utilization, saturation, and errors" ([Gregg, *The USE Method*](https://www.brendangregg.com/usemethod.html)). Processors, memory, disks, connection pools, the places of Chapter 6.4. These are causes, or the beginnings of them.

Chapter 1.7's four golden signals are both lists, merged.

**Labels are where a failure can hide, or cannot.** A metric with no labels is the scenario's dashboard. A **label** divides a metric into separate series — requests *by endpoint*, *by status* — so that each can be looked at alone. Choose labels by asking along which lines the service could fail in part:

- **the endpoint**, or the kind of operation;
- **the outcome**: success, the caller's error, the service's error;
- **where it ran**: zone, region, and the version of the code, which is what shows a bad deploy (Chapter 9.2).

**Record durations as histograms** (Chapter 1.7), and remember what a percentile from a histogram is. The system knows only how many requests fell in each bucket, and estimates within the bucket. The example reports a 99th percentile of 453 ms for checkout, where every slow request in fact took 400: both are in the bucket from 250 to 500. Put bucket boundaries near the latencies that you have promised (Chapter 7.2).

### 4. Cardinality: what a label costs

It is tempting, after the scenario, to add a label for the customer. Then the dashboard could show each customer's error rate.

Every combination of label values is a series of its own, kept in memory, indexed and stored. Prometheus's documentation gives the warning in capitals: "Remember that every unique combination of key-value label pairs represents a new time series, which can dramatically increase the amount of data stored. Do not use labels to store dimensions with high cardinality (many different label values), such as user IDs, email addresses, or other unbounded sets of values" ([Prometheus documentation, *Metric and label naming*](https://prometheus.io/docs/practices/naming/)).

The number of series is a product:

> series = values of label 1 × values of label 2 × …

Three endpoints and two outcomes: 6. Add twenty shards: 120. Add two thousand customers in place of the shards: 12,000 — and that is for one metric, and a shop with two thousand customers.

**The rule: a label's values must be few, and known in advance.** Endpoint, outcome, zone, version: yes. Customer, order, request, IP address, or a URL with an identifier in it: never.

Then where does the customer go? Into the signals that keep each event: the log line, and the trace.

### 5. Logs that a program can question

A line such as `checkout failed for customer 4411 (shard 7) after 400ms` is written for a person. To ask ten thousand such lines *which shard?*, somebody has to write a pattern for every wording that every author used.

A **structured** log is a record with named fields. Go's standard library has had one since [version 1.21](https://go.dev/doc/go1.21): "Package slog provides structured logging, in which log records include a message, a severity level, and various other attributes expressed as key-value pairs" ([Go documentation, *log/slog*](https://pkg.go.dev/log/slog)).

```go
logs.Info("request",
    "endpoint", r.URL.Path, "status", status, "ms", ms,
    "customer", customer, "shard", shard, "region", region, "version", version)
```

```json
{"time":"…","level":"INFO","msg":"request","endpoint":"/checkout","status":500,"ms":400.6,"customer":1387,"shard":7,"region":"us","version":"v42"}
```

Three habits make such logs worth their cost.

- **One wide line for each request**, written when it ends, with everything known about it. Stripe calls these *canonical log lines*: "in addition to their normal log traces, requests also emit one long log line at the end that includes many of their key characteristics. Having that data colocated in single information-dense lines makes queries and aggregations over it faster to write, and faster to run" ([Stripe, *Fast and flexible observability with canonical log lines*](https://stripe.com/blog/canonical-log-lines)).
- **The same field names everywhere**: `customer` in one service and `cust_id` in the next make a question that spans two services twice as hard.
- **An identifier that joins it to the rest**: the trace's ID, so that a log line leads to the trace and back (Chapter 7.4).

And one prohibition: no passwords, tokens, card numbers or personal details. Logs are copied to more places, and read by more people, than the database they came from (Chapter 8.4).

With lines like these, *why* often has a mechanical first step: for each field, compare its commonest value among the failures with its share of all requests. A field in which the two differ sharply is pointing at something. The example does exactly this.

### 6. Sampling

At a thousand requests a second, one line each is 86 million lines a day, and a trace of each is several times that. Most are records of nothing going wrong. **Sampling** keeps a share.

- **Keep every failure, and every slow request.** They are the ones that will be searched for.
- **Keep a small share of the rest**: one in a hundred, or less.
- **Record the rate with each line that is kept**, so that a count can be scaled back up.

Sampling damages counting. A hundred successes kept from ten thousand give an estimate of the total that is a few per cent out, and a rare kind of request may not be in the sample at all. **Count with metrics; explain with logs.** Each is bad at the other's job.

---

## A real example

### Run it: ten seconds, three views

[`code/ch01-telemetry/main.go`](code/ch01-telemetry/main.go), about 350 lines, runs the scenario: a real HTTP server with three endpoints, 10,000 requests from 2,000 customers, and a checkout that fails for every customer whose data is on database shard 7. The server keeps one histogram, with two labels, and writes one structured line for each request.

```bash
go run ./phase-7-observability/code/ch01-telemetry
```

```text
PART 1 — ten seconds, 10,000 requests. Checkout is failing for one customer in twenty.

  1. The totals
                requests     errors         mean 99th percentile
  everything       10000       0.5%        24 ms           89 ms

  2. The same metric, by its endpoint label
                requests     errors         mean 99th percentile
  /search           5902       0.0%        25 ms           50 ms
  /cart             3102       0.0%        10 ms           25 ms
  /checkout          996       5.3%        59 ms          453 ms

  3. The logs: 10000 lines, of which 53 are failures. What do the failures have in common?
  attribute    commonest value             share of the failures      share of all requests
  endpoint     /checkout                                    100%                        10%
  region       us                                            55%                        50%
  version      v42                                           74%                        65%
  shard        7                                            100%                         5%
  customers affected: 39 of 2,000
```

(Times vary by a millisecond or two from run to run. The counts do not.)

**View 1 is the scenario's dashboard**, and every number on it is true.

**View 2 is the same metric, divided by one label.** Nothing new was measured. The 53 failures are no longer diluted by 9,000 requests to other endpoints, and checkout shows a rate of errors ten times the total's, and a 99th percentile five times as long. This is the answer to *what is broken?*

**View 3 is the logs, asked one question.** All the failures are checkouts, which was known already. Region and version are what they are for all traffic: neither is the cause. Then shard: all 53 failures are on shard 7, which has a twentieth of the requests. That is not yet a diagnosis. It is the place to look, found in one query.

The customer and the shard appear only in view 3. Neither could have been a label, as Part 2 shows.

```text
PART 2 — what a label costs. One histogram of request times; each row adds a label.

  labels                                                    time series  memory, in this
                                                                                 program
  endpoint (3 values), status (2)                                     6             1 KB
  …and shard (20)                                                   120            24 KB
  …and customer (2,000)                                           12000             2 MB
  …and customer (200,000)                                       1200000           233 MB
```

The memory is this program's own, at about 190 bytes for each series, for one metric, in one process. A real monitoring system keeps each series' history as well, and indexes it. The shard, with twenty values, would have been an acceptable label, and would have shown the fault in view 2. The customer is a different order of thing: its cost follows the growth of the business.

```text
PART 3 — the same log, keeping every failure and one success in a hundred.

                                          lines        bytes   failures in it
  every request                           10000      1741585               53
  failures, and 1 success in 100            144        25244               53

  requests, counted by the metric:                 10000
  requests, estimated from the sampled log:        9153   (91 successes × 100, and 53 failures)
```

The sampled log is a seventieth of the size, and has every failure in it: view 3's question gets the same answer. What it has lost is the count. Ninety-one successes were kept where a hundred were expected, and the estimate of the total is 8% short. The metric knew the answer exactly, in six numbers.

### How Stripe uses one line for each request

Stripe's engineers describe the limits of metrics in the terms of this chapter: "The emitted metrics and dashboards that interpret them are designed in advance, and in a pinch they're often difficult to query in creative or unexpected ways. Where logging really shines in comparison to these systems is flexibility" ([Stripe, *Fast and flexible observability with canonical log lines*](https://stripe.com/blog/canonical-log-lines)).

Their difficulty with ordinary logs was that the facts about one request were spread over many lines: "Relevant information is spread across many individual log lines, and even with the most powerful log processing systems, searching for the right details can be slow and requires intricate query syntax" ([Stripe](https://stripe.com/blog/canonical-log-lines)). The canonical line gathers them — the endpoint, the status, the duration, the user, and whatever else the request learned on its way — so that a question such as *which users are being rate limited the most?* is one aggregation over one kind of line.

That is view 3. The line is designed once, as a table would be, and every later question is a query.

---

## Trade-offs

**Labels against cost.** Each label multiplies the series. Too few, and a failure in one part is averaged away. Too many, and the monitoring system is the most expensive service you run. Start with endpoint and outcome, and add a label when an incident shows that one was missing.

**Metrics against logs.** A metric is cheap, fast to query, and answers only the questions that its labels foresaw. A log answers questions nobody foresaw, and costs in proportion to traffic. A service needs both, and should not ask either to do the other's work.

**Sampling against certainty.** A sampled log is small and may lack the one line you want. Keeping failures and slow requests covers most of what is searched for. The request that succeeded quickly and did the wrong thing is not covered.

**Detail against privacy.** The more a log line holds, the more useful and the more dangerous it is. Decide which fields may be logged, as a list, and review it as you would a schema (Chapter 8.4).

**Instrumentation is code.** It runs on every request, it can be wrong, and a metric that is misleading is worse than none. The latency of a failed request and that of a successful one are different quantities: record the outcome with the time.

---

## Practice

### Check yourself

1. A service has a single metric, `requests_total`, with labels `endpoint` (40 values), `status_code` (12), `zone` (3) and `version` (usually 2 during a deploy). How many series is that? Somebody proposes adding `user_agent`. What do you say?
2. An alert fires: *error rate above 2%*. Which signal do you look at first, and what do you do with each of the other two?
3. A team samples its request logs at 1 in 1,000 and uses them to produce the weekly figure for *requests served, by customer*. What is wrong, and what should produce that figure?

<details>
<summary>Answers</summary>

1. 40 × 12 × 3 × 2 = **2,880** series, which is comfortable. `user_agent` is free text that the client sends: there are thousands of values, anyone can invent new ones, and each would multiply 2,880. It belongs in the log line. If a coarse version is useful on a dashboard, reduce it first to a handful of fixed values, such as *browser*, *mobile app*, *other*.
2. Metrics first, divided by their labels: which endpoint, which zone, which version? That narrows *what*. Then the logs for the failing part: what do the failures share that the successes do not? Then a trace of one failing request, to see which call inside it failed or was slow.
3. A sample of 1 in 1,000 gives a noisy count for large customers and, for a customer with a few hundred requests a week, usually none at all. Counting is a metric's job — but *by customer* cannot be a label. So this needs a count kept on purpose: a counter in the database, or a batch job over the unsampled records (Chapter 4.4). It is a product feature with a storage cost, not a by-product of logging.

</details>

### Exercise: instrument the payments API

Take Capstone 6's `POST /payments`. It records the request, calls the bank through a breaker and a bulkhead, and answers *paid*, *declined*, *pending*, or *not charged, try later*.

1. Choose its metrics, with their labels, and work out the number of series.
2. Write the fields of its one log line.
3. For each of these, say which signal shows it first, and in what: (a) the bank becomes slow; (b) one shop's requests are all being declined; (c) payments left *pending* are not being settled.

<details>
<summary>Worked solution</summary>

**1. Metrics.**

| Metric | Kind | Labels | Series |
|---|---|---|---|
| `payments_requests` | Histogram of duration | `outcome` (paid, declined, pending, not_charged, refused), `zone` (3), `version` (2) | 5 × 3 × 2 = 30, times the buckets |
| `bank_calls` | Histogram of duration | `result` (approved, declined, error, timeout), `zone` (3) | 12, times the buckets |
| `bank_breaker_open` | Gauge: 0 or 1 | `instance` (9) | 9 |
| `payments_in_progress`, `places` | Gauges | `instance` (9) | 18 |
| `payments_unsettled`, `oldest_unsettled_seconds` | Gauges | none | 2 |
| `replays` | Counter | `zone` (3) | 3 |

About 75 series before buckets. The shop is not a label: there are thousands. `instance` is one, although instances come and go, because there are few at a time and a single bad one must be findable; some teams keep that for logs too.

**2. The log line.** The request's ID and trace ID; the shop; the key, hashed; the amount and currency; the outcome; the duration, and the time spent waiting for the bank; the bank's result, and the reference sent to it; whether this was a repeat; whether the breaker was open; the instance, zone and version; and the time that the caller had left when the request arrived. Not the card number, and not the customer's name.

**3. What shows first.**

- **(a) The bank is slow**: the `bank_calls` histogram, in its upper buckets, and `timeout` rising. Then `payments_requests` with `outcome=pending`, and then `not_charged` as the breakers open. Metrics are enough to see it, and to alert on it.
- **(b) One shop is always declined**: nothing in the metrics. `declined` is a normal outcome, and one shop's share of it is small. It is in the logs: outcomes grouped by shop. If this needs to be noticed without a person asking, it needs a job that asks.
- **(c) Pending payments are not being settled**: `oldest_unsettled_seconds`, which should never be large. This is a metric that exists only because someone foresaw the failure. A system's own bookkeeping — queues, backlogs, things waiting — always deserves a gauge for its size and for the age of its oldest item (Chapter 4.6).

</details>

---

## Production tips and common mistakes

- **Measure where the user is**, or as near as you can get: at the balancer, or in the client. A service's own count does not include the requests that never reached it.
- **Every service: rate, errors, duration**, by endpoint and outcome. Every pool, queue and limit: how full, how long the wait, how many refused.
- **One wide structured line for each request**, with the same field names in every service, and the trace ID.
- **Keep identifiers out of labels.** Check a new label's values before it ships: how many now, and what makes the number grow?
- **Watch the telemetry's own cost**: series for each service, log bytes a day. Both grow without anyone deciding that they should.
- **Sample by keeping what is interesting**, and record the rate.
- **Put the version on everything.** Most incidents begin with a change (Chapter 9.2), and *did it start with the deploy?* should be a glance.

**Common mistakes**

- **A dashboard of totals and averages.**
- **A URL as a label**, with an ID in the path: one series for every order ever placed.
- **Logs written as prose.**
- **Logging at every step and never at the end**: forty lines for a request, and none that says how it finished.
- **Secrets in logs.**
- **Counting from sampled logs.**
- **Alerting on a cause that users cannot feel**, which is the subject of Chapter 7.3.

---

## Key takeaways

- Monitoring answers two questions: what is broken, and why. The first needs symptoms measured where users feel them. The second needs detail about single requests.
- Metrics count cheaply and forget which request. Logs remember each event and count badly. Traces follow one request through every service. Use all three, in that order.
- Totals hide partial failures. Labels divide a metric along the lines on which a service can fail in part: endpoint, outcome, place, version.
- Every combination of label values is a series. Labels must have few values, known in advance. Customers, orders and requests go in logs and traces.
- A structured log is a table that you can query: one wide line for each request, with consistent names.
- Sample by keeping failures and slow requests. Count with metrics, never with a sample.

**Next:** 7.2 SLIs, SLOs, and error budgets in practice *(planned)* — turning *is it working?* into a number, a target, and a rule for what to do when the target is missed.

---

## Sources

- Rob Ewaschuk, [*Monitoring Distributed Systems*](https://sre.google/sre-book/monitoring-distributed-systems/), chapter 6 of *Site Reliability Engineering*, Google, 2016.
- Prometheus documentation: [*Metric types*](https://prometheus.io/docs/concepts/metric_types/) and [*Metric and label naming*](https://prometheus.io/docs/practices/naming/).
- OpenTelemetry documentation: [*Logs*](https://opentelemetry.io/docs/concepts/signals/logs/) and [*Traces*](https://opentelemetry.io/docs/concepts/signals/traces/).
- Brandur Leach, [*Fast and flexible observability with canonical log lines*](https://stripe.com/blog/canonical-log-lines), Stripe, 2019.
- Julie Dam, [*The RED Method: How to Instrument Your Services*](https://grafana.com/blog/2018/08/02/the-red-method-how-to-instrument-your-services/), Grafana Labs, 2018, on Tom Wilkie's method.
- Brendan Gregg, [*The USE Method*](https://www.brendangregg.com/usemethod.html).
- Go documentation, [*log/slog*](https://pkg.go.dev/log/slog).
