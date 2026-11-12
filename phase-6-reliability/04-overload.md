# 6.4 Overload: rate limiting, load shedding, and admission control

[Syllabus](../SYLLABUS.md) · Phase 6 · Previous: [6.3 Circuit breakers, bulkheads, and graceful degradation](03-circuit-breakers-bulkheads-degradation.md) · Next: 6.5 Redundancy and failure domains *(planned)*

> **Time:** about 30 minutes to read, 40 minutes of practice.
> **You need:** queues and utilisation from [Chapter 1.6](../phase-1-foundations/06-concurrency-pools-queues.md); retry budgets and deadlines from [Chapter 6.2](02-timeouts-and-retries-at-scale.md). The example needs only Go.

**By the end of this chapter you can**

- say why a service that accepts every request can end up finishing none, and what *goodput* measures;
- choose what to do with work that there is no room for: queue it, drop it late, or refuse it early;
- limit each client with a token bucket, and choose its rate and its burst;
- shed load by priority, so that what matters most is what survives;
- explain why a limit on requests *in progress* protects a service in situations where a limit on the *rate* does not.

---

## The problem

A service can finish 200 requests a second. One evening it is sent 220.

It seems that ten per cent too much should cost about ten per cent: 200 answered, 20 not. What happens is this. The service accepts all 220. Each second, 20 more requests are waiting than the second before. After ten seconds a new request waits a full second for its turn, and that is how long its client is prepared to wait. From then on the service is as busy as it has ever been — every worker occupied, 200 requests finished a second — and every one of them is finished just after the client has stopped listening.

**Ten per cent too much has cost a hundred per cent.** And the service does not recover when the traffic falls back to 200, or to 190: it has a second's worth of dead requests in front of every live one, and with clients that retry (Chapter 6.1) it never clears them.

Google's SRE book states what a service should do: "a backend task provisioned to serve a certain traffic rate should continue to serve traffic at that rate without any significant impact on latency, regardless of how much excess traffic is thrown at the task" ([Google SRE book, *Handling Overload*](https://sre.google/sre-book/handling-overload/)). Sent 220, finish 200, quickly, and tell the other 20 *no*.

That takes a decision that most services never make: which requests to refuse. A service that does not decide has decided to refuse them all.

---

## The concept

### 1. Goodput: the work that somebody was still waiting for

Two numbers describe a service under load, and Amazon's engineers define them together: "Throughput is the total number of requests per second that is being sent to the server. Goodput is the subset of the throughput that is handled without errors and with low enough latency for the client to make use of the response" ([AWS Builders' Library, *Using load shedding to avoid overload*](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/)).

Below its capacity, a service's goodput is its throughput. Above it, the two part company, and the queue is what separates them. A request that waits too long costs the service as much work as any other and gives nothing to anyone.

