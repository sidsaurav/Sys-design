# 3.4 Cache failure modes

[Syllabus](../SYLLABUS.md) · Phase 3 · Previous: [3.3 Caching patterns](03-caching-patterns.md) · Next: 3.5 HTTP caching and CDNs *(planned)*

> **Time:** about 45 minutes to read, 45 minutes of practice.
> **You need:** [Chapter 3.3](03-caching-patterns.md), and queues and overload from [Chapter 1.6](../phase-1-foundations/06-concurrency-pools-queues.md). The example is plain Go.

**By the end of this chapter you can**

- explain why adding a cache creates a new way for the whole system to fail;
- describe a cache stampede and stop one three ways: one query per key, serving stale data, and staggering expiry;
- say what happens when an entire cache is lost, and how to prepare for it;
- handle requests for things that do not exist;
- size a cache, choose an eviction policy, and know what to watch;
- list the settings that matter when running Redis as a cache.

---

## The problem

*An illustrative scenario; the example reproduces its mechanics.* The shop from Chapter 3.3 has had its cache for a year. The hit ratio is 95%, the database idles at a fifth of its capacity, and the team has stopped thinking about it. To save money, the database was moved to a smaller machine: it only sees the misses, after all.

One afternoon the cache server is restarted for a routine upgrade. It comes back in seconds — empty. Every read is now a miss. The database, sized for a twentieth of the traffic, receives all of it. Queries queue; requests time out; clients retry, adding load; the entries that would relieve the pressure cannot be computed because the database is too busy to answer. The upgrade took ten seconds. The outage takes forty minutes.

Amazon's engineers have a phrase for a system in this condition: "a service that has become addicted to its cache. The cache has been inadvertently elevated from a helpful addition to the service to a necessary and critical part of its ability to operate" ([AWS Builders' Library, *Caching challenges and strategies*](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/)).

Chapter 3.3 was about the cache giving wrong answers. This chapter is about the cache giving no answers — or the wrong load.

---

## The concept

### 1. A cache changes what "normal" means

Before the cache, the database handled every read, and you knew it could. After a year with a 95% hit ratio, nobody knows whether it still can — and usually it cannot, because capacity has been removed, traffic has grown twentyfold behind the cache's protection, or both.

The same article names the underlying issue: "the modal behavior introduced by the cache, with differing behavior depending on whether a given object is cached" ([AWS](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/)). The system has two modes — cache warm, cache cold — and only one of them is ever tested, because the other does not occur until the day it does.

So the first question of this chapter is one to ask of any cache you run: **what happens to the thing behind it if the cache is empty right now?** If the answer is "it falls over", the cache is no longer an optimisation. It is a single point of failure that was never designed as one.

Each of the failure modes below is a way of ending up in the cold mode, partly or completely.

### 2. The stampede

A popular entry expires. In the instant before, a thousand requests a second were being answered from the cache. In the instant after, every one of them misses — and each, following the cache-aside pattern faithfully, goes to the database to recompute the same value. If the value takes 300 ms to compute, three hundred identical queries are in flight before the first one finishes.

It has several names — cache stampede, dog-piling, thundering herd. The AWS article describes it as the situation "in which many clients make requests that need the same uncached downstream resource at approximately the same time" ([AWS](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/)). Three defences, which combine:

**One query per key.** Let the first request that misses do the work, and make the others wait for its answer. The same article: "we use request coalescing, where the servers or external cache ensure that only one pending request is out for uncached resources". Inside one process this is a few lines of code — Go's is [`singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight), "a duplicate function call suppression mechanism". Across many instances, the cache itself has to arbitrate, which is the second job of the *leases* from Chapter 3.3. Facebook measured the effect on a set of keys prone to stampedes: "Without leases, all of the cache misses resulted in a peak database query rate of 17K/s. With leases, the peak database query rate was 1.3K/s" ([Nishtala et al., *Scaling Memcache at Facebook*](https://www.usenix.org/system/files/conference/nsdi13/nsdi13-final170_update.pdf)).

