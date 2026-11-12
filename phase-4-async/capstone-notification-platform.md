# Capstone 4: a notification platform — once, in time, and only if wanted

[Syllabus](../SYLLABUS.md) · Phase 4 · Previous: [4.7 Event-driven design](07-event-driven-design.md)

> **Time:** three hours for your own design, then an hour with the worked design.
> **You need:** all of Phase 4; idempotency (1.5), check-and-change statements (2.4) and pools (2.6) from earlier phases. The reference implementation needs the PostgreSQL database of Phase 2.

Capstone 2 was about a few rows that everyone wanted to change. Capstone 3 was about reads that should never reach the database. This one is about work that happens after the user has been answered: a platform that other teams ask to *tell a user something*, and that must then get a message out through somebody else's email, SMS and push services — which have limits, fail, and cost money.

Every chapter of the phase is in it. Work accepted and done later (4.1). A queue with leases, retries and a place for what cannot be sent (4.2). Deliveries that must not happen twice, through providers that may or may not help with that (4.3). A decision between a queue and a log (4.4). Accepting a request without a dual write (4.5). A marketing campaign that must not delay a login code, and a provider that is down for an hour (4.6). And events about notifications, for other teams to read (4.7).

**How to use this page**

1. Read the brief.
2. Write your own design, on paper, against the task list. Give yourself a time limit.
3. Only then open the worked design and compare, section by section.
4. Run the reference implementation — and then run it with each safeguard removed.

---

## The brief

*From your product manager:*

> Every team that needs to contact a user has built its own way of doing it. We want one platform.
>
> **What it must do**
> - A team sends one request: *tell user U about X*. The request has a class.
>   - **Login codes**, by text message. A code is valid for 10 minutes and should arrive within 10 seconds.
>   - **Transactional** messages — an order has shipped, a payment failed — by email and by push notification. Within a minute.
>   - **Marketing**, by email. Within a few hours.
> - Users can opt out of marketing. **An opt-out takes effect at once**, including for campaigns already under way.
>
> **How much**
> - 30 million notifications a day: 2 million codes, 10 million transactional, 18 million marketing. Transactional traffic peaks at three times its average.
> - Marketing arrives as campaigns: up to **5 million emails queued in one request**, a few times a week.
>
> **The providers** — all outside our control
> - Email: accepts 1,000 a second. Accepts an idempotency key.
> - Text messages: 100 a second. **Each one costs money. No idempotency key.**
> - Push: 5,000 a second. Accepts an idempotency key.
> - Each fails for a few minutes most weeks, and for an hour once or twice a year. Each refuses requests above its limit, and may suspend an account that keeps exceeding it.
>
> **How well**
> - A team that retries its request must not cause a second notification.
> - **No message is sent twice** on a channel — as far as that can be guaranteed. Say where it cannot.
> - **An expired code is never sent.**
> - **Nothing accepted is silently lost.** Every delivery ends as sent, suppressed, expired or failed, and we can say which, for 90 days.
> - A campaign must not delay a login code or an order update.
>
> **Constraints**
> - PostgreSQL is available. You may add a broker or a log, with a reason.
>
> **Six months after launch**
> - A second text-message provider, for when the first is down.
> - An option for users to receive one email an hour that summarises their notifications.

---

## Your task

Produce a design document of three or four pages. The suggested times add up to about three hours.

