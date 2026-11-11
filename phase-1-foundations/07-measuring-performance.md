# 1.7 Measuring performance: percentiles, load tests, and bottlenecks

[Syllabus](../SYLLABUS.md) · Phase 1 · Previous: [1.6 Inside the server](06-concurrency-pools-queues.md) · Next: [Capstone: a URL shortener on one server](capstone-url-shortener.md)

> **Time:** about 50 minutes to read, 60 minutes of practice.
> **You need:** [Chapter 1.1](01-what-production-demands.md) (percentiles) and [Chapter 1.6](06-concurrency-pools-queues.md) (queues, utilisation, saturation).

**By the end of this chapter you can**

- name the four signals to measure on any service, and say why you need all four;
- record latency so that percentiles can be calculated — and combined across servers — correctly;
- explain the difference between open-loop and closed-loop load, and what "coordinated omission" hides;
- pick the kind of load test that answers the question you actually have;
- find a bottleneck by method: saturation first, then a profiler.

---

## The problem

*An illustrative scenario.* Before launch, a team load-tests its service. A tool simulating 50 users runs for five minutes against a staging server and reports: **average latency 40 ms at 500 requests per second.** The target was 300. They ship.

In production, at 300 requests per second, users wait for seconds. The p99 is 4 s.

The test was not faked and the tool was not broken. Four ordinary decisions each hid part of the truth:

1. **They reported the average.** (Chapter 1.1.)
2. **The load generator waited for each response before sending the next request.** When the server stalled, the generator stalled with it — and so it sent almost nothing during exactly the moments that mattered.
3. **Every simulated user requested the same product.** Everything was answered from a warm cache. Production users ask for millions of different things.
4. **The test ran for five minutes.** The problem — a slow leak — takes an hour to appear.