So the aim of everything in this chapter is the one in that article: "Load shedding lets a server maintain its goodput and complete as many requests as it can, even as offered throughput increases" ([AWS Builders' Library](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/)).

### 2. What to do with the excess: queue it, drop it late, or refuse it early

There are three things a service can do with a request that it has no worker for.

**Queue it.** A queue helps with a burst: more work than usual for a moment, followed by less (Chapter 4.1). It does nothing for an excess that lasts, except to hide it until every request is late.

**Drop it later, when its turn comes and its client has gone** (Chapter 6.2). This is better, and has a trap in it, which the example will show: a request that has 5 ms of its client's patience left is not yet expired, will be started, and will be finished too late. The request to drop is the one that can no longer be *finished* in time. Even done properly, this serves requests as late as it possibly can: everyone who is answered has waited nearly the whole of their patience.

**Refuse it at the door.** Keep the queue short, and when it is full, say *no* immediately. The SRE book's advice on its length: "it is usually better to have small queue lengths relative to the thread pool size (e.g., 50% or less), which results in the server rejecting requests early when it can't sustain the rate of incoming requests" ([Google SRE book, *Addressing Cascading Failures*](https://sre.google/sre-book/addressing-cascading-failures/)).

The length of a queue is a length of time:

> longest wait = requests waiting ÷ requests finished a second

Twenty waiting, at 200 a second, is a tenth of a second. Choose the wait first, as a small part of what clients will tolerate, and work out the length from it.

Refusing early is kind to the client as well as the service. A client that is told *no* in a millisecond can show a message, try another region, or try again in a while. A client that is told nothing for a second can only give up.

**Say what kind of *no* it is.** HTTP has two.

- `429 Too Many Requests`: *you* are sending too much. "The 429 status code indicates that the user has sent too many requests in a given amount of time" ([RFC 6585](https://www.rfc-editor.org/rfc/rfc6585)).
- `503 Service Unavailable`: *we* have too much, whoever you are.

Either may carry `Retry-After`, which servers send "to indicate how long the user agent ought to wait before making a follow-up request" ([RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html)). A refusal is a failure that the layer above should think twice about retrying, and never at once (Chapter 6.2).

### 3. A limit for each client: the token bucket

Some overload is one client's doing: a script in a loop, a retry storm in somebody else's system, a customer who has grown. Without a limit for each client, the service shares its capacity in proportion to how much each one sends, so the client that misbehaves most gets most.

The usual limiter is the **token bucket**. Stripe's description: "you take tokens on each request, and slowly drip more tokens into the bucket. If the bucket is empty, reject the request. In our case, every Stripe user has a bucket" ([Stripe, *Scaling your API with rate limiters*](https://stripe.com/blog/rate-limiters)).

A bucket has two numbers. Go's library states what they promise: "in any large enough time interval, the Limiter limits the rate to r tokens per second, with a maximum burst size of b events" ([Go documentation, *golang.org/x/time/rate*](https://pkg.go.dev/golang.org/x/time/rate)).

```go
// A bucket holds up to burst tokens and gains rate tokens a second. A request takes one.
func (b *bucket) allow(now float64) bool {
    b.tokens = min(b.burst, b.tokens+(now-b.last)*b.rate)   // what has dripped in since last time
    b.last = now
    if b.tokens < 1 {
        return false
    }
    b.tokens--
    return true
}
```

- **The rate** is what the client may send, sustained.
- **The burst** is how far it may exceed that for a moment: a page that makes twelve calls at once is not an attack. Stripe added exactly this: "the ability to briefly burst above the cap for sudden spikes in usage during real-time events" ([Stripe](https://stripe.com/blog/rate-limiters)).

Three decisions come with it.

**Who is a client?** An API key or an account, if you have one. An IP address is a poor stand-in: an office of a thousand people may share one.

**Where is the bucket?** With one server, in memory. With fifty, either each server enforces a fiftieth of the limit, which is simple and exact only when traffic is spread evenly; or the buckets are in a shared store, which every request must then consult. Stripe's are in Redis, and its engineers are clear about what must happen when that store cannot be reached: "Make sure that if there were bugs in the rate limiting code (or if Redis were to go down), requests wouldn't be affected" ([Stripe](https://stripe.com/blog/rate-limiters)). A limiter is there to protect availability. When it fails, it lets requests through.

**Is the limit a promise?** A rate limit is part of an API's contract (Chapter 1.4). Publish it, return `429`, and say when to try again.

A limit on the rate is not the only kind. Stripe also limits how many requests each user may have *in progress*, for its expensive endpoints, and explains the reason in terms that Chapter 6.1 would recognise: users "get frustrated waiting for the endpoint to return and then retry. These retries add more demand to the already overloaded resource" ([Stripe](https://stripe.com/blog/rate-limiters)).

### 4. Shedding by priority

Limits for each client stop one client from taking everything. They do not help when every client is behaving and the total is still too much: a sale, a failed zone, a slow dependency. Then something must be dropped, and the question is what. Stripe separates the two tools: a rate limiter looks at who is asking, whereas "A load shedder makes its decisions based on the whole state of the system rather than the user who is making the request" ([Stripe](https://stripe.com/blog/rate-limiters)).

Dropping at random treats a payment and a crawler alike. So requests are given a **priority**, and the least important are refused first.

- Google's RPC system has four levels, and a rule about the top two: "Services are expected to provision enough capacity for all expected CRITICAL and CRITICAL_PLUS traffic" ([Google SRE book, *Handling Overload*](https://sre.google/sre-book/handling-overload/)). The lower levels are for work that can be done later, such as batch jobs.
- Stripe also has four: critical methods, other writes, reads, and test traffic. When a machine has too much to do, "it will slowly start shedding less-critical requests, starting with test mode traffic" ([Stripe](https://stripe.com/blog/rate-limiters)).
- Stripe also keeps room back in advance: "We always reserve a fraction of our infrastructure for critical requests. If our reservation number is 20%, then any non-critical request over their 80% allocation would be rejected with status code 503" ([Stripe](https://stripe.com/blog/rate-limiters)). That is a bulkhead (Chapter 6.3).

Three things make it work.

- **A request carries its priority**, set once at the edge and passed down with the deadline. A service in the middle cannot guess it.
- **Few levels.** Google reports that "four values were sufficiently robust to model almost every service" ([Google SRE book](https://sre.google/sre-book/handling-overload/)). With twenty, everything ends up in the top one.
- **Shed and restore gradually.** Stripe again: "It's very important that shedding and bringing load happen slowly, or you can end up flapping" ([Stripe](https://stripe.com/blog/rate-limiters)).

### 5. Limit what is in progress, not only what arrives

A rate limit of *200 a second* contains a guess: that the service can do 200 a second. The guess was right on the day of the load test. It is wrong whenever a request costs more than it did then: a dependency is slow, a neighbour is using the disk, requests have grown, a third of the servers are being replaced.

Netflix's engineers describe the usual practice and its fault: "RPS limits are then set somewhere below this tipping point (say 75% of this value) and enforced via a token bucket. However, in large distributed systems that auto-scale this value quickly goes out of date and the service falls over by becoming non-responsive as it is unable to gracefully shed excess load. Instead of thinking in terms of RPS, we should be thinking in terms of concurrent requests" ([Netflix, *concurrency-limits*](https://raw.githubusercontent.com/Netflix/concurrency-limits/main/README.md)).

A limit on requests **in progress** needs no guess. By Little's law (Chapter 1.6), requests in progress = requests a second × seconds each. Hold the first number fixed, and when requests become slower, the rate that is let in falls by itself, to whatever the service can now finish.

It is a dozen lines of Go:

```go
// limit lets at most n requests into a handler at once, and refuses the rest at once.
func limit(n int, next http.Handler) http.Handler {
    places := make(chan struct{}, n)
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        select {
        case places <- struct{}{}:
            defer func() { <-places }()
            next.ServeHTTP(w, r)
        default:
            w.Header().Set("Retry-After", "1")
            http.Error(w, "too busy", http.StatusServiceUnavailable)
        }
    })
}
```

This is Chapter 6.3's bulkhead, turned to face inwards: there it limited what a service spends on a dependency, and here it limits what the world may spend on the service. The right number is found by load test (Chapter 1.7): the concurrency at which latency starts to climb. Libraries such as Netflix's adjust it while the service runs, by watching latency; start with a fixed number.

**Use both kinds.** The limit on each client's rate is about fairness: no client takes more than its share. The limit on what is in progress is about survival: the service never takes on more than it can finish.

---

## A real example

### Run it

[`code/ch04-overload/main.go`](code/ch04-overload/main.go) is about 340 lines. Parts 1 to 4 are a model with no network and no clock: a service with 20 workers, each of which takes 100 ms over a request, and clients that wait a second. Requests arrive at slightly irregular moments, from a fixed seed. Part 5 is the middleware above, over real HTTP.

```bash
go run ./phase-6-reliability/code/ch04-overload
```

```text
PART 1 — a service that can finish 200 requests a second, and is sent more. A client waits 1 s.

  answered in time, a second                                100 /s    180 /s    220 /s    300 /s    600 /s sent
  every request waits its turn                                 101       180         0         0         0
  …and one whose client has gone is dropped unstarted          101       180         0         0         0
  …and so is one that can no longer be finished in time        101       180       200       200       200
  at most 20 may wait; the rest are refused at once            101       180       200       200       200

  the median answer took                                    100 /s    180 /s    220 /s    300 /s    600 /s sent
  every request waits its turn                              100 ms    100 ms         —         —         —
  …and one whose client has gone is dropped unstarted       100 ms    100 ms         —         —         —
  …and so is one that can no longer be finished in time     100 ms    100 ms    997 ms    998 ms    999 ms
  at most 20 may wait; the rest are refused at once         100 ms    100 ms    196 ms    197 ms    198 ms
```

**The first row is the opening of this chapter.** At 180 a second everything is answered. At 220, nothing is.

**The second row is the trap.** The service now throws away any request whose client has already gone — and still answers nobody. In steady overload the request at the front of the queue has always waited a little under a second. It has not expired, so it is started. It is finished 100 ms later, which is too late.

**The third row drops a request when there is no longer time to finish it.** Now the service delivers all 200 a second, at any load. Look at the second table, though: the median answer takes 997 ms. Every user who is served has waited as long as they were willing to.

**The fourth row refuses at the door.** The same 200 a second, in under 200 ms. The requests that are turned away are the same in number. They are turned away at once, where the third row made them wait a second to find out.

```text
PART 2 — three clients. Two send 50 requests a second each. The third sends 1,000.

  answered in time, a second (and as a share of what was sent)        client A, 50 /s        client B, 50 /s     client C, 1,000 /s
  one limit for everybody: at most 20 waiting                              8  (17%)               8  (15%)             184  (18%)
  …and each client may send 80 a second, and no more                     50  (100%)             50  (100%)               80  (8%)
```

With one limit for everybody, the service is protected and its customers are not. It finishes its 200 a second, and 184 of them belong to the client that is misbehaving. The two that send a twentieth as much lose five requests in six. With a bucket for each client, the two are untouched, and the third gets what any client may have.

```text
PART 3 — 300 requests a second for a service that can do 200: 50 payments, 110 page views, and 140 from a
         crawler.

  answered in time, a second (and as a share of what was sent)        payments, 50 /s     page views, 110 /s        crawler, 140 /s
  whoever arrives when 20 are waiting is refused                          33  (65%)              72  (66%)              95  (68%)
  the crawler is refused first, then page views, then payments             50  (100%)            110  (100%)              40  (29%)
```

Nobody here is misbehaving, and there is simply too much. Refusing whoever arrives at a bad moment loses a third of everything, payments included. In the second row a request from the crawler is let in only when nothing is waiting, a page view when fewer than 10 are, a payment when fewer than 20 are. The service finishes the same 200 a second. All the payments and all the page views are among them.

```text
PART 4 — the service is sent 180 requests a second. At second 30, something it depends on slows down, and
         each request takes a worker 250 ms where it took 100: the service can now finish 80 a second.

                                                                  answered in time,      answered in time,       refused at once,
                                                                   a second, before        a second, after        a second, after
  a limit on the rate: 200 a second may come in                                 180                      0                      0
  a limit on requests in progress: 20 working, 20 waiting                       180                     80                     99
```

The rate limit was correct when it was set. The traffic never exceeded it, so it never refused anything — and the service that it was protecting went from 180 answers a second to none. The limit on requests in progress knew nothing about the slowdown either. It did not need to: requests stayed longer, the places stayed full, and what could not come in was refused. Eighty a second were answered, which is all that the service could do, and 99 were told so at once.

```text
PART 5 — real HTTP. A handler that takes 100 ms, behind limit(10, …). 100 requests arrive together.

  served: 10
  refused: 90, with status 503 and the header Retry-After: 1
```

### How Stripe layers its limiters

Stripe's account of its API's limiters is a worked example of this chapter, and each quotation here is from it ([Stripe, *Scaling your API with rate limiters*](https://stripe.com/blog/rate-limiters)).

It runs [four at once](https://stripe.com/blog/rate-limiters), and they are sections 3 to 5 in order. A **request rate limiter**: "This rate limiter restricts each user to N requests per second". A **concurrent requests limiter**, for each user. A **fleet usage load shedder**, the reserved fifth quoted in section 4. And a **worker utilisation load shedder**, the last line of defence, which sheds by priority when a machine's workers are backing up.

How often each one acts is worth noticing. The first "has rejected millions of requests this month alone". The last "gets triggered very rarely, only during major incidents" ([Stripe](https://stripe.com/blog/rate-limiters)). The cheap, per-client limit does the daily work, so that the drastic one almost never has to.

The post is equally useful on how to introduce a limiter without causing the outage it was meant to prevent.

- Run it without enforcing it first: "Dark launch each rate limiter to watch the traffic they would block."
- Keep a way out: "Make sure you have kill switches to disable the rate limiters should they kick in erroneously" ([Stripe](https://stripe.com/blog/rate-limiters)).

---

## Trade-offs

**Refusing work is a product decision.** Every limit in this chapter turns away something that a customer wanted. Engineers choose the mechanism. Which requests matter most, and what a customer is promised, is for the business to say — beforehand.

**A short queue refuses during bursts** that a longer one would have absorbed. If traffic really is bursty, the SRE book suggests sizing the queue from the bursts. Start short, and lengthen it on evidence.

**Limits per client need state**, and with many servers, shared state: a new dependency on the path of every request. Dividing the limit among the servers avoids the dependency and is less exact.

**Priorities can be gamed.** If callers mark their own requests, everything becomes critical. Set the priority at the edge, from what the request is, not from what it says about itself.

**Shedding has a cost of its own.** A refused request must still be received and answered. Amazon's article is plain about the limit: "the act of shedding load isn't free", and at a high enough multiple of capacity, goodput falls anyway ([AWS Builders' Library](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/)). Refuse as early and as cheaply as possible — at the balancer, if it can be done there.

**A fixed limit on requests in progress is still a number someone chose.** It adapts when requests get slower. It does not adapt when the right number itself changes, such as when a service is given more processors.

---

## Practice

### Check yourself

1. A service has 50 workers, and a request takes 40 ms. Its clients wait 2 seconds. What is its capacity? If it accepts everything and is sent 10% more than its capacity, for how long does it go on answering in time? How long may its queue be, if no request is to wait more than 100 ms?
2. A token bucket has a rate of 10 a second and a burst of 50. A client that has been idle for a minute sends 80 requests in one instant, and then 20 a second. What happens?
3. Your API returns `503` when a customer exceeds their rate limit. Why is that the wrong status, and what does it cost?

<details>
<summary>Answers</summary>

1. 50 ÷ 0.04 s = **1,250 a second**. At 10% over, the queue grows by 125 a second, and a request times out when it waits 2 seconds: when 2 × 1,250 = 2,500 are waiting, which takes 2,500 ÷ 125 = **20 seconds**. After that, nothing is answered in time. For a longest wait of 100 ms: 0.1 × 1,250 = **125** waiting; the SRE book's *half the pool* would be 25, and a wait of 20 ms.
2. The bucket is full: 50 tokens. Of the 80, **50 are allowed and 30 refused**. After that the bucket gains 10 a second and the client sends 20, so **half are refused**, for as long as that continues. The burst is spent once. It does not raise the rate.
3. `503` says that the *service* is in trouble. A well-behaved client, or a balancer, may respond by trying another server, which is pointless, since the limit is on the customer. Monitoring counts it as a server error, so one customer's script looks like an outage, and a real outage is harder to see among the limit's refusals. `429` says *you are sending too much*, and with `Retry-After`, says when to stop waiting.

</details>

### Exercise: limits for an API

An API runs on servers with **30 workers** in all. A request takes **60 ms**. Clients wait **1.5 seconds**. It has four customers: three small ones, whose peak is 40 requests a second each, and a large one, whose peak is 250. A fifth of the requests are writes; the rest are reads.

1. What is the capacity, and how busy is the service at the customers' combined peak?
2. Choose the length of the queue.
3. Choose a rate and a burst for each customer. Must the rates add up to less than the capacity?
4. A dependency becomes slow, and requests take 150 ms. What is the capacity now? With your limits from question 3 and nothing else, what happens? What must be added, and who is served?

<details>
<summary>Worked solution</summary>

**1.** 30 ÷ 0.06 = **500 a second**. The combined peak is 3 × 40 + 250 = 370: **74%**.

**2.** Longest wait = waiting ÷ 500. A queue of **15** — half the pool — is a wait of 30 ms. Even 50 would be only 100 ms, a fifteenth of the clients' patience. Start at 15, and lengthen it only if the service is seen refusing requests while its workers are not all busy.

**3.** A limit should not be reached in normal use, so it goes above the peak. Small customers: **80 a second**, twice their peak. The large one: **400**. Bursts of about a quarter of a second's worth: **20** and **100**.

The rates add up to 640, which is more than 500. That is intended. A limit for each client is there to stop *one* client from taking everything, and the chance of all four being at their limit together is small. If they are, the next mechanism deals with it. Rates that added up to less than the capacity would mean refusing customers while workers stood idle.

**4.** 30 ÷ 0.15 = **200 a second**, less than the peak of 370. The rate limits are no help: nobody has exceeded theirs, and they allow up to 640. With nothing else, this is Part 1's first row: the queue grows, and within seconds nobody is answered.

Add a limit on requests in progress — 30 working and 15 waiting — and refuse the rest with `503`. The service then finishes its 200 a second.

Add priority, to choose which 200. Writes are a fifth of 370: 74 a second. Let a read in only when fewer than 5 are waiting, and a write when fewer than 15 are. All **74 writes** are served, and the remaining **126** places go to reads, of which there are 296: about two in five.

Tell the customers what to expect: `503` with `Retry-After` for the reads that are refused. And check that their clients do not retry at once (Chapter 6.2), or 370 a second will soon be far more.

</details>

---

## Production tips and common mistakes

- **Load-test past the limit.** Amazon's article: "if they haven't load tested their service to the point where it breaks, and far beyond the point where it breaks, they should assume that the service will fail in the least desirable way possible" ([AWS Builders' Library](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/)). The curve you want is Part 1's last row: flat.
- **Limit requests in progress in every service**, and keep its queue short. It is the one protection that needs no knowledge of who is calling.
- **Give each client a rate and a burst**, publish them, and return `429` with `Retry-After`.
- **Give requests a priority at the edge**, with three or four levels, and pass it down.
- **Refuse as early as you can**: before parsing, before authenticating, before opening a transaction.
- **Count refusals separately from errors**, by reason and by client. A refusal is the system working.
- **Introduce limits in two steps**: first count what they would refuse, then enforce. Keep a switch that turns each one off.
- **Leave health checks out of the shedding.** A service that refuses its balancer's check because it is busy is taken out of service, and its load goes to the others (Chapter 6.1).

**Common mistakes**

- **No limit at all.** Go's HTTP server accepts connections by "creating a new service goroutine for each" ([Go documentation, *net/http*](https://pkg.go.dev/net/http)), however many there are. It has no limit unless you add one, and will accept work until the machine runs out of memory.
- **A long queue, for safety.**
- **Dropping expired requests, and stopping there**: the second row of Part 1.
- **A rate limit as the only protection**, set from a load test that is two years old.
- **A shared limiter that fails closed**, so that a Redis outage becomes an API outage.
- **`503` for a customer's limit, and `429` for the service's overload.**
- **A limiter that is slower than the request it refuses.**

---

## Key takeaways

- A little too much traffic, queued, becomes a total failure: every request waits behind the excess. Goodput, not throughput, is what to measure.
- Refuse early. A short queue and a quick *no* give the same goodput as serving requests late, with a fifth of the delay, and tell the unlucky at once.
- Dropping what has expired is not enough. A request must be dropped when it can no longer be *finished* in time.
- A token bucket for each client keeps one client from taking the others' share. Its rate is the sustained allowance, and its burst is the momentary one.
- When everything is too much, shed by priority, with few levels, set at the edge.
- A limit on the rate is a guess about capacity. A limit on requests in progress follows the capacity when it changes. Use the first for fairness, and the second for survival.

**Next:** 6.5 Redundancy and failure domains *(planned)* — three copies that fail together are one copy: zones, regions, correlated failure, and how much of the system one fault may reach.

---

## Sources

- Alejandro Forero Cuervo, [*Handling Overload*](https://sre.google/sre-book/handling-overload/), and Mike Ulrich, [*Addressing Cascading Failures*](https://sre.google/sre-book/addressing-cascading-failures/), chapters 21 and 22 of *Site Reliability Engineering*, Google, 2016.
- Paul Tarjan, [*Scaling your API with rate limiters*](https://stripe.com/blog/rate-limiters), Stripe, 2017.
- AWS Builders' Library, [*Using load shedding to avoid overload*](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/).
- Netflix, [*concurrency-limits*](https://raw.githubusercontent.com/Netflix/concurrency-limits/main/README.md), the project's README.
- Go documentation, [*golang.org/x/time/rate*](https://pkg.go.dev/golang.org/x/time/rate).
- [RFC 6585](https://www.rfc-editor.org/rfc/rfc6585), *Additional HTTP Status Codes*, 2012; [RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html), *HTTP Semantics*, 2022.