| # | Deliverable | Draws on | Time |
|---|---|---|---|
| 1 | Questions for the product manager; and the **invariants** — each written so that a query could check it | 4.3 | 15 min |
| 2 | Estimates: rates, the size of a campaign's backlog, recovery after an hour's outage | 4.6 | 20 min |
| 3 | The request: what a team sends, what it gets back, and what makes a retry harmless | 1.5, 4.1 | 10 min |
| 4 | The tables | 2.5 | 15 min |
| 5 | Accepting a request without a dual write | 4.5 | 10 min |
| 6 | A queue or a log — for what? | 4.4 | 10 min |
| 7 | Sending: taking a delivery, the lease, what is checked at that moment, and recording the result | 4.2 | 20 min |
| 8 | Once per channel: where it is guaranteed, where it is not, and what you choose there | 4.3 | 15 min |
| 9 | Staying inside three providers' limits, and keeping a campaign from delaying anything else | 4.1, 4.6 | 15 min |
| 10 | Retries, expiry and dead letters, for each class | 4.2, 4.6 | 15 min |
| 11 | Opt-outs | 4.3 | 5 min |
| 12 | What the providers tell you afterwards: delivered, bounced | 4.3, 4.7 | 10 min |
| 13 | What you monitor and alert on | 4.6 | 10 min |
| 14 | **What breaks first as it grows — in order** | all | 10 min |

The two standing rules:

- **Every number has a source**: the brief, arithmetic you show, or an assumption you label.
- **Choose.** Pick an option, give the reason, and name what you gave up.

And one for this capstone:

- **For every message, say what happens to it when each thing it depends on fails** — the sender, the provider, the caller's network — and which of *sent twice* and *not sent* you have chosen, where you had to choose.

---

## Worked design

<details>
<summary><strong>Open only after writing your own.</strong></summary>

### 1. Questions and invariants

| Question | Assumed answer |
|---|---|
| If a login code cannot be sent in time, is a late code better than none? | No. After 10 minutes it does not work. The user asks again |
| If we cannot tell whether a text message was sent, do we send it again? | For a login code, yes: a second copy of the same code is harmless. For anything where a second copy would cost or confuse, no |
| Does an opt-out stop a marketing email that is already queued? | Yes. That is what *at once* means |
| May transactional messages be sent to someone who has opted out of marketing? | Yes. They are not marketing |
| If push fails, do we fall back to another channel? | Not in version one |
| Who decides the wording? | The calling team supplies a template and its data. The platform delivers |

**The invariants.** The reference implementation checks each of these after its run, by comparing what the providers were actually asked to send with what should have been sent.

1. One request, however many times it is sent, is one notification.
2. A delivery is sent at most once on its channel — where the provider makes that possible.
3. No marketing email is sent to a user after the moment they opted out.
4. No login code is sent after it has expired.
5. No provider's limit is exceeded.
6. Every delivery ends in exactly one of: sent, suppressed, expired, dead.

### 2. Estimates

| Class | Per day | Average | Peak |
|---|---|---|---|
| Login codes (SMS) | 2 million | 23 /s | 70 /s |
| Transactional (email and push) | 10 million | 115 /s each | 350 /s each |
| Marketing (email) | 18 million | in campaigns of up to 5 million | |

**Against the limits.** Codes peak at 70 a second against 100: enough, with little to spare. Transactional email peaks at 350 against 1,000. So the email provider's 1,000 a second is divided: **400 kept for transactional, 600 for marketing.**

**A campaign.** 5 million emails at 600 a second is 8,300 seconds: **2.3 hours**, inside *a few hours*. For those hours there is, by design, a backlog of millions. It must be one that nothing else waits behind.

**An hour without the email provider.** Transactional email keeps arriving at 115 a second: 414,000 waiting when it returns. With 400 a second reserved and 115 arriving, headroom is 285 a second, and the backlog clears in **24 minutes** (Chapter 4.6: backlog ÷ headroom). Lend it marketing's share for the recovery and it is under 8.

**An hour without the SMS provider.** Nothing to recover: every code older than ten minutes is discarded unsent.

**Rows.** 30 million notifications make about 40 million deliveries a day. At an assumed 150 bytes each with indexes, 6 GB a day; 90 days is over 500 GB. That table is the first thing this design will outgrow (section 14).

### 3. The request

`POST /notifications` with the user, the class, the template and its data, and a **request key chosen by the caller**: `order-881-shipped`, `spring-sale-2026:user-4411`. The key names the thing that should happen once (Chapter 4.3), and the platform stores it under a unique index. A request whose key has been seen changes nothing and returns the original notification.

