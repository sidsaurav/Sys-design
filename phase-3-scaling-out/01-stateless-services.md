# 3.1 Scaling up vs scaling out: stateless services

[Syllabus](../SYLLABUS.md) · Phase 3 · Previous: [Capstone 2: ticket booking](../phase-2-data/capstone-ticket-booking.md) · Next: 3.2 Load balancing *(planned)*

> **Time:** about 35 minutes to read, 40 minutes of practice.
> **You need:** Phase 1 — especially [Chapter 1.6](../phase-1-foundations/06-concurrency-pools-queues.md) — and [Chapter 2.8](../phase-2-data/08-choosing-a-datastore.md) for object storage. The example is plain Go and needs nothing else.

**By the end of this chapter you can**

- compare the two ways of getting more capacity — a bigger machine, or more machines — and say what each costs;
- find the state hiding in an application that looks stateless;
- say where each kind of state should live once there is more than one instance;
- explain what sticky sessions are, and why they postpone the problem and do not solve it;
- choose between sessions kept on the server and sessions carried in a signed token;
- say what a stateless service lets you do that a stateful one cannot.

---

## The problem

*An illustrative scenario; the example below reproduces every part of it.* A shop runs on one server. Traffic grows, so on Friday a second server is added, with a load balancer in front sending requests to each in turn. Nothing else is changed. Both servers are healthy; every test passes.

By Saturday, support has a queue of complaints:

- "I keep getting logged out." Every other click, in fact.
- "My new profile picture shows up only half the time."
- And one that nobody reports: the API's rate limit of five requests a minute has quietly become ten.

No server crashed and no code changed. The application had always kept things in its own memory and on its own disk — who is logged in, which files were uploaded, how many requests each client has made — and with one server that was invisible, because the next request always came back to the same place. With two, it is a coin toss.

Going from one server to several is the first step of scaling, and this is what makes it hard. It is rarely the load balancer. It is everything the application remembers.

---

## The concept

### 1. Two ways to get more capacity

**Scaling up** (vertical) is replacing the machine with a bigger one. **Scaling out** (horizontal) is adding machines and spreading the work.

| | Scaling up | Scaling out |
|---|---|---|
| Changes to the application | None | It must not depend on which machine a request reaches |
| Ceiling | The largest machine you can buy | In principle, none — for the parts that can be spread |
| When a machine fails | Everything is down | The others carry on |
| Adding capacity | A restart on new hardware | Start another instance |
| New problems | None | Load balancing, shared state, partial failure |