**Serve the old value while fetching the new one.** Coalescing removes the duplicate work, but everyone still waits for the one query. Often they need not: a value that was good enough a second ago is good enough for another few hundred milliseconds. Give each entry two lifetimes:

```text
 stored                 soft expiry                              hard expiry
   │◄────── fresh ───────►│◄──────────────── stale ─────────────────►│
   │   served from the    │  still served from the cache, while one   │  a miss
   │   cache              │  request refreshes it in the background   │
```

HTTP has had a name for this since 2010, and the name has stuck: "The stale-while-revalidate HTTP Cache-Control extension allows a cache to immediately return a stale response while it revalidates it in the background, thereby hiding latency (both in the network and on the server) from clients" ([RFC 5861](https://www.rfc-editor.org/rfc/rfc5861.html)). Facebook's paper makes the same observation about its own cache: the wait can be reduced "by identifying situations in which returning slightly out-of-date data is acceptable" ([Nishtala et al.](https://www.usenix.org/system/files/conference/nsdi13/nsdi13-final170_update.pdf)).

The two lifetimes buy something else. If the refresh *fails* — the database is down — the stale entry is still there to serve. The AWS article describes this as "a soft TTL and a hard TTL", where "if the downstream service is unavailable or otherwise doesn't respond to the request, then the existing cache data will continue to be used until the hard TTL is reached" ([AWS](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/)). A cache used this way makes the system *more* available than the database alone, and that is the opposite of the addiction in the opening.

**Do not let entries expire together.** If ten thousand entries are written in the same second — at start-up, by a batch job, after a deploy — with the same TTL, they expire in the same second. Add a random amount to each TTL (*jitter*, the same idea as Chapter 1.5's retry jitter), and the expiries spread out.

### 3. Losing the whole cache

A restart, a failover to an empty replica, a `FLUSHALL` typed into the wrong terminal, a deploy that changes the format of every key: the hit ratio goes from 95% to zero in an instant, and the database is offered twenty times its usual load.

Coalescing helps, because a cold cache produces a stampede on *every* popular key at once. Beyond that, the defences are about not getting there, and about surviving it:

- **Do not lose it.** Give the cache a replica, so that a failed node is replaced by one that already has the data. Restart nodes one at a time. Treat a flush as you would dropping a table.
- **Warm it before it takes traffic.** Facebook built a system for exactly this: "When we bring a new cluster online, an existing one fails, or perform scheduled maintenance the caches will have very poor hit rates diminishing the ability to insulate backend services. A system called Cold Cluster Warmup mitigates this" by letting the cold cluster fill itself from a warm one and not from the database ([Nishtala et al.](https://www.usenix.org/system/files/conference/nsdi13/nsdi13-final170_update.pdf)). The small-scale version: bring the new cache up beside the old one and send it a growing share of traffic.
- **Protect the database from its own callers.** Limit how many misses may query the database at once, and fail the rest quickly (Chapters 1.6 and 6.4). The cache then refills at the rate the database can sustain. Some requests fail for a while; the system recovers by itself instead of collapsing.
- **Know the number.** Measure, or work out, what the database can serve with no cache at all, and compare it with today's traffic. If the gap is large, you are in the opening scenario already, and waiting.

The same reasoning applies to a single cache *node* failing in a large cache: the keys that lived on it all miss at once. Facebook keeps a small pool of spare machines for this — "When a memcached client receives no response to its get request, the client assumes the server has failed and issues the request again to a special Gutter pool" ([same paper](https://www.usenix.org/system/files/conference/nsdi13/nsdi13-final170_update.pdf)) — so that a dead node's load lands on a cache, not on the database.

### 4. Things that do not exist

Cache-aside stores what the database returns. If the database returns nothing — the product was deleted, the URL was mistyped, a crawler is working through ID numbers — there is nothing to store, so the next request for the same thing misses again. Requests for missing things *always* reach the database. A broken link on a popular page, or a hostile client, can generate more database load than all the legitimate traffic.

**Negative caching** is storing the absence: "product 4711: not found", with a short TTL. AWS's article recommends the same for errors from a dependency — "we use a 'negative cache'" with "a different TTL than positive cache entries" — and puts the reason bluntly: "Don't cause or amplify an outage by repeatedly asking for the same downstream resource and discarding the error responses" ([AWS](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/)).

It has a limit, which the example shows. It works when the *same* missing things are asked for repeatedly. If every request invents a new key, each "not found" entry is used once and never again — the database load is unchanged, and the cache fills with junk. That traffic has to be stopped earlier: by rejecting identifiers that cannot be valid, by rate limiting (Chapter 6.4), or by a compact summary of which keys exist ([a Bloom filter](https://redis.io/docs/latest/develop/data-types/probabilistic/bloom-filter/)).

And remember to remove the negative entry when the thing is created — or keep its TTL short enough not to matter.

### 5. Hot keys

In any cache a few keys are far more popular than the rest. Two consequences:

- A stampede on a hot key is the worst kind, so hot keys are where coalescing and stale-while-revalidate matter most. Section 2 is mostly about them.
- When a cache is spread over several nodes (Chapter 5.3), one hot key lives on one node, and can overload it however many nodes there are. A common relief is a tiny cache *inside each application instance* for the hottest few keys, held for a second or two — the per-instance cache that Chapter 3.1 warned about, used on purpose, where a second of disagreement between instances is acceptable.

### 6. A cache that is full

Memory is finite, so a cache must discard entries to make room: **eviction**. What it discards is a policy. Redis's are typical ([Redis docs, *Key eviction*](https://redis.io/docs/latest/develop/reference/eviction/)):

- `allkeys-lru` — "Evict the least recently used (LRU) keys." The usual choice for a cache: what has not been asked for lately probably will not be asked for soon.
- `allkeys-lfu` — "Evict the least frequently used (LFU) keys." Better when a burst of one-off reads would otherwise push out entries that are steadily popular.
- `noeviction` — "Keys are not evicted but the server will return an error when you try to execute commands that cache new data." Right when Redis is being used as a store; wrong for a cache, which then fails every write once it is full.

Eviction is normal. A cache that never evicts is bigger than it needs to be. What matters is the hit ratio it leaves you with, and because access is skewed, a cache far smaller than the data can do most of the work — the example measures it. The warning sign is a *rising* eviction rate with a *falling* hit ratio: the cache no longer holds the popular set, and section 1's arithmetic is turning against you.

### 7. Running Redis as a cache

Redis is the usual choice of cache server. A short list of what matters in production, each from its documentation:

- **Set a memory limit and an eviction policy** (section 6). An unlimited cache grows until the operating system kills it.
- **Decide about persistence deliberately.** Redis can write to disk — "RDB persistence performs point-in-time snapshots of your dataset at specified intervals", and "AOF persistence logs every write operation received by the server" ([Redis docs, *Persistence*](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)). For a pure cache, persistence mainly buys a warm restart; for anything you cannot rebuild, see Chapter 2.9 first.
- **Run a replica, with failover.** It is what turns "the cache node died" from section 3's emergency into a non-event.
- **Never run commands that scan everything.** "Redis can serve a single request in every given moment, so all the requests are served sequentially" ([Redis docs, *Diagnosing latency issues*](https://redis.io/docs/latest/operate/oss_and_stack/management/optimization/latency/)) — so one slow command stalls every client. Of the command that lists keys, the manual says: "Use extreme care when using this command in production environments. It may ruin performance when it is executed against large databases… Don't use KEYS in your regular application code" ([Redis docs, `KEYS`](https://redis.io/docs/latest/commands/keys/)).
- **Give the client a short timeout.** A cache lookup that normally takes a millisecond should be abandoned after a few tens of milliseconds and treated as a miss (Chapter 1.5). A cache client with a five-second timeout turns a slow cache into a slow everything.
- **Watch four numbers**: hit ratio, evictions, memory used, and latency of cache calls.

---

## A real example

### Run it: four failures

[`code/ch04-stampede/main.go`](code/ch04-stampede/main.go) uses the same kind of stand-in database and cache as Chapter 3.3, with one addition: the database can run only so many queries at once, and a request that waits too long for it gives up.

```bash
go run ./phase-3-scaling-out/code/ch04-stampede
```

**Part 1: one popular entry expires.**

```text
PART 1 — one popular entry: 500 reads a second. Computing it takes 300 ms; it is cached for 1 second.
         The database runs 12 queries at once. A reader waits at most 2 seconds. Six seconds of load.

  when the entry expires...                            db queries       ok   failed       p50       p99
  every reader that misses asks the database                  192     2890      110    1.1 ms     2.0 s
  one reader asks; the others wait for its answer               4     3000        0    1.1 ms    289 ms
  one reader asks; the others get the old value                 4     3000        0    1.1 ms    1.3 ms
```

Four queries were needed in six seconds. The plain cache-aside code ran 192. Each time the entry expired, the 150 requests that arrived during the next 300 ms all went to the database; it can run twelve at a time, so the rest queued for seconds, and by the time the queue drained the entry had expired again. A hundred and ten readers gave up, and one request in a hundred took two seconds — for a value that was being recomputed, identically, 48 times over.

With one query per key, the database does the four queries it should. The readers who arrive during each one wait for it: the 99th percentile is 289 ms, the time of one computation. With stale-while-revalidate they do not wait at all: 1.3 ms. The price is that the value they see can be up to 1.3 seconds old, where before it was at most 1.

**Part 2: the cache is emptied.**

```text
PART 2 — 2,000 reads a second over the whole catalogue. A query takes 50 ms; the database runs 12 at once
         (240 a second). With a warm cache it sees about 100 a second. Two seconds in, the cache is emptied.
         A reader waits at most 1 second.

                                 failed reads in each second                                      in all   db queries
                                 ------------------------------------------------------------------------------------
  every miss asks the database      0    0  273  195   81   18    0    0    0    0    0    0        567         2199
  one query per key at a time       0    0    0    0    0    0    0    0    0    0    0    0          0         1955
```

This is the opening scenario, with a database that has more than twice the capacity it normally needs — a comfortable margin on any ordinary day. When the cache is emptied it is offered 2,000 queries a second and can do 240. For four seconds, readers at the back of its queue wait more than a second and give up: 567 of them.

Asking once per key removes the duplicate queries for popular products, and that is enough, here, for every reader to be answered in time. Look at the last column, though. Over the ten seconds after the flush the two runs asked the database for almost the same amount of work: 2,199 queries against 1,955. Coalescing removed only about 250 queries — but they were all in the first second or two, when the queue was longest, and that was the difference between waiting and giving up. The work of refilling the cache did not go away, and this database had just enough room for it. With a slower database, a bigger catalogue or flatter popularity it would not have — and then the answer is the rest of section 3: do not lose the cache, warm it first, and limit what reaches the database.

(This stand-in database is kind. It keeps serving 240 queries a second however long its queue. A real one under this kind of pressure usually slows down, and its clients retry. Real stampedes are worse than this one.)

**Part 3: things that do not exist.**

```text
PART 3 — 2,000 reads a second, a quarter of them for products that do not exist. A query takes 5 ms.
         The cache holds 5,000 entries. Five seconds of load.

  the missing products are...          "not found" is...          db queries/s hit ratio, real products  "not found" entries
  the same 50, again and again         not remembered                      565                    96%                    0
  the same 50, again and again         remembered for 30 s                  76                    96%                   50
  different every time                 not remembered                      565                    96%                    0
  different every time                 remembered for 30 s                 565                    96%                 2500
```

The real products are cached well: a 96% hit ratio in every row. But in the first row the database is doing 565 queries a second, and nearly all of them are for fifty products that do not exist. Remembering "not found" brings it down to 76.

The last two rows are the limit of the technique. When every missing ID is new, remembering them changes nothing — 565 either way — except that after five seconds half the cache is entries that will never be read again.

**Part 4: how big?**

```text
PART 4 — how big must the cache be? 200,000 reads of a 100,000-product catalogue, through caches of four sizes.
         When a cache is full it evicts the entry used least recently.

         cache holds share of the catalogue    hit ratio    evictions
         100 entries                   0.1%          75%        50856
        1000 entries                   1.0%          91%        17951
       10000 entries                  10.0%          96%            0
      100000 entries                 100.0%          96%            0
```

A cache holding one product in a thousand answers three reads in four. One in a hundred: nine in ten. Past ten thousand entries, more memory buys nothing — the remaining misses are products being read for the first time. This is what skewed access means for capacity: size the cache for the popular set, not for the data. (How skewed your own access is, only your own measurements can say.)

The heart of the one-query-per-key defence is short enough to read in full (slightly condensed here):

```go
func (s *service) loadOnce(key int, deadline time.Time) (int, error) {
	s.mu.Lock()
	if f, ok := s.flights[key]; ok { // someone is already asking: wait for their answer
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.value, f.err
		case <-time.After(time.Until(deadline)):
			return 0, errTimeout
		}
	}
	f := &flight{done: make(chan struct{})}
	s.flights[key] = f // we are the one who asks
	s.mu.Unlock()

	f.value, f.err = s.load(key, deadline)

	s.mu.Lock()
	delete(s.flights, key)
	s.mu.Unlock()
	close(f.done) // wake everyone who was waiting
	return f.value, f.err
}
```

---

## Trade-offs

**Coalescing trades a little latency for a great deal of load.** Waiters are tied to one query: if it is slow, they are all slow; if it fails, they all fail. Give the shared query a deadline, and do not let one caller's impatience cancel it for the others.

**Serving stale data trades freshness for latency and availability.** It is nearly always a good trade for content, and a bad one for anything where "a bit old" means "wrong": a balance, a permission, a price at checkout. The hard expiry is your statement of how old is too old.

**A replica and persistence for a cache cost money to protect data you could rebuild.** They are worth it exactly when rebuilding is the problem — when the database could not survive the cold start.

**Negative caching hides newly created things for its TTL**, and can be turned against you by a client inventing keys. Keep the TTL short and the entries small.

**A bigger cache has diminishing returns; a smaller one, a cliff.** Part 4's curve is flat on the right and steep on the left. Run on the flat part, and watch for drifting towards the edge as the data grows.

**Every defence here is more code on the read path.** A cache-aside read was eight lines. With coalescing, two lifetimes, negative entries and a miss limit it is fifty, with its own bugs. Use a library that has them, and add each one when a measurement asks for it.

---

## Practice

### Check yourself

Answer before reading on.

1. A home page is cached for 60 seconds and takes 2 seconds to build. It receives 300 requests a second. What happens at each expiry with plain cache-aside? Give two fixes and what users experience with each.
2. A team's cache has a 97% hit ratio. Their database, with no cache, could serve about a fifth of today's read traffic. Describe what they should do about it *before* anything goes wrong.
3. A cache server is configured with a memory limit and the policy `noeviction`. The team reports that the cache has stopped helping: the hit ratio has been falling for a week. What is happening?

<details>
<summary>Answers</summary>

1. For the two seconds after expiry, every request misses: 600 requests each start building the page. If the database can run, say, twenty such builds at once, the rest queue; builds slow each other down; many requests time out; and the entry may expire again before the backlog clears, so the stampede repeats every minute. **Fix one, coalescing:** one build; the other 599 requests wait up to two seconds for it. Correct, but every minute there is a two-second stall for 600 users. **Fix two, stale-while-revalidate:** one build in the background; everyone is served the previous page instantly. Nobody waits; the page is at most 62 seconds old instead of 60. For a home page, the second is clearly right. (A third: rebuild it on a timer every 50 seconds, so that it never expires under load at all.)

2. They are one cache restart away from an outage: the cold-mode load is five times what the database can take. Before it happens: (a) add a replica to the cache and make sure single-node restarts are rolling, so the cache is never empty; (b) add coalescing, so that a cold start costs one query per key and not one per request; (c) put a limit on concurrent misses, so that the database is never offered more than it can do and the cache refills at a survivable rate; (d) write and rehearse the procedure for a cold start — bringing traffic up gradually; (e) put *database capacity with no cache, against current traffic* on a dashboard, so that the gap is seen growing. Optionally, give the database back some capacity: a fifth is a thin margin for something every request depends on.

3. With `noeviction`, a full cache refuses new writes. Old entries stay until their TTLs expire; new, currently popular items cannot be stored at all, so reads for them always miss. The cache is slowly becoming a museum of last week's popular data. (If entries have no TTL, it is frozen entirely.) The fix is an eviction policy meant for caches — least recently used, or least frequently used — and an alert on rejected writes, which would have caught this on the first day.

</details>

### Exercise: harden a cached endpoint

`GET /products/{id}` is served by twelve application instances with a shared Redis cache in front of PostgreSQL. Today:

- cache-aside, TTL 10 minutes, no jitter; entries are deleted when a product is edited;
- hit ratio 96%; 4,000 requests a second at peak; a product query takes 8 ms;
- the database could serve about 600 of these queries a second on top of its other work;
- Redis is a single node with no replica and no memory limit;
- the Redis client's timeout is the library default of 5 seconds;
- last month a marketing email linked to a product that had been deleted, and the database CPU hit 100% for an hour.

Find the risks, and give the fix for each, in the order you would do them.

<details>
<summary>Worked solution</summary>

**The arithmetic first.** Normal load on the database is 4% of 4,000: 160 queries a second, against a capacity of 600. Cold, it is 4,000 against 600: nearly seven times too much. This service is "addicted" in the sense of the opening.

| # | Risk | Fix | Why this order |
|---|---|---|---|
| 1 | **Redis is a single node.** Its loss empties the cache and sends 4,000 queries a second at a database that can do 600. | Add a replica with automatic failover. Restart nodes one at a time. | The largest outage on the list, and the cheapest to prevent |
| 2 | **No memory limit.** The node grows until it is killed — which is risk 1 again. | Set a limit below the machine's memory, and `allkeys-lru`. | A setting. Do it the same day |
| 3 | **A 5-second client timeout.** If Redis is slow, every request waits 5 seconds before even trying the database. Twelve instances' worth of request handlers fill up, and the site is down while the database sits idle. | Timeout of 20–50 ms; on timeout or error, treat it as a miss. | A slow cache is likelier than a dead one |
| 4 | **Misses are unlimited.** After a cold start, or when Redis is unreachable and every read is a miss, all traffic reaches the database. | Coalesce per key in each instance. And cap what this endpoint may ask of the database at about 600 queries a second in total, refusing the rest at once with a 503. At 8 ms a query that is only five in flight across the *whole* fleet, so a per-instance cap is too coarse: give the endpoint its own database role behind a pooler with five connections (Chapter 2.6), or a rate limit of 50 a second per instance (Chapter 6.4). | Turns a collapse into a brief period of partial errors, during which the cache refills |
| 5 | **The deleted product.** Every request for it reached the database: nothing to cache. | Negative caching: remember "not found" for 30–60 seconds. | It has already caused an incident |
| 6 | **Stampedes on popular products** at each ten-minute expiry. With an 8 ms query they are small today; they will grow with the query's cost. | Serve stale for up to a minute past expiry while one request refreshes (coalescing from fix 4 does the rest). | Cheap once 4 is in place |
| 7 | **No jitter.** Entries loaded together — after a restart, or a bulk import — expire together, ten minutes later, and again ten minutes after that. | TTL of 10 minutes ± 1 minute, chosen at random per entry. | One line |
| 8 | **Nobody is watching.** | Dashboards and alerts for hit ratio, evictions, Redis memory, cache-call latency, and the number of requests refused by the limit in fix 4. | So that the next problem is seen before it is an incident |

**Then test it.** In a staging environment under load, empty the cache and watch. The target is not "nothing happens". It is: some requests fail fast for a few seconds; database load rises to its limit and no further; the hit ratio climbs back; and nobody has to do anything. If that is what you see, the cache has gone back to being an optimisation.

</details>

---

## Production tips and common mistakes

**Tips**

- **Know your cold-mode load**: what the database would receive with an empty cache, against what it can serve.
- **One query per key** on every cache miss path that is expensive or popular.
- **Two lifetimes**: a soft expiry that triggers a refresh, and a hard one that ends the entry.
- **Add jitter to TTLs.**
- **Cache "not found"**, briefly.
- **Limit concurrent misses**, so that the database is protected even from a completely cold cache.
- **Give the cache a memory limit, an eviction policy and a replica.**
- **Short timeouts on cache calls**; a failed cache call is a miss, not an error.
- **Rehearse a cold start** before one happens to you.
- **Watch hit ratio, evictions, memory and cache latency.**

**Common mistakes**

- **Shrinking the database because the cache makes it look idle.**
- **Restarting or flushing the cache at a busy time**, or all nodes at once.
- **Deploying a change to the key format** — an instant, total cold start that nobody thought of as one.
- **The same TTL on everything**, written at the same moment.
- **Retrying failed misses immediately**, multiplying the load on a database that is already struggling.
- **Treating a cache error as a request error**, so that a cache outage is a site outage even where the database could have coped.
- **`noeviction` on a cache**, or no memory limit at all.
- **Commands that scan the whole keyspace** on a production cache.
- **Negative entries with long TTLs**, hiding things that now exist.
- **Never having seen the system run with a cold cache.**

---

## Key takeaways

- A cache hides load from the database. Lose the cache and the load returns at once, usually to a database that can no longer take it.
- A stampede is many requests recomputing one expired entry. Stop it by letting one request do the work, by serving the old value meanwhile, and by staggering expiries.
- Two lifetimes — refresh after the first, discard after the second — remove the latency spike at expiry and keep the system answering when the database is down.
- Prepare for a cold cache: replicate it, warm it, limit what misses may do to the database, and rehearse.
- Things that do not exist are never cached unless you cache their absence. That helps only when the same absences recur.
- Size a cache for the popular set. Give it a limit and a least-recently-used policy, and watch hit ratio and evictions together.

**Next:** 3.5 HTTP caching and CDNs *(planned)* — the caches you do not run: browsers, proxies and the edge.

---

## Sources

- AWS Builders' Library: [*Caching challenges and strategies*](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/).
- Rajesh Nishtala et al., [*Scaling Memcache at Facebook*](https://www.usenix.org/system/files/conference/nsdi13/nsdi13-final170_update.pdf), NSDI 2013 (leases, Gutter, Cold Cluster Warmup).
- Mark Nottingham, [RFC 5861: *HTTP Cache-Control Extensions for Stale Content*](https://www.rfc-editor.org/rfc/rfc5861.html), 2010.
- Redis documentation: [*Key eviction*](https://redis.io/docs/latest/develop/reference/eviction/); [*Persistence*](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/); [`KEYS`](https://redis.io/docs/latest/commands/keys/); [*Bloom filter*](https://redis.io/docs/latest/develop/data-types/probabilistic/bloom-filter/).
- Go: [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight).