The answer is `202 Accepted` with the notification's ID (Chapter 4.1). Nothing has been sent yet. `GET /notifications/{id}` says what became of each delivery.

A campaign is not five million requests. It is one — *this template, to this segment* — which the platform expands into deliveries in batches (section 14).

### 4. The tables

```sql
CREATE TABLE notifications (
    id          bigserial PRIMARY KEY,
    request_key text NOT NULL UNIQUE,
    user_id     bigint NOT NULL,
    class       text NOT NULL,           -- 'code', 'transactional', 'marketing'
    template    text NOT NULL,
    data        jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz               -- after this it is not worth sending
);

CREATE TABLE deliveries (                 -- one per notification and channel. Also the queue
    id              bigserial PRIMARY KEY,
    notification_id bigint NOT NULL REFERENCES notifications,
    channel         text NOT NULL,        -- 'email', 'sms', 'push'
    class           text NOT NULL,
    state           text NOT NULL DEFAULT 'waiting',   -- sent | suppressed | expired | dead
    attempts        int  NOT NULL DEFAULT 0,
    visible_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    provider_id     text,                 -- what the provider called it, for matching later reports
    provider_status text,                 -- delivered, bounced… (section 12)
    UNIQUE (notification_id, channel)
);
CREATE INDEX deliveries_waiting ON deliveries (channel, class, visible_at) WHERE state = 'waiting';
```