Scaling up goes further than its reputation. A single general-purpose cloud machine can have "up to 192 vCPUs and 768 GiB memory" ([AWS, M7i instances](https://aws.amazon.com/ec2/instance-types/m7i/)). And Stack Overflow published what it took to run its sites in 2016: on one sample day, "209,420,973 … HTTP requests to our load balancer" — about 2,400 a second — handled by nine web servers and four database servers, the main database pair having "384GB of RAM … and 2x 12 cores" each ([Nick Craver, *Stack Overflow: The Architecture — 2016 Edition*](https://nickcraver.com/blog/2016/02/17/stack-overflow-the-architecture-2016-edition/)).

[The same post](https://nickcraver.com/blog/2016/02/17/stack-overflow-the-architecture-2016-edition/) is frank about *why* there were nine web servers: "we can run the entire Q&A network off of a single application pool on a single server", and the reasons for the rest are "rolling builds, headroom, and redundancy". That is the lesson worth keeping. **The first reason to have more than one server is usually not capacity. It is that one server is one failure, or one deploy, away from being zero servers.**

So the usual shape, and Stack Overflow's, is:

- **Scale the application tier out** early — at least two instances — for redundancy and for deploys without downtime.
- **Scale the database up** for as long as you reasonably can, because spreading data over machines is far harder than spreading requests (Phase 2 was the case for one database; Chapters 3.6 and 5.3 are what comes after).

Both depend on the application tier being able to run as several interchangeable copies. That is what the rest of this chapter is about.

### 2. State: what a request leaves behind

**State** is anything one request leaves behind that a later request will need. With one instance, the later request always finds it. With several, it finds it only if it happens to reach the same instance.

It hides in more places than the session:

| State | Typical code | With several instances |
|---|---|---|
| **Who is logged in** | A map of sessions in memory | Logged out on every request that reaches another instance |
| **Uploaded files** | Written to the local disk | Found only on the instance that received the upload |
| **Counters** — rate limits, quotas | A number in memory | Each instance counts separately: the limit is multiplied by the number of instances |
| **Locks** | A mutex: *one export at a time* | Protects nothing: each instance has its own mutex |
| **Scheduled jobs** | A timer inside the application | Runs once *per instance*: four instances, four digest emails |
| **Background work** | A queue in memory, a goroutine or thread | Lost when that instance restarts; the user who polls for the result asks an instance that never heard of it |
| **Caches** | A map in memory | Each instance has its own copy, and they disagree for a while |
| **Open connections** | WebSockets | Tied to one instance by nature (Chapter 3.7) |

The third and fourth rows deserve a second look, because they fail *silently*. A broken session produces complaints within the hour. A rate limit that has doubled, or a "one at a time" rule that is no longer enforced, produces nothing — until the day it matters.

### 3. Sticky sessions: the tempting fix

Load balancers offer a way to leave the application as it is. In AWS's words: "By default, an Application Load Balancer routes each request independently to a registered target based on the chosen load-balancing algorithm. However, you can use the sticky session feature (also known as session affinity) to enable the load balancer to bind a user's session to a specific target" ([AWS docs](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/edit-target-group-attributes.html)). The load balancer sets a cookie of its own and sends each browser back to the instance it met first.

The logins work again. Four things do not:

- **Losing an instance loses its sessions.** Everyone pinned to it is logged out, and has lost whatever else it was holding for them. Every deploy does this to every user.
- **State that is shared *between* users is still split.** Ana's upload is on her instance; Ben, pinned to another, cannot see it.
- **Load becomes uneven.** Requests follow users, and users are not equally busy. A new instance receives only new visitors; an old one keeps its heavy users.
- **You can no longer treat instances as interchangeable**, which is the property the next two chapters and most of Phase 9 depend on.

The Twelve-Factor App — a set of rules for building services, written by people who built the Heroku hosting platform — is blunt: "Sticky sessions are a violation of twelve-factor and should never be used or relied upon" ([*VI. Processes*](https://12factor.net/processes)). They have a place as a bridge while state is being moved out, and for connections that are stateful by nature (Chapter 3.7). As a design they only postpone the work.

### 4. Where state should live

The same page gives the alternative in one sentence: "Twelve-factor processes are stateless and share-nothing. Any data that needs to persist must be stored in a stateful backing service, typically a database" ([12factor.net](https://12factor.net/processes)).

*Stateless* is a slightly misleading word. The application as a whole still has all its state. What changes is that **no instance holds any of it between requests**: each request brings what identifies it (a cookie, a token), and everything else is fetched from somewhere all instances can reach.

| State | Move it to | Notes |
|---|---|---|
| Sessions | A shared store with expiry — or a signed token (section 5) | "Session state data is a good candidate for a datastore that offers time-expiration, such as Memcached or Redis" ([12factor.net](https://12factor.net/processes)) |
| Files | Object storage, with the key in a database row | Chapter 2.8 |
| Counters and limits | A shared store with an atomic increment | Or accept an approximate limit: give each of *N* instances a limit of 1/*N* |
| Locks | The database: a row lock, a unique constraint | Chapter 2.4. The database is the one place every instance already shares |
| Scheduled jobs | One scheduler; or every instance tries and a database lock picks one | Chapter 2.4's `SKIP LOCKED` queue does exactly this |
| Background work | A durable queue | Chapter 4.2 |
| Caches | Leave them per instance if a little disagreement is acceptable; otherwise a shared cache | Chapter 3.3. A cache is a copy, never the truth |

The filesystem and memory of an instance are still useful — within one request. As a place to keep anything for the next request, [the same page](https://12factor.net/processes) says, the application "never assumes that anything cached in memory or on disk will be available on a future request or job".

### 5. Sessions: a store, or a signature

HTTP has no memory of its own — "HTTP is a stateless protocol … where each request and response pair is independent of other web interactions" ([OWASP, *Session Management Cheat Sheet*](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)) — so a "logged-in user" is always something built on top. There are two ways to build it that work with many instances.

**A session ID, and a shared store.** The cookie holds a long random identifier and nothing else — it "must be meaningless", in OWASP's words — and each request looks it up in a store that every instance can reach.

- It costs one more network round trip on every request, and the store becomes something the whole application depends on.
- In return the server is in control: delete the entry, and that session is over, immediately.

**A signed token.** The cookie holds the facts themselves — *this is user 42, until 15:00* — plus a signature made with a key that only the servers know. Any instance can check the signature without asking anyone. JSON Web Tokens (JWTs) are the common format.

- No store, no lookup, no dependency.
- But the server has given away control. The token is valid until it expires, wherever it is and whoever holds it. OWASP: "if you use JWTs for user sessions, you will need a solution for managing session invalidation. This can be achieved using a deny list of revoked sessions/tokens. If your application implements such a deny list, user sessions won't be completely stateless anymore which might defeat the benefits of stateless sessions" ([*JSON Web Token Cheat Sheet*](https://cheatsheetseries.owasp.org/cheatsheets/JSON_Web_Token_Cheat_Sheet.html)).
- And whatever is in it — a role, a plan — stays as it was when the token was issued. Remove someone's admin rights, and their token still says admin until it expires.

The example shows this from both sides: the signed cookie survives the loss of an instance with no store at all, and also survives the user's own logout.

A common middle course is a signed token that lives for minutes, renewed with the help of a longer-lived session that *is* checked against a store. Chapter 8.2 returns to it. For a product's own web sessions, the plain session ID in a shared store is the simpler and safer default — which is [OWASP's advice](https://cheatsheetseries.owasp.org/cheatsheets/JSON_Web_Token_Cheat_Sheet.html) too: "You might want to consider using a plain session system".

### 6. What statelessness buys

Once no instance holds anything that another could not fetch, instances become **disposable** — the Twelve-Factor word: "they can be started or stopped at a moment's notice. This facilitates fast elastic scaling, rapid deployment of code or config changes, and robustness of production deploys" ([*IX. Disposability*](https://12factor.net/disposability)).

Concretely:

- **An instance dies**, and the only users who notice are those whose request was in flight (Chapter 3.2 deals with those).
- **A new instance** is useful from its first request. Adding capacity is starting a process (Chapter 9.7).
- **A deploy** replaces instances one at a time while the service keeps running (Chapters 2.7 and 9.2).

And one thing it does not buy. The state has not gone away; it has moved into the database, the session store and the object store, and *those* are now where scaling is hard. Making the application tier stateless is what turns "scaling the system" into "scaling the data" — the subject of Phase 2, the rest of this phase, and Phase 5.

---

## A real example

### Run it: one application, four places to keep its state

[`code/ch01-stateless/main.go`](code/ch01-stateless/main.go) is a small web application that remembers three things — who is logged in, how many requests each API client has made, and uploaded pictures. The program starts two instances of it behind a round-robin load balancer, all inside one process, and plays the same story four times: with the state in each instance's memory; the same with sticky sessions; with the state in a store shared by both instances; and with the session in a signed cookie.

```bash
go run ./phase-3-scaling-out/code/ch01-stateless
```

```text
2 instances of the application behind a load balancer. Where the state is kept:

                                                              in each instance          in each instance, sticky  in a shared store         signed cookie + store
Ana logs in, then views 12 pages: pages that know her         6 of 12                   12 of 12                  12 of 12                  12 of 12
A client limited to 5 requests makes 20: allowed              10 of 20                  5 of 20                   5 of 20                   5 of 20
Ana uploads a picture; Ben loads it 12 times: found           6 of 12                   0 of 12                   12 of 12                  12 of 12
Ben logs out; his old cookie is replayed 12 times: accepted   0 of 12                   0 of 12                   0 of 12                   12 of 12
The instance that logged Ana in stops; 12 pages: know her     0 of 12                   0 of 12                   12 of 12                  12 of 12
```

Read it a column at a time.

**In each instance.** This is Saturday's support queue. Ana is known on six pages out of twelve — the six that reach the instance where she logged in. The rate limit of five lets ten requests through: each instance counts to five on its own. Ben sees Ana's picture half the time. And when the instance that logged her in stops, she is logged out everywhere.

**Sticky sessions.** The first two rows are repaired: Ana always reaches her instance, and so does the API client. But Ben never sees Ana's picture now — he is pinned to the other instance, permanently. And the last row is unchanged: her instance stops, and so does her session. Stickiness made the symptoms rarer. The state is exactly where it was.

**A shared store.** Every row is right. Either instance can serve any request, so losing one costs nothing.

**A signed cookie.** The same, with one difference in the fourth row. After Ben logs out, a copy of his old cookie is still accepted, twelve times out of twelve: the server never had a record of the session, so it has nothing to delete. All it can do is ask the browser to forget. This is the price of section 5, and whether it is acceptable depends on what a stolen cookie could do.

Run it again with `-instances 4`. The first column gets worse in proportion — three pages in twelve, and all twenty requests allowed — and it grows a new symptom: Ben's logout reaches an instance that has never heard of him, so his session survives on the instance that has. With state in memory, even logging out is unreliable.

The code that makes the difference is small. The application asks for its state through one interface:

```go
type state interface {
	get(key string) (value string, ok bool)
	set(key, value string)
	del(key string)
	incr(key string) int // add one, return the new count
}
```

and the only thing that changes between the first column and the third is which implementation each instance is given — a map of its own, or a client for a store they all share. That is usually what the real migration looks like too: the hard part is finding every place the application remembers something, not changing it.

---

## Trade-offs

**Scaling up is simpler; scaling out is safer.** One big machine needs no load balancer and has no shared-state problems — and when it fails, or is being deployed to, there is nothing else. Two modest machines cost you this chapter's work and give you redundancy. For the application tier, pay it early. For the database, put it off (Phase 2).

**A shared store is a new dependency on every request.** Sessions in Redis mean that if Redis is unreachable, nobody is logged in. It needs the same care as the database: a replica, monitoring, timeouts (Chapter 3.4). A signed token removes the dependency and takes away your ability to end a session.

**Exact shared counters cost a round trip; approximate local ones are free.** A rate limit that must be exact needs a shared counter. One that only needs to be "about right" can stay in memory, divided by the number of instances. Know which kind you have.

**Per-instance caches are fast and inconsistent; shared caches are consistent and one hop away.** Chapter 3.3 is about this choice.

**Sticky sessions buy time, and time is sometimes what you need.** Turning them on for a week while sessions are moved to a shared store is reasonable. Building on them is not.

---

## Practice

### Check yourself

Answer before reading on.

1. A service runs on three instances. A limit of *100 requests a minute per API key* is implemented with a counter in each instance's memory. What limit does a client actually get? Give two ways to fix it, and what each costs.
2. Sessions are in memory, and the load balancer uses sticky sessions. Why does every deploy still log users out — and what else is lost with them?
3. A team moves to signed tokens that last 30 days, so as not to need a session store. Name three things they can no longer do.

<details>
<summary>Answers</summary>

1. **About 300 a minute**: each instance counts to 100 by itself, and round-robin spreads the client's requests across all three. (The number also changes whenever instances are added or removed, so the limit is not even stable.) Fix one: keep the counter in a shared store with an atomic increment — exact, at the price of a network round trip on every request and a dependency on that store. Fix two: keep the counters local and give each instance a limit of 100 ÷ 3 — free and independent, but only approximate, and wrong whenever traffic is unevenly spread.

2. Stickiness decides *where a request goes*; it does not change *where the session is*. A deploy replaces each instance with a new process, whose memory is empty. Every user pinned to it arrives at an instance that has never seen their session. Lost with it: anything else that instance held for them — a cart, a half-finished upload, a background job they were waiting on, their rate-limit history.

3. Any three of: log a user out, in the sense of making the token stop working; end a session that has been stolen; lock out a user who has been banned or has left the company, before the token expires; make a change of role or plan take effect immediately, since the token carries the old one; see who is currently logged in; limit a user to one session. Each can be bought back — with a denylist, or short-lived tokens renewed against a store — and each of those is a session store by another name.

</details>

### Exercise: find the hidden state

*Notely* is a note-taking product that has run on one server for two years. It is about to move to four instances behind a load balancer. From the code review, this is how it works today:

1. **Login**: sessions are a `map[string]User` in memory.
2. **Attachments**: files are saved under `/var/app/uploads`.
3. **Export**: *export all my notes as a PDF* starts a background thread, writes the result to `/tmp`, and the browser polls `/export/status` until it is ready.
4. **Rate limit**: 60 requests a minute per user, a token bucket in memory.
5. **Daily digest**: a timer in the application fires at 08:00 and emails every user.
6. **Feature flags**: read from the database into memory at start-up; the admin page updates the database *and* the copy in memory.
7. **Imports**: *only one import at a time per workspace*, enforced with a mutex per workspace.
8. **Presence**: *3 people are viewing this note* comes from a set of user IDs in memory, per note.

For each: what goes wrong with four instances, how you would fix it, and how urgent it is.

<details>
<summary>Worked solution</summary>

| # | What goes wrong with four instances | Fix | Urgency |
|---|---|---|---|
| 1. Login | Users are logged out on three requests in four | Session IDs in a shared store with expiry | **Blocks the launch** |
| 2. Attachments | A file exists on one instance in four; the others return "not found". Lost for good when that instance is replaced | Object storage; the key in a database row (Chapter 2.8). Copy the existing files across first | **Blocks the launch**, and risks data loss |
| 3. Export | The job runs on one instance; three polls in four ask an instance that knows nothing about it. A deploy kills the job silently | Record the job in a database table; a worker picks it up (Chapter 2.4, `SKIP LOCKED`); the result goes to object storage; the status endpoint reads the table. Later, a real queue (Chapter 4.2) | **Blocks the launch** |
| 4. Rate limit | The limit becomes 240 a minute | A counter in the shared store — or 15 a minute per instance, if "roughly 60" is good enough | Not urgent, but invisible. Decide deliberately |
| 5. Daily digest | **Every user receives four emails** | One scheduler outside the application; or each instance tries to insert a row for *today's digest* under a unique constraint, and only the one that succeeds sends (Chapter 2.4) | **Blocks the launch** — the most visible failure on the list |
| 6. Feature flags | The admin's change reaches the database and *one* instance. The other three keep the old value until they restart. Users see a feature flicker on and off | Treat the in-memory copy as a cache: reload from the database every few seconds on every instance. A change takes effect everywhere within that interval | Important. Confusing rather than damaging |
| 7. Imports | Four imports can run at once in one workspace, one per instance — each instance has its own mutex | A lock in the database: a row per workspace claimed with a conditional `UPDATE`, as in Capstone 2 | Depends on what concurrent imports damage. If it is data, **blocks the launch** |
| 8. Presence | Each instance knows only its own viewers: the count is wrong, and differs from one refresh to the next | A shared store with expiring entries; or accept an approximate count. Real-time presence is really Chapter 3.7's problem | Cosmetic. Fix later |

Three things to notice about the answer.

- **The list is the hard part.** None of the fixes is difficult. Finding all eight was: three of them — the digest, the flags, the mutex — are not what anyone means by "session state", and a review that only asked "where are the sessions?" would have shipped with all three broken.
- **The failures that hurt most are not the obvious ones.** Being logged out is noticed in minutes and fixed in a day. Four emails to every customer, or four imports corrupting a workspace, are the ones that reach a postmortem.
- **Sticky sessions would have hidden items 1, 3 and 4** well enough to pass a demo — and left 2, 5, 6, 7 and 8 exactly as broken.

</details>

---

## Production tips and common mistakes

**Tips**

- **Run at least two instances from early on**, even when one would carry the load. It finds the hidden state while the system is small, and it makes deploys and failures routine.
- **Look for state by asking what a request leaves behind**, not by searching for the word "session": files, counters, mutexes, timers, background threads, caches.
- **Keep session IDs meaningless and random, in a shared store with an expiry.** Reach for signed tokens when you have a reason, and decide how you will revoke them before you ship.
- **Put files in object storage**, never on an instance's disk.
- **Use the database for locks, and whenever exactly one instance must do something**: it is the one thing every instance already shares.
- **Make instances disposable**: quick to start, and able to stop cleanly at any moment.
- **Test by killing an instance** during normal use, and by deploying in the middle of the day. If either is frightening, state is hiding somewhere.

**Common mistakes**

- **Turning on sticky sessions and calling the application stateless.**
- **Scheduled jobs inside the application**, running once per instance.
- **An in-memory mutex guarding something shared.** It guards one instance's share.
- **Rate limits and quotas counted per instance**, silently multiplied by the number of instances.
- **Uploads on local disk**, lost with the instance.
- **Long-lived signed tokens with no way to revoke them.**
- **An in-memory cache updated by the request that changed the data** — on one instance out of many.
- **Scaling out an application whose database is the bottleneck.** More instances make more connections and more queries; the database gets no faster (Chapter 2.6).

---

## Key takeaways

- There are two ways to add capacity: a bigger machine, or more machines. Scaling up is simpler and goes a long way; scaling out is what gives redundancy and deploys without downtime.
- The usual shape is to scale the application tier out early and the database up for as long as possible.
- Scaling out requires that any instance can serve any request. What stops it is state: anything a request leaves behind for a later one.
- State hides in sessions, files, counters, locks, timers, background work and caches. Several of these fail silently.
- Sticky sessions hide the problem for some of that state, and leave instances impossible to lose or replace quietly.
- Move state to something every instance shares: a session store, object storage, the database, a queue.
- A session can be an ID looked up in a store, which the server can end at any moment, or a signed token, which needs no store and cannot be taken back.
- Stateless instances are disposable. The hard scaling problem that remains is the data.

**Next:** 3.2 Load balancing *(planned)* — how requests are spread over instances, how a dead one is noticed, and how a deploy avoids dropping the requests in flight.

---

## Sources

- Adam Wiggins, *The Twelve-Factor App*: [VI. Processes](https://12factor.net/processes) and [IX. Disposability](https://12factor.net/disposability).
- Nick Craver, [*Stack Overflow: The Architecture — 2016 Edition*](https://nickcraver.com/blog/2016/02/17/stack-overflow-the-architecture-2016-edition/), 2016.
- AWS documentation: [Application Load Balancer target group attributes — sticky sessions](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/edit-target-group-attributes.html); [Amazon EC2 M7i instances](https://aws.amazon.com/ec2/instance-types/m7i/).
- OWASP Cheat Sheet Series: [*Session Management*](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html); [*JSON Web Token*](https://cheatsheetseries.owasp.org/cheatsheets/JSON_Web_Token_Cheat_Sheet.html).