Brendan Gregg, whose writing on performance methodology is used throughout the industry, summarises this kind of testing in one line: "you benchmark A, but actually measure B, and conclude you've measured C" ([*Active Benchmarking*](https://www.brendangregg.com/activebenchmarking.html)).

Chapters 1.1 to 1.6 gave you a model of what a server does. This chapter is about checking the model against reality without fooling yourself — which is harder than it sounds, because a wrong measurement looks exactly like a right one.

---

## The concept

### 1. What to measure: four signals

Google's SRE book gives a short list that has become the standard starting point: "The four golden signals of monitoring are latency, traffic, errors, and saturation. If you can only measure four metrics of your user-facing system, focus on these four" ([SRE book, *Monitoring Distributed Systems*](https://sre.google/sre-book/monitoring-distributed-systems/)).

| Signal | The question | Notes |
|---|---|---|
| **Latency** | How long do requests take? | A distribution, not a number. Track failed requests separately from successful ones. |
| **Traffic** | How much is being asked of us? | Requests per second. |
| **Errors** | How many requests fail? | Including the ones that fail *fast*. |
| **Saturation** | How full are we? | Queue lengths, pool waits, requests in flight (Chapter 1.6). |

None of the four means much alone. Latency is meaningless without the traffic it was measured at (Chapter 1.1). A falling latency is bad news if errors are rising — failures are often quick, and the [SRE book](https://sre.google/sre-book/monitoring-distributed-systems/) warns both ways: counting fast errors into your latency "might result in misleading calculations", while "a slow error is even worse than a fast error". And saturation is the early warning for the other three; the same chapter notes that "many systems degrade in performance before they achieve 100% utilization" — which you saw as a table in Chapter 1.6.

### 2. Recording latency so that the numbers mean something

**Averages hide the tail.** You know this from Chapter 1.1. The [SRE book's](https://sre.google/sre-book/monitoring-distributed-systems/) version: "If you run a web service with an average latency of 100 ms at 1,000 requests per second, 1% of requests might easily take 5 seconds." And because of fan-out, "the 99th percentile of one backend can easily become the median response of your frontend."

**Record a histogram, not a number.** To compute a percentile you need the distribution. Storing every individual latency is expensive, so services keep a **histogram**: a set of buckets and a count of the requests that fell in each. The [SRE book](https://sre.google/sre-book/monitoring-distributed-systems/) describes it: "collect request counts bucketed by latencies… how many requests did I serve that took between 0 ms and 10 ms, between 10 ms and 30 ms, between 30 ms and 100 ms, between 100 ms and 300 ms, and so on?" Any percentile can be estimated afterwards from the counts.

**You cannot average percentiles.** This is the mistake that follows directly from the last point. Suppose you have ten servers, each reporting its own p99, and you want the p99 of the whole service. Averaging the ten numbers is tempting and wrong. The Prometheus documentation is blunt: "averaging the quantiles yields statistically nonsensical values" ([Prometheus, *Histograms and summaries*](https://prometheus.io/docs/practices/histograms/)).

A small example shows why. Server A handled 9,900 requests with a p99 of 20 ms. Server B, which was nearly idle and sick, handled 100 requests with a p99 of 2 s. The average of the two p99s is about one second — a number that describes nothing. Server B's requests are 1% of the total and should count for 1%, not for half.

The correct method is to **add up the histograms** — bucket counts from different servers simply sum — and compute the percentile from the combined histogram. This is the reason to record histograms in the first place: unlike precomputed percentiles, they are "represented in a way that can be aggregated with each other" ([same page](https://prometheus.io/docs/practices/histograms/)).

**Mind the window.** A p99 over 24 hours can look healthy while containing five catastrophic minutes. Look at short windows as well as long ones, and look at the maximum.

**Measure from outside.** Time recorded inside a handler starts after any queueing in front of it (Chapter 1.6). Measure at the load balancer or in the client as well.

### 3. Open and closed load — and the measurement that lies

This section is about the most common way load tests mislead, and almost nobody is taught it.

There are two fundamentally different ways for a load generator — or a real population of users — to behave. A paper from Carnegie Mellon defines them: generators "may be classified as based on a closed system model, where new job arrivals are only triggered by job completions (followed by think time), or an open system model, where new jobs arrive independently of job completions" ([Schroeder, Wierman and Harchol-Balter, *Open Versus Closed: A Cautionary Tale*, NSDI 2006](https://www.usenix.org/legacy/event/nsdi06/tech/schroeder.html)).

- **Closed:** a fixed number of users. Each one sends a request, waits for the answer, perhaps pauses, and sends another. "50 virtual users" is a closed model.
- **Open:** requests arrive at some rate, whatever the server is doing. A slow server does not make the next visitor wait before arriving.

Traffic from the public internet is far closer to open. The person opening your app at 09:00:03 does not know, or care, that the request sent at 09:00:02 has not been answered yet.

The [paper's](https://www.usenix.org/legacy/event/nsdi06/tech/schroeder.html) finding is that "there is a vast difference in behavior between open and closed models", and that "system designers pay little attention to whether a workload generator is closed or open." Here is why it matters so much for latency.

Suppose the server freezes for 300 ms. Watch what each kind of generator does:

```text
Server      fast  fast  fast  [========= frozen for 300 ms =========]  fast  fast  fast

Closed      ●  ●  ●  ●  ●     ●.....................................●  ●  ●  ●  ●  ●
(1 user)                      one request waits; nothing else is sent

Open        ●  ●  ●  ●  ●     ●  ●  ●  ●  ●  ●  ●  ●  ●  ●  ●  ●  ●   ●  ●  ●  ●  ●  ●
                              every one of these waits for the thaw
```

The closed generator sends one request into the freeze and then stops, because it is waiting. Once the server thaws, it carries on. In its results the freeze appears as **one slow sample per user** among thousands of fast ones. Every request that *would* have been sent during the freeze — and would have been slow — simply does not exist.

The open generator keeps sending. Every request that arrives during the freeze is slow, and all of them are recorded.

The closed-loop test has let the server choose when it is measured: it measures heavily when the server is fast and hardly at all when it is slow. This has a name: **coordinated omission**. The k6 load-testing documentation describes the effect — "when the target system is stressed and starts to respond more slowly, a closed model load test will wait, resulting in increased iteration durations and a tapering off of the arrival rate of new VU iterations" — and notes that "in some testing literature, this problem is known as coordinated omission" ([k6 documentation, *Open and closed models*](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/)). The term was popularised by Gil Tene, whose talk [*How NOT to Measure Latency*](https://www.infoq.com/presentations/latency-response-time/) is the classic treatment.

The fix is to generate load on a schedule that does not depend on the server:

- Use a generator with a **constant arrival rate**. In k6 these are the arrival-rate executors, which "use the open model". Gil Tene's `wrk2` exists for this purpose: it produces "a constant throughput load" and "measures response latency from the time the transmission should have occurred according to the constant throughput" ([wrk2 README](https://github.com/giltene/wrk2)).
- **Measure each request from when it was supposed to be sent**, not from when the generator got round to sending it.

The example below runs both kinds of test against the same server. One reports a p99 of 2.9 ms. The other reports 275 ms.

### 4. Kinds of load test

"Load test" is not one thing. Different tests answer different questions, and running the wrong one gives a confident answer to a question you did not ask. This classification follows the [k6 documentation](https://grafana.com/docs/k6/latest/testing-guides/test-types/):

| Test | Load | Duration | The question it answers |
|---|---|---|---|
| **Smoke** | Minimal | Seconds to minutes | Do the test script and the system work at all? |
| **Average-load** | A normal day | 5–60 minutes | Do we meet our objectives under expected conditions? |
| **Stress** | Above normal | 5–60 minutes | How does it behave when busier than expected? |
| **Spike** | Sudden and very high | A few minutes | Does it survive a surge — and does it *recover* afterwards? |
| **Soak** | Normal | Hours | Does anything get worse over time? (Leaks, growing tables, full disks.) |
| **Breakpoint** | Rising until something breaks | As long as needed | Where is the limit, and what gives way first? |

The scenario at the top needed a soak test and got a five-minute average-load test.

A test only predicts production to the extent that it resembles production. The usual differences, each of which invalidates results:

- **Data volume.** A query over a thousand rows tells you nothing about the same query over a hundred million. (Chapter 1.4's pagination experiment took 0.03 ms or 454 ms depending only on where in the table it looked.)
- **Variety.** One user, one product, one search term means everything comes from a cache. Real requests are spread over many keys, unevenly.
- **Arrival pattern.** Perfectly regular requests cause no queueing (the exercise in Chapter 1.6). Real arrivals are bursty.
- **Client behaviour.** Do real clients reuse connections? Retry? Time out?
- **Where the generator runs.** On the same machine as the server, it competes with the server for CPU. And an overloaded generator produces latency numbers about *itself*.

[Gregg's advice](https://www.brendangregg.com/activebenchmarking.html) for making a test trustworthy is to watch the system while it runs: "While the benchmark is running, analyze the performance of all components involved using other tools, to identify the true limiter of the benchmark." If you cannot say what was limiting throughput during the test, you do not yet know what you measured.

**Finding capacity.** Raise the arrival rate in steps, holding each step long enough to settle, and record the p99 at each. The result is the curve from Chapter 1.6: flat, then a knee. Your capacity is the highest rate at which you still meet your objective — **not** the rate at which the server falls over, which is far past the point where users stopped being served well.

### 5. Finding the bottleneck: a method, not a hunch

A slow system has, at any moment, one stage that limits it. Improving anything else changes nothing. The whole craft is finding that stage before spending effort, and it takes three steps.

**Step 1 — Is it waiting or working?** These need different tools, so decide first. Go through the resources and for each one ask Gregg's three questions — the [USE method](https://www.brendangregg.com/usemethod.html): **u**tilisation (how busy?), **s**aturation (is work queueing?), **e**rrors. Cover the hardware — CPU, memory, disk, network — and, just as important, the software resources from Chapter 1.6: thread pools, connection pools, locks, queues.

If something is saturated while the CPU is quiet, you have a *waiting* problem, and the saturated resource is your bottleneck. No profiler is needed; the answer is in the pool's wait time.

**Step 2 — If it is working, profile it.** When the CPU is the busy resource, a **profiler** tells you where the time goes. It samples the running program many times a second and records which function it finds executing; functions that appear in many samples are where the time is spent. Go builds one in: "Profiling tools analyze the complexity and costs of a Go program such as its memory usage and frequently called functions to identify the expensive sections" ([Go, *Diagnostics*](https://go.dev/doc/diagnostics)). The results are usually read as a sorted table or as a [flame graph](https://www.brendangregg.com/flamegraphs.html), Gregg's visualisation "of profiled software so that the most frequent code-paths can be identified quickly and accurately".

One caution that saves hours: a CPU profile shows where a program spends its time "while actively consuming CPU cycles (as opposed to while sleeping or waiting for I/O)" ([Go, *Diagnostics*](https://go.dev/doc/diagnostics)). A request that spends 2 s waiting for a database connection and 1 ms computing will show the 1 ms. **If the problem is waiting, a CPU profile will point you at the wrong thing.** That is why Step 1 comes first.

**Step 3 — Change one thing, and run the same test again.** If the number did not move, you did not fix the bottleneck — however sensible the change looked. If it did move, the bottleneck is now somewhere else; go back to Step 1.

*One line on a related method:* where USE looks at resources, the RED method looks at each service's requests — Rate, Errors, Duration ([Grafana blog](https://grafana.com/blog/2018/08/02/the-red-method-how-to-instrument-your-services/)). You want both views. Phase 7 uses them to build dashboards.

---

## A real example

### Run it: one server, two tests, two answers

[`code/ch07-loadgen/main.go`](code/ch07-loadgen/main.go) starts an HTTP server that answers in about 1 ms but freezes for 300 ms every 3 seconds — it is unusable 10% of the time. It then load-tests that server twice, aiming for 1,000 requests per second each time: once with ten closed-loop "users", once with an open-loop schedule.

```bash
go run ./phase-1-foundations/code/ch07-loadgen
```

```text
A server that answers in ~1 ms but freezes for 300ms every 3s (10% of the time).
Both tests aim for 1000 requests per second for 12s.

Test 1 — closed loop: 10 users, each waits for its answer before sending again
  requests sent: 10850 (the plan was 12000)
  mean 2.7ms   p50 1.5ms   p90 1.8ms   p99 2.9ms   p99.9 301ms   max 302ms
  requests slower than 50 ms: 40 (0.4%)

Test 2 — open loop: requests sent on schedule, whether or not earlier ones came back
  requests sent: 12000 (the plan was 12000)
  mean 17ms   p50 2.0ms   p90 6.4ms   p99 275ms   p99.9 303ms   max 307ms
  requests slower than 50 ms: 1023 (8.5%)
```

(Your numbers will differ slightly; the gap between the two tests will not.)

It is the same server in both tests. Read the two reports as if you were deciding whether to ship.

- **Test 1 says:** p99 of 2.9 ms; four requests in a thousand are slow. A healthy service.
- **Test 2 says:** p99 of 275 ms; one request in twelve is slow. A service with a serious problem.

Test 2 is the truth for independent users: the server is frozen a tenth of the time, so about a tenth of arrivals are caught by it. Test 1 is not lying about the samples it took. It just took almost none during the freezes — ten users were stuck, so only ten slow samples were recorded per freeze: 40 in total over four freezes.

Look at the first line of each report. Test 1 planned 12,000 requests and sent 10,850. The missing 1,150 are the requests that should have been sent during the freezes. They are the omission in "coordinated omission", and they are precisely the requests that would have been slow.

The difference between the two generators is small in code. The closed loop measures from when it sent the request:

```go
start := time.Now()
get(client, url)
took := time.Since(start)
```

The open loop sends on a schedule, in a new goroutine each time so that nothing waits, and measures from the *scheduled* moment:

```go
scheduled := begin.Add(time.Duration(i) * interval)
time.Sleep(time.Until(scheduled))
go func() {
    get(client, url)
    took := time.Since(scheduled)   // from when it was due, not from when it was sent
    ...
}()
```

Notice also what the mean did: 2.7 ms against 17 ms. Even the open-loop average of 17 ms sounds acceptable. It describes a service where a tenth of the users wait a quarter of a second. Averages again.

---

## Trade-offs

**Open or closed.** Use the model that matches the real population. For public traffic that is open, and a closed-loop test will flatter you. But some workloads really are closed — a fixed pool of 20 batch workers, each of which starts its next job only when the last one finishes, will never send more than 20 requests at once. Testing those with an open model exaggerates the problem. The NSDI paper's point is not that one model is right; it is that you must know which one you have.

An open-loop generator has a hazard of its own: if the server stops answering, the generator keeps starting requests, and the number in flight grows without limit. Give it a ceiling, and make it report clearly when it hits the ceiling instead of quietly sending less.

**Histogram resolution.** More buckets give more accurate percentiles and cost more to store and transmit. Fixed buckets also have to be chosen before you know the answer; the SRE book suggests spacing them "approximately exponentially", so that both a 2 ms and a 2 s request land in a bucket that says something useful.

**Where to test.** A staging environment is safe to break and differs from production in data, hardware, and neighbours — sometimes in ways that make its results worthless. Testing in production is real and risky. Most teams end up doing both: structured load tests in staging to catch regressions, and careful, small-scale experiments in production (Chapter 9.2) to learn what staging cannot tell them.

**How long.** Longer tests find slower problems and cost more time and money. Match the duration to the question: minutes for "does it meet the target", hours for "does anything leak".

**Optimise, or add capacity?** Profiling and fixing costs engineering time; another server costs money. A hot spot that a profile finds in an afternoon is nearly always worth fixing. A week of micro-optimisation to avoid one extra machine rarely is. Phase 9 returns to this trade with numbers.

---

## Practice

### Check yourself

Answer before reading on.

1. Server A handled 9,900 requests with a p99 of 20 ms. Server B handled 100 requests with a p99 of 2 s. Can you work out the p99 of the two servers combined? If not, what would you need?
2. A closed-loop test with 20 simulated users runs for 60 seconds and reports a p99 of 15 ms. During the test, the server froze once, for 2 seconds. Roughly how many of the recorded samples experienced the freeze? If real traffic arrives at 1,000 requests per second, roughly what p99 do real users see over that minute?
3. An endpoint takes 2 seconds. Its CPU profile shows nothing remarkable, and the server's CPU is 10% busy. What have you learned, and where do you look next?

<details>
<summary>Answers</summary>

1. **No.** The combined p99 is the latency exceeded by the slowest 1% of all 10,000 requests — the slowest 100. You know that at least 99 of A's requests took 20 ms or more and at least one of B's took 2 s or more, but you know nothing about B's other 99 requests. If they were all fast, the combined p99 is close to 20 ms. If they were all slow, it is far higher. Two percentiles are not enough to reconstruct a distribution. You need the **histograms** from both servers: add the bucket counts, then compute the percentile.

2. About **20** — one per user. Each user sent one request into the freeze and waited. Out of roughly 80,000 samples (20 users at about 15 ms per request for 58 seconds), 20 slow ones are 0.03%, so they do not move the p99 at all.

   For real users: 1,000 requests per second for 2 seconds means **2,000 requests arrive during the freeze**, out of 60,000 in the minute — 3.3%. Their waits are spread evenly between 0 and 2 seconds. The p99 is set by the slowest 1%, which is 600 requests; those are the ones that arrived in the first 0.6 s of the freeze and so waited at least 1.4 s. **Real p99 ≈ 1.4 s**, against a reported 15 ms: a hundred times worse.

3. You have learned that the request is **waiting, not computing**. A CPU profile only sees time spent on the CPU, and an idle CPU confirms there is little of it. Look for where it waits: pool wait times and queue depths (is it waiting for a database connection?), the latency of each call the endpoint makes to a database or another service, and lock contention. A request trace that shows time per step, or the pool's own statistics, will find it. Optimising code would achieve nothing.

</details>

### Exercise: find the hot spot

[`code/ch07-profile/main.go`](code/ch07-profile/main.go) contains an HTTP handler that validates an email address and returns a small JSON document. It is slower than it should be. The program calls the handler in a tight loop on every core for three seconds, prints the throughput, and writes a CPU profile.

**Do not read the handler's code yet.**

1. Run it, and note the requests per second:

   ```bash
   go run ./phase-1-foundations/code/ch07-profile
   ```

2. Look at the profile, sorted by cumulative time — the time spent in a function *and everything it calls*:

   ```bash
   go tool pprof -top -cum -nodecount=14 cpu.prof
   ```

   Read down the `cum%` column. Where does `main.slowHandler`'s time go?

3. Predict the line of code responsible. Then confirm it:

   ```bash
   go tool pprof -list 'main.slowHandler' cpu.prof
   ```

4. Say what the fix is. Run the fixed version and compare:

   ```bash
   go run ./phase-1-foundations/code/ch07-profile -fixed
   ```

5. Last question, and the important one: the real endpoint also runs a database query that takes 20 ms. How much faster will the real endpoint be after your fix?

<details>
<summary>Worked solution</summary>

Numbers are from the 4-core machine this chapter was written on. Yours will differ; the shape will not.

**1. Baseline.**

```text
slow handler: 378944 requests in 3s on 4 cores = 126315 requests per second
```

**2. The profile, by cumulative time.**

```text
      flat  flat%   sum%        cum   cum%
     0.08s  1.02%  1.02%      5.73s 72.81%  main.main.func1
     0.01s  0.13%  1.14%      5.50s 69.89%  main.slowHandler
         0     0%  1.14%      5.50s 69.89%  net/http.HandlerFunc.ServeHTTP (inline)
     0.01s  0.13%  1.27%      4.11s 52.22%  regexp.MustCompile
         0     0%  1.27%      4.10s 52.10%  regexp.Compile (inline)
     0.01s  0.13%  1.40%      4.10s 52.10%  regexp.compile
     0.14s  1.78%  3.18%      2.82s 35.83%  runtime.mallocgc
     ...
```

`flat` is time spent in the function's own code; `cum` includes everything it calls. `main.slowHandler` accounts for 5.50 s of CPU time, and 4.11 s of that — three-quarters — is inside `regexp.MustCompile`. The handler is not slow at *matching* an email address. It is slow at *compiling the pattern*, which it apparently does on every request. (The `runtime.mallocgc` line is a consequence: compiling a pattern allocates a great deal of memory, and allocating it takes time too.)

**3. The line.**

```text
      10ms      5.50s (flat, cum) 69.89% of Total
         .          .     41:func slowHandler(w http.ResponseWriter, r *http.Request) {
         .      190ms     42:	email := r.URL.Query().Get("email")
         .      4.60s     43:	valid := regexp.MustCompile(emailPattern).MatchString(email)
         .      170ms     44:	w.Header().Set("Content-Type", "application/json")
      10ms      540ms     45:	json.NewEncoder(w).Encode(signup{Email: email, Valid: valid})
         .          .     46:}
```

Line 43: 4.60 s of the handler's 5.50 s.

Did you guess JSON encoding? Most people do, and it is 540 ms — a tenth of the total. This is the argument for profiling: the expensive line is frequently not the one that looks expensive.

**4. The fix.** Compile the pattern once, when the program starts, and reuse it:

```go
// Compiled once, when the program starts.
var emailRE = regexp.MustCompile(emailPattern)

func fixedHandler(w http.ResponseWriter, r *http.Request) {
    email := r.URL.Query().Get("email")
    valid := emailRE.MatchString(email)
    ...
```

```text
fixed handler: 1882060 requests in 3s on 4 cores = 627353 requests per second
```

Five times the throughput, from moving one line.

**5. The real endpoint.** Work out what the fix saved *per request*. Before: 4 cores ÷ 126,000 requests per second ≈ 32 microseconds of CPU per request. After: 4 ÷ 627,000 ≈ 6 microseconds. The fix saves about **25 microseconds**.

The real endpoint takes at least 20 milliseconds because of its query. 25 microseconds is about **0.1%** of that. No user will ever notice.

So was the fix worthless? No — but be clear about what it bought. It did nothing for *latency*, which is dominated by the database. It cut the *CPU cost* of each request by a factor of five, which matters if this service is limited by CPU and you are paying for the servers. Those are different goals, and the benchmark only measured one of them.

This is the "benchmark A, measure B, conclude C" trap, in miniature. The benchmark exercised the handler in isolation (A). It measured CPU cost per call (B). "The endpoint is five times faster" (C) does not follow. Before optimising part of a request, measure the whole request and find out how much of it that part is.

</details>

---

## Production tips and common mistakes

**Tips**

- **Export the four signals for every service**, with latency as a histogram.
- **Compute percentiles from combined histograms**, never by averaging per-server percentiles.
- **Use an open-model generator for anything user-facing**, and check what your tool does by default — many default to closed-loop "virtual users".
- **Find the knee.** Step the load up, record the p99 at each step, and write down the rate at which you stop meeting your objective. Repeat after significant changes; a capacity that has quietly halved is a regression like any other.
- **Test with production-sized data and a realistic spread of keys.**
- **Run the load generator on separate machines**, and watch its own CPU and network. A saturated generator measures itself.
- **Watch the system during the test**, not only the report afterwards.
- **Keep a profiler within reach in production.** Go's [`net/http/pprof`](https://pkg.go.dev/net/http/pprof) serves profiles over HTTP under `/debug/pprof/`. Bind it to an internal address only — it must never be reachable from the internet.
- **Change one thing at a time**, and rerun the identical test.

**Common mistakes**

- **Reporting the average**, or one percentile without the load it was measured at.
- **Averaging percentiles** across servers or across time.
- **Closed-loop tests of an open system** — coordinated omission.
- **Tests that are too short** to show leaks, cache expiry, or anything else that takes time.
- **Tiny datasets and a single hot key.**
- **Ignoring errors in the results.** A server that fails instantly posts superb latency figures.
- **Calling the point of collapse "our capacity".** Capacity is where you stop meeting the objective.
- **Optimising without profiling**, or optimising something that is not the bottleneck.
- **Reaching for a CPU profile to explain a waiting problem.**
- **Trusting a number you cannot explain.** If you cannot say what limited throughput during a test, you do not know what the test measured.

---

## Key takeaways

- Measure latency, traffic, errors and saturation together; none of them means much alone.
- Record latency as a histogram. Percentiles can be computed from combined histograms; they cannot be averaged.
- A closed-loop load generator slows down when the server does, and so barely samples the slow moments. For user-facing traffic, generate load at a constant arrival rate and measure from the scheduled send time.
- Choose the test — smoke, average-load, stress, spike, soak, breakpoint — that matches the question.
- To find a bottleneck: first decide whether the system is waiting or working. Waiting shows up as saturation; working shows up in a CPU profile. Then change one thing and measure again.
- A benchmark of a part is not a measurement of the whole.

**Next:** [Capstone — a URL shortener on one server](capstone-url-shortener.md): everything in Phase 1, applied to one design.

---

## Sources

- Google, *Site Reliability Engineering*: [Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/) (the four golden signals).
- Bianca Schroeder, Adam Wierman and Mor Harchol-Balter, [*Open Versus Closed: A Cautionary Tale*](https://www.usenix.org/legacy/event/nsdi06/tech/schroeder.html), NSDI 2006.
- Grafana k6 documentation: [Open and closed models](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/); [Load test types](https://grafana.com/docs/k6/latest/testing-guides/test-types/).
- Gil Tene: [*How NOT to Measure Latency*](https://www.infoq.com/presentations/latency-response-time/) (talk); [wrk2](https://github.com/giltene/wrk2) (constant-throughput load generator).
- Prometheus documentation: [Histograms and summaries](https://prometheus.io/docs/practices/histograms/).
- Brendan Gregg: [*Active Benchmarking*](https://www.brendangregg.com/activebenchmarking.html); [*The USE Method*](https://www.brendangregg.com/usemethod.html); [*Flame Graphs*](https://www.brendangregg.com/flamegraphs.html).
- Go: [Diagnostics](https://go.dev/doc/diagnostics); [`net/http/pprof`](https://pkg.go.dev/net/http/pprof); [*Profiling Go Programs*](https://go.dev/blog/pprof) (blog).
- Grafana blog: [*The RED Method: How to Instrument Your Services*](https://grafana.com/blog/2018/08/02/the-red-method-how-to-instrument-your-services/).