`UNIQUE (notification_id, channel)` is the constraint that makes a second delivery of the same notification on the same channel impossible to *record* (invariant 2's first half). The partial index holds only waiting rows, so finding the next one stays fast however many are finished.

### 5. Accepting, without a dual write

The request must be recorded *and* its deliveries queued. If those were a database and a broker, Chapter 4.5's problem would apply. They are not: **the deliveries table is the queue**, so one statement does both.

```sql
WITH n AS (INSERT INTO notifications (request_key, user_id, class, …)
           VALUES ($1, $2, $3, …) ON CONFLICT (request_key) DO NOTHING RETURNING id)
INSERT INTO deliveries (notification_id, channel, class)
SELECT n.id, c, $3 FROM n, unnest($4::text[]) AS c;
```

Either the notification and all its deliveries exist, or none of them. A repeated request inserts nothing.

### 6. A queue, and a log

**Deliveries are jobs** (Chapter 4.4): each is done once, retried alone, delayed, expired, and given a priority. That is a queue. A table is enough to begin with, and being in the same database as the notifications is what made section 5 one statement.

**What happened to them is events.** Other teams want to know that an order email bounced, or how a campaign performed. The platform publishes `notification.sent`, `notification.bounced` and so on to a log, through an outbox (Chapter 4.5), with designed payloads (Chapter 4.7). Nobody outside reads the `deliveries` table.

### 7. Sending

A sender takes one delivery for its channel and classes (Chapter 4.2):

```sql
UPDATE deliveries SET attempts = attempts + 1, visible_at = now() + interval '30 seconds'
WHERE id = (SELECT id FROM deliveries
            WHERE state = 'waiting' AND channel = $1 AND class = ANY ($2) AND visible_at <= now()
            ORDER BY visible_at, id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id, notification_id, class, attempts;
```

Then, in order:

1. **Read what decides whether to send — now.** Has it expired? Has the user opted out? Those are read at this moment, not when the request was accepted, which may be hours ago.
2. If expired: mark it `expired`. If opted out: `suppressed`. Stop.
3. Wait for permission from the rate limiter (section 9).
4. Call the provider, with **the delivery's ID as the idempotency key**.
5. Record the result: `sent`; or hidden again for a retry; or `dead`.

If the sender dies anywhere, the delivery's 30 seconds run out and another sender takes it. If it died between steps 4 and 5, the provider is called again — with the same key.

### 8. Once per channel

| Channel | The provider accepts a key? | Guarantee |
|---|---|---|
| Email | Yes | **Once.** A second call with the same key sends nothing |
| Push | Yes | **Once** |
| Text message | No | **A choice**, per class |

For text messages there is no way to make *send* and *record that it was sent* one action (Chapter 4.3). So choose which error to allow:

- **Login codes: at least once.** Send, then record. If the sender dies between, the code is sent again — the same code, a second time. That is cheap and harmless, and the alternative is a user who cannot log in.
- **Anything by text where a duplicate matters** (none in version one): record first, then send — at most once.

The honest statement of invariant 2 is therefore: *once on email and push; on SMS, at least once, with a duplicate only when a sender dies in a window of a few milliseconds.* The run below counts them.

### 9. Three limits, and a campaign

**The limit is enforced by a pace, not by counting workers.** Each provider has a limiter that releases one permission every *1 ÷ rate* seconds, set 10% under the provider's limit. Senders wait for it. With several instances of the service, each is given a share of the rate — a fixed share is the simplest thing that is always safe.

**A refusal from a provider is a signal to slow down**, not to retry at once. A `429` puts the delivery back with a wait, and counts on a graph that should stay at zero.

**The campaign cannot delay anything**, because it cannot use what it was not given:

| Provider | Senders and rate kept for | The rest |
|---|---|---|
| Email, 1,000 /s | Transactional: 400 /s | Marketing: 600 /s |
| SMS, 100 /s | Codes: all of it | — |
| Push, 5,000 /s | Transactional: all of it | — |

Transactional senders take only transactional deliveries. Five million marketing rows are simply not candidates for them (Chapter 4.6: separate queues, reserved workers). The cost is that marketing cannot borrow transactional's idle share; lending it when the transactional queue is empty is a refinement.

### 10. Retries, expiry, dead letters

| Class | Retries | Expires | After the last attempt |
|---|---|---|---|
| Login code | Every second or two | 10 minutes after the request | `expired`. Never sent late |
| Transactional | Waits doubling from 1 s to 5 minutes, for 24 hours | After 24 hours | `dead`, and an alert |
| Marketing | Waits doubling to 15 minutes, for 6 hours | When the campaign ends | `dead`. Counted, not alerted |

A failure that will not pass — *no such address* — is `dead` at the first attempt, and the address is marked so that the next notification does not try it. Sending repeatedly to dead addresses is how an account's reputation with a provider is lost.

`dead` and `expired` are states in the same table, not a separate queue, so `GET /notifications/{id}` can report them and a person can put them back by setting `state = 'waiting'`.

### 11. Opt-outs

The preference is read at step 1 of section 7, at the moment of sending. An opt-out is one `UPDATE` to the user's row, and from that instant every marketing delivery for them — queued an hour ago or a second ago — is marked `suppressed` when its turn comes. Nothing has to find and cancel queued messages.

The tempting alternative — check when the request is accepted — is right for a second and wrong for the two hours a campaign takes.

### 12. What providers say afterwards

Providers report later, by calling a webhook: *delivered*, *bounced*, *marked as spam*. These arrive at least once, and in any order (Chapter 4.3). So the handler is idempotent and ordered by rank, not by arrival:

```sql
UPDATE deliveries SET provider_status = $2
WHERE provider_id = $1 AND status_rank(provider_status) < status_rank($2);
```

where `status_rank` is a small function that orders the statuses: sent, then delivered, then bounced, then complained.

A *delivered* that arrives after a *bounced* changes nothing. A bounce marks the address; a spam complaint is treated as an opt-out.

### 13. Monitoring

For each channel and class:

- **The age of the oldest waiting delivery** — the alert. Codes: 5 seconds. Transactional: 1 minute. Marketing: 6 hours.
- Deliveries waiting; sent per second, against the limit.
- Provider errors and refusals, by provider. Refusals should be zero.
- `expired` and `dead` per minute. A burst of expired codes *is* an SMS outage.
- Bounces and complaints as a share of sends, by campaign.

And, for the product: the time from request to sent, by class, as percentiles.

### 14. What breaks first

1. **The deliveries table.** 40 million rows a day, each inserted and updated, in the database that also holds the record. First: partition it by day and drop partitions after 90 (Chapter 5.3). Then: move the *queue* to a broker and keep the table as the record — at which point accepting a request becomes a dual write, and needs an outbox (Chapter 4.5).
2. **A campaign as one transaction.** Five million inserts at once would hold locks and flood replication (Chapter 3.6). Expand it in batches of a few thousand, pausing when the marketing queue is deep (Chapter 4.6).
3. **One rate limit, many senders.** A fixed share per instance wastes capacity when instances are unevenly busy. A shared limiter fixes that and becomes something every send depends on.
4. **One provider per channel.** For login codes, the SMS provider is a hard dependency (Chapter 1.1). The second provider in the brief needs a rule for when to switch (Chapter 6.3) — and reopens section 8: a code the first provider may or may not have sent is sent again by the second.
5. **The digest.** *One email an hour* means holding notifications and grouping them per user per window: stream processing, with Chapter 4.7's question of when the hour is finished.
6. **One team's flood.** Reserved shares protect classes from each other, not teams. A team that queues ten million transactional messages delays every other team's (Chapter 4.6).
7. **The bill.** Two million text messages a day is the largest line, and every duplicate is paid for (Chapter 9.8).

</details>

---

## Reference implementation

[`code/capstone-notify/main.go`](code/capstone-notify/main.go) is the worked design in about 460 lines of Go. PostgreSQL is real, and the deliveries table is the queue. The three providers are stand-ins inside the program: each has a rate limit that it enforces, an outage, and — for email and push — a memory of idempotency keys. They also keep a log of everything they were really asked to send, which is what the checks read.

The run is the design scaled down: a login code is valid for 3 seconds, and the providers accept 300, 60 and 1,000 a second.

```bash
go run ./phase-4-async/code/capstone-notify
```

```text
25 seconds of requests: 20 login codes and 40 order updates a second; one request in twenty is sent twice.
  second  4: a campaign of 4,000 marketing emails is queued
  second  6: 400 of its recipients opt out of marketing
  seconds 9 to 13: the email provider is down          seconds 17 to 22: the SMS provider is down

WHAT HAPPENED
  requests accepted 5500, of which the caller sent 289 twice; senders that died after sending: 27

  channel   class              queued      sent   suppressed   expired    dead   waiting
  email     marketing            4000      3600          400         0       0         0
  email     transactional        1000      1000            0         0       0         0
  push      transactional        1000      1000            0         0       0         0
  sms       code                  500       443            0        57       0         0

  from request to sent                     median    99th percentile
  code, by sms                             0.03 s             2.33 s
  transactional, by email                  0.04 s             5.31 s
  transactional, by push                   0.01 s             0.31 s
  marketing, by email                     14.13 s            24.10 s

CHECKS
  requests that became two notifications                      0
  emails sent more than once                                  0
  push messages sent more than once                           0
  text messages sent more than once                           2
  marketing emails sent after the user opted out              0
  login codes sent after they had expired                     0
  calls refused by a provider for exceeding its limit         0
  deliveries still waiting, unaccounted for                   0
```

(Counts vary a little from run to run. The checks, apart from the text messages, do not.)

Read the middle table first: **every delivery is accounted for.** Of 4,000 marketing emails, 3,600 were sent and 400 suppressed — the 400 people who opted out two seconds after the campaign was queued. Of 500 codes, 57 expired during the SMS provider's five seconds away and were never sent.

- **289 requests were sent twice** by their callers, and none became a second notification.
- **27 senders died** between sending and recording. For email and push, the next sender called the provider with the same key and nothing more was sent. For text messages there is no key, and **two were sent twice**. That is section 8's choice, measured: two duplicate codes, and no user without one.
- **During the campaign**, an order update's email took 0.04 seconds at the median. Its 99th percentile, 5.31 seconds, is the four seconds the email provider was down, plus a retry. The campaign itself took about 24 seconds, as it should at two thirds of 300 a second.
- **No provider refused a call**: the pace held every sender under the limit, including during the recovery from each outage.

### Remove one safeguard at a time

```bash
go run ./phase-4-async/code/capstone-notify -without check-at-send
```

```text
  email     marketing            4000      4000            0         0       0         0
  marketing emails sent after the user opted out            400
```

The preference is checked when the request is accepted, and not again. All 400 people who opted out at second 6 were sent the campaign anyway, some of them twenty seconds later.

```bash
go run ./phase-4-async/code/capstone-notify -without reserved
```

```text
  transactional, by email                  7.17 s            13.44 s
```

One pool of senders and one rate for all email. Nothing failed and every email was sent — and an order update now waits behind the campaign: 7 seconds at the median, where it had been 0.04.

```bash
go run ./phase-4-async/code/capstone-notify -without ttl
```

```text
  sms       code                  500       500            0         0       0         0
  login codes sent after they had expired                    60
```

Every code was delivered, which looks like success. Sixty of them arrived after they had stopped working, and each cost the price of a text message.

```bash
go run ./phase-4-async/code/capstone-notify -without request-key
```

```text
  email     marketing            4199      3786          413         0       0         0
  requests that became two notifications                    289
```

Every retried request became a second notification, with deliveries and idempotency keys of its own — so the providers, correctly, sent them. Two hundred and eighty-nine people were told the same thing twice, and the check on duplicate emails still reads zero: each *delivery* was sent once. Deduplication at the provider cannot repair a duplicate that was created at the front door.

---

## Review your own design

Check your design against these. Each is a mistake that an experienced reviewer would look for.

- [ ] Does the **caller** choose a key, and does a unique constraint enforce it?
- [ ] Is the request answered **before** anything is sent — and can the caller find out what happened later?
- [ ] Are the notification and its queued deliveries recorded **atomically**? If your queue is a broker, where is the outbox?
- [ ] Did you choose a **queue** for deliveries, and say why not a log?
- [ ] What happens when a sender **dies after the provider call** and before recording it — on each channel?
- [ ] For the provider with no idempotency key: did you **choose** between twice and never, and say so?
- [ ] Is the opt-out checked **when sending**?
- [ ] Can an expired code be sent **late**?
- [ ] Is each provider's limit enforced by something that holds **during recovery from an outage**, when the backlog is largest?
- [ ] Can five million marketing emails delay **one** login code or order update?
- [ ] Does every class have a **last attempt**, and a state after it that a person can see?
- [ ] Are provider reports handled **idempotently and in any order**?
- [ ] Do you alert on the **age of the oldest waiting delivery**, per class?
- [ ] Can you say what breaks **first**?

---

## Extensions

1. **A second SMS provider.** Add it, with a rule for switching. Count the duplicate codes that the switch itself causes, and decide whether that is acceptable.
2. **Lend the idle share.** Let marketing use transactional's email rate when no transactional delivery is waiting, without ever delaying one. Measure both delays.
3. **The outbox.** Publish `notification.sent` and `notification.expired` to the log model of Chapter 4.4 through an outbox table, and write a consumer that counts sends per campaign — idempotently.
4. **Provider webhooks.** Have the stand-in providers report *delivered* and *bounced* later, at least once and out of order, and implement section 12.
5. **A campaign of a million.** Expand it in batches with backpressure, and compare the database's behaviour with one enormous insert.
6. **Quiet hours.** No marketing between 22:00 and 08:00 in the user's time zone. Where does the check go, and what does `visible_at` have to do with it?
7. **The digest.** Hold transactional notifications for users who chose it, and send one email per user per hour. Decide what happens to a notification that arrives a second after its hour closed.

---

## Sources

This capstone applies the chapters of Phase 4, where the sources for each technique are given. All output shown was produced by the reference implementation on PostgreSQL 16.

**Next:** [Phase 5 — Distributed data](../phase-5-distributed-data/README.md): replication, partitioning and consistency — which take on the first items of this capstone's *what breaks first*, and of the two before it. It begins with [5.1 Leader–follower replication](../phase-5-distributed-data/01-leader-follower-replication.md).
