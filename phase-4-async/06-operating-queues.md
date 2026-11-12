# 4.6 Operating queues

[Syllabus](../SYLLABUS.md) · Phase 4 · Previous: [4.5 The dual-write problem](05-dual-writes-outbox-cdc.md) · Next: 4.7 Event-driven design *(planned)*

> **Time:** about 30 minutes to read, 40 minutes of practice.
> **You need:** [Chapters 4.1 to 4.4](README.md), and queueing from [Chapter 1.6](../phase-1-foundations/06-concurrency-pools-queues.md). The example needs only Go.

**By the end of this chapter you can**

- name the two numbers that say whether a queue is keeping up, and the dashboards that do not;
- work out how long a backlog takes to clear, and choose among four ways of clearing it;
- explain backpressure, and why a queue with no limit is a decision to fail later;
- keep one bad record from stopping a partition;
- keep urgent work from waiting behind bulk work, and schedule work for later.

---

## The problem

*An illustrative scenario; the example reproduces its arithmetic.* On Tuesday at 09:05 a marketing campaign doubles the rate of sign-ups. The queue of welcome and verification emails begins to grow. Every dashboard is green: the workers report no errors, their CPU is steady, the API is fast — faster than usual, if anything, since it only writes to a queue.

At 15:00 support notices a pattern in the tickets: *I never got my verification email.* The emails are being sent. They are being sent six hours late, to people who gave up five and a half hours ago.

A second incident, the same month. One event in a partition of the orders log cannot be parsed by the warehouse's consumer. The consumer fails, restarts, reads the same event, and fails. Behind that event, every order in the partition waits. Other partitions flow normally, so three quarters of orders ship and the totals look only slightly low.

A queue turns failures into delays (Chapter 4.1). That is what it is for, and it is why a queue in trouble is so quiet. This chapter is about hearing it.

---

## The concept

### 1. Two modes, and two numbers

Amazon's engineers describe queues as having two personalities: "a queue-based system has two modes of operation, or bimodal behavior. When there is no backlog in the queue, the system's latency is low, and the system is in fast mode. But if a failure or unexpected load pattern causes the arrival rate to exceed the processing rate, it quickly flips into a more sinister operating mode" ([AWS Builders' Library, *Avoiding insurmountable queue backlogs*](https://aws.amazon.com/builders-library/avoiding-insurmountable-queue-backlogs/)).

In the second mode nothing errors. Workers are fully busy, which looks like health. Two numbers tell the truth:

- **Depth** (for a log, **lag**: the distance between a reader's offset and the end of the partition). How much work is waiting.
- **The age of the oldest waiting message.** How late the work is. This is the one to alert on, because it is in the unit users feel — seconds — and because it does not need to be compared with a normal rate to mean something.

Neither tells you *why*. For that, compare the arrival rate with the completion rate.

### 2. The arithmetic of a backlog

Chapter 4.1 did this sum once. It is the whole of capacity planning for queues.

- While work arrives faster than it is done, the backlog grows by the difference.
- When the burst ends, it shrinks by the *spare* capacity: what the workers can do, less what is still arriving.

Spare capacity is usually small — workers are sized for the average — so recovery is slow. With workers that handle 100 a second and 80 a second arriving, a backlog shrinks at 20 a second: 48,000 messages take forty minutes. Recovery time is the backlog divided by the headroom, and the headroom is what you chose when you chose the number of workers.

### 3. Four ways through a backlog

Once a backlog exists, there is a choice about what to work on.

**Oldest first.** The default. Every message is late until the whole backlog is gone, including the ones arriving now.

**Newest first** — or its practical form, moving the backlog aside. New messages are handled as they arrive; the old ones are worked through with whatever capacity is left. The Builders' Library describes it: on taking a message, check its age, and use that "to decide whether to move the message into a backlog queue that we work through only after we're caught up on the live queue… This frees up consumer resources to work on fresh messages more quickly than if we had simply worked the backlog in order" ([same article](https://aws.amazon.com/builders-library/avoiding-insurmountable-queue-backlogs/)). Right when a fresh message is worth more than a stale one, and order does not matter.

**Drop what is too old.** "Some systems can tolerate very old messages being dropped" ([same article](https://aws.amazon.com/builders-library/avoiding-insurmountable-queue-backlogs/)). A login code that expired ten minutes ago should not be sent at all. Give such messages a time to live, and let workers discard what has outlived it. This is the only strategy that makes the backlog smaller without doing the work.

**More workers.** Raises the headroom directly — if whatever the workers call can take it. Tripling the workers triples the load on the database or the provider behind them (Chapter 4.1).

### 4. Backpressure: a queue needs a limit

A queue with no limit accepts everything, for ever. Under sustained overload it grows until something else gives: memory, disk, or the patience of whoever is waiting for the results. It does not prevent the failure. It postpones it, and makes it larger.

A queue with a limit refuses new work when it is full. That is **backpressure**: the refusal travels backwards to the producer, which must slow down, shed load, or tell its own caller *not now* (a 429, Chapter 1.4). It turns silent lateness back into something visible, at the edge, while there is still someone to tell. Chapter 1.6 made the same argument for the queue inside a server; it holds for the one between services.

Choose the limit in time, not in messages: *we never hold more than two minutes of work*.

### 5. One bad record in a partition

On a queue, a message that cannot be handled is retried a few times and set aside (Chapter 4.2), and the messages behind it are unaffected. On a log, a reader's position is one number (Chapter 4.4). It cannot move past a record it has not handled. One bad record stops the partition.

The reader has to be given a way to move on: after a bounded number of attempts, **copy the record to another topic** — a dead-letter topic — and commit past it. Kafka's own connector framework has the setting: "The name of the topic to be used as the dead letter queue (DLQ) for messages that result in an error when processed by this sink connector" ([Kafka documentation, *Kafka Connect Configs*](https://kafka.apache.org/39/configuration/kafka-connect-configs/)). For failures that may pass — a dependency that is down — the copy can go to a *retry topic* that is read after a delay.

Either way, the record leaves its place in line. If the records behind it depend on it — later events about the same order — moving on is not free: skip the whole key until the bad record is dealt with, or stop and call someone. That is a decision to make in advance, per topic.

### 6. Urgent work and bulk work

A queue is first-come, first-served. Sixty thousand newsletter emails queued at 10:00 are ahead of a password reset requested at 10:01.

Do not try to reorder one queue. Use **separate queues, with workers reserved for each**. The reservation matters: a shared pool that prefers the urgent queue still has every worker busy with bulk work at the moment something urgent arrives.

The same reasoning separates *tenants*. One customer's enormous import should not delay every other customer's jobs: give each a share of the workers, or a queue.

### 7. Work for later

Some work has a time on it: *send the reminder tomorrow*, *retry in five minutes*, *cancel the order if it is unpaid in an hour*.

- **A delay on the message.** Brokers offer it for short delays: on SQS, "Delay queues let you postpone the delivery of new messages to consumers for a number of seconds" ([AWS docs](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-delay-queues.html)), up to fifteen minutes. The table-queue of Chapter 4.2 does it with its `visible_at` column, for any length of time.
- **A row with a due time, and a scanner.** For anything longer, or anything that may be cancelled: store *what* and *when* in the database, and have a job that looks for what is due, every minute, and queues it. The database is the record of what is scheduled; the queue only carries what is due now.

Scheduled work bunches. A thousand reminders set for 09:00 are a burst (the arithmetic of section 2 applies), and a job that runs "at midnight" on every server runs everywhere at once. Spread due times where exactness does not matter.

---

## A real example

### Run it: the arithmetic, stepped through

[`code/ch06-backlog/main.go`](code/ch06-backlog/main.go) — about 270 lines, no dependencies — does not run a queue. It steps a clock a tenth of a second at a time and counts messages, which is all a queue's timing is. That lets it use real-sized numbers and finish instantly.

```bash
go run ./phase-4-async/code/ch06-backlog
```

```text
PART 1 — workers can handle 100 messages a second. 80 a second arrive — except from minute 5 to
         minute 15, when 150 a second arrive.

    time  arriving /s   handled /s workers busy     errors      waiting      oldest has waited
    0:00           80           80          80%          0            0                    0 s
    5:00          150          100         100%          0            5                    0 s
   10:00          150          100         100%          0        15005                  100 s
   15:00           80          100         100%          0        29998                  200 s
   20:00           80          100         100%          0        23998                  300 s
   25:00           80          100         100%          0        17998                  225 s
   30:00           80          100         100%          0        11998                  150 s
   35:00           80          100         100%          0         5998                   75 s
   40:00           80           80          80%          0            0                    0 s
   45:00           80           80          80%          0            0                    0 s
   50:00           80           80          80%          0            0                    0 s
```

Read it column by column, as dashboards would show it.

- **Handled per second, workers busy, errors**: 100, 100%, 0, for thirty-five minutes. Nothing here says that anything is wrong. Busy workers with no errors is what a good day looks like.
- **Waiting**: 30,000 by the end of the burst.
- **Oldest has waited**: this is the user's experience. It is still *rising* five minutes after the burst has ended — the message at the front at 20:00 is one that arrived at the burst's last moment, behind everything else. A ten-minute burst made work late for thirty-five minutes, and five minutes late at the worst.

```text
PART 2 — the workers are down for 10 minutes while 80 messages a second arrive: 48,000 are waiting
         when they come back. Arrivals continue at 80 a second; the workers handle 100.

  the backlog is worked through...                new messages are prompt    longest any message      dropped
                                                              again after                 waited
  oldest first                                                     40 min                 10 min            0
  newest first                                                      0 min                 50 min            0
  oldest first, dropping what is over 60 s old                      4 min                  1 min        43200
  oldest first, with three times the workers                        4 min                 10 min            0
```

A ten-minute outage. Worked oldest-first, it becomes a fifty-minute incident: for forty minutes after the workers return, every new message is late. Newest-first ends the incident at once for new messages, and makes the oldest wait fifty minutes. Dropping stale messages recovers in four minutes by not doing 43,200 pieces of work — right for login codes, unthinkable for payments. Three times the workers also takes four minutes, and sends three times the traffic to whatever is behind them.

None of these is the right answer in general. Each is right for some kind of message, and the choice should be made before the outage.

```text
PART 3 — 150 messages a second arrive for 10 minutes. The workers handle 100.

  the queue...                                      waiting      oldest has waited      refused at the door
  holds whatever it is given                          30000                  200 s                 0 (0%)
  holds at most 1,000, and refuses the rest             990                   10 s             29010 (32%)
```

The same overload, with and without a limit. Without one, nothing is refused and everything is minutes late, and getting later. With a limit of ten seconds' work, a third of the arrivals are refused — immediately, where the producer can react — and what is accepted is at most ten seconds late. The system could not do 150 a second in either case. Only one of the two said so.

```text
PART 4 — one partition of a log holds 1,000 records. Record 100 cannot be handled.

  when a record fails, the reader...                handled      never reached            set aside
  tries it again, for ever                               99                901                    0
  tries 3 times, then copies it elsewhere               999                  0                    1
```

```text
PART 5 — 60,000 newsletter emails are queued at once. Password-reset emails arrive at one a second.
         The workers send 100 emails a second.

                                                   a password reset waits the newsletter is finished
  one queue for everything                                        599.9 s                     10 min
  a queue of its own, and 10 workers kept for it                      0.0 s                     11 min
```

In one queue, someone who asks for a password reset just after the newsletter is queued waits ten minutes for it. With a queue of its own and ten of the hundred workers reserved, it is sent at once, and the newsletter takes eleven minutes where it took ten. One minute of bulk work was traded for ten minutes of a locked-out user.

---

## Trade-offs

**Headroom costs money every day and buys recovery time on the bad one.** Workers sized exactly for the average never recover from anything. Size them for the recovery time you can accept: backlog ÷ headroom.

**Oldest-first is fair and slow to recover; newest-first recovers at once and is unfair to the old.** Dropping is fastest and loses work. Which is right depends on what a late message is worth.

**A limit converts lateness into refusals.** A refusal is worse for the one refused and better for everyone else — and it is visible, which lateness is not.

**Setting a bad record aside keeps the partition moving and breaks its order.** Stopping keeps the order and stops the partition. Decide per topic which is the lesser harm.

**Reserved workers protect urgent work and stand idle when there is none.** That idleness is the price of the guarantee.

**Scale workers on the queue's age, with care.** It is the right signal — and more workers move the bottleneck to whatever they call.

---

## Practice

### Check yourself

Answer before reading on.

1. A queue's workers handle 500 messages a second, and 400 a second arrive. A dependency fails for 30 minutes, during which nothing is handled. How large is the backlog when it recovers, and how long until a newly arrived message is handled promptly again? What would halve that?
2. Your dashboard for a queue shows: worker error rate, worker CPU, messages handled per second. During the Tuesday incident all three were normal. What two graphs were missing, and what alert?
3. A consumer of a log reads *order placed*, *order paid* and *order shipped* events, keyed by order. An *order paid* event cannot be handled, because of a bug. If the consumer sets it aside and continues, what can go wrong? What are the alternatives?

<details>
<summary>Answers</summary>

1. 400 × 1,800 = **720,000 messages**. Headroom is 500 − 400 = 100 a second, so the backlog clears in 720,000 ÷ 100 = 7,200 seconds: **two hours**, after a thirty-minute failure. To halve it, double the headroom: workers for 600 a second, not 500 — if what they call can take 600. Or work newest-first, which makes new messages prompt immediately and leaves only the old ones late. Or, if these messages lose their value with age, drop those past it.

2. **The queue's depth, and the age of its oldest message.** The three graphs shown describe the workers, and the workers were fine: fully busy, error-free. The alert belongs on the age — *the oldest message has waited more than five minutes* — because it is in the unit that matters, and it fires however the backlog arose: a burst, slow workers, a stalled dependency, or workers that are not running at all (in which case the other three graphs read zero, which many dashboards also show as green).

3. The **order's later events are handled without its payment**: *order shipped* is applied to an order that the consumer still believes is unpaid. Depending on the consumer, that is an error, a wrong state, or a shipment of something recorded as not paid for. Alternatives: **stop the partition** and alert — correct, and it delays every other order in that partition until someone fixes the bug; or **set aside the whole key** — record that order 123 is blocked, send its later events to the same holding place in order, and carry on with other orders, replaying 123's events in sequence once the bug is fixed. The third is the most work and usually the right one where order within a key matters. Which to do is decided when the consumer is written, not at three in the morning.

</details>

### Exercise: run a queue for a year

A product sends email through one queue: verification codes that are valid for ten minutes; receipts; a weekly newsletter to 500,000 subscribers; and reminders that users schedule for a chosen time. The provider accepts 200 emails a second. On an ordinary day about 20 a second are sent.

Design how this queue is operated: the queues and workers; what is monitored and alerted on; the limits; what happens after a two-hour outage of the provider; and how the reminders are scheduled.

<details>
<summary>Worked solution</summary>

**Queues and workers.** Three queues, not one, with the provider's 200 a second divided between them.

| Queue | Holds | Workers reserved | Why |
|---|---|---|---|
| Urgent | Verification codes, password resets | Capacity for 50 a second | Worth nothing if late. Must never wait behind anything |
| Normal | Receipts, reminders that are due | 50 a second | Should arrive within minutes |
| Bulk | The newsletter | 100 a second, and whatever the others are not using | Nobody notices an hour. 500,000 at 100 a second is 83 minutes |

**Monitor**, per queue: depth; the age of the oldest message; arrivals and completions per second; the dead-letter queue's size.

**Alert** on the age of the oldest message: urgent over 30 seconds; normal over 5 minutes; bulk over 6 hours. And on any dead letter.

**Limits.**

- *Urgent*: every message carries a time to live of ten minutes — the life of the code. Workers discard older ones without sending. The queue holds at most a minute of work; beyond that the API tells the user to try again shortly, which is honest.
- *Normal*: a limit of an hour's work. A receipt is never dropped.
- *Bulk*: no time to live, and a large limit. The newsletter job queues its messages in batches, pausing when the bulk queue is deep: backpressure on the one producer that can wait.

**After a two-hour outage of the provider.**

- *Urgent*: 20% of traffic, say 4 a second for two hours, is about 29,000 messages — nearly all of them older than ten minutes when the provider returns, and discarded. The queue is empty within seconds. Users who are still waiting ask again and get a code at once.
- *Normal*: perhaps 100,000 receipts waiting. At 50 a second with little arriving, about 35 minutes. Lend it the bulk workers and it is under 15.
- *Bulk*: waits its turn.
- The workers do not all resume at full rate at the same instant: the provider has just recovered, and 200 a second from a standing start is how to make it fail again (Chapter 1.5).

**Reminders.** A row per reminder in the database: who, what, when, and whether it has been queued. A job runs every minute and, in one statement, marks and returns the reminders now due (`UPDATE … WHERE due_at <= now() AND queued_at IS NULL … RETURNING`, with `SKIP LOCKED` if two copies of the job may run), then puts them on the normal queue. A user who cancels a reminder deletes a row. Reminders set for exactly 09:00 — there will be thousands — are a burst that the normal queue absorbs: 10,000 at 50 a second is a little over three minutes, which is acceptable for a reminder and worth knowing in advance.

**What this design refuses to do**: let a newsletter delay a login code; send a code that has expired; or find out about a backlog from support.

</details>

---

## Production tips and common mistakes

**Tips**

- **Alert on the age of the oldest message**, per queue, with a threshold chosen from what a late message costs.
- **Graph arrivals and completions together.** The gap between them is the future of the backlog.
- **Know your headroom**, and the recovery time it implies for a ten-minute and a one-hour outage.
- **Decide in advance how each queue's backlog is worked through**: in order, newest first, or with old messages dropped.
- **Give messages that expire a time to live.**
- **Give every queue a limit**, expressed as time, and make producers handle a refusal.
- **Give a log's readers a way past a bad record**: bounded attempts, then a dead-letter topic — and a rule for the records behind it.
- **Separate urgent from bulk, and tenant from tenant**, with reserved workers.
- **Keep schedules in the database** and only due work in the queue.
- **Rehearse**: stop the workers in a test environment for ten minutes and watch what the alerts and the recovery do.

**Common mistakes**

- **Watching the workers and not the queue.**
- **No alert on age**, and a six-hour backlog reported by customers.
- **Workers sized for the average**, with no headroom to recover.
- **Working through an hours-old backlog in order** while fresh, valuable messages wait behind it.
- **Sending what has expired.**
- **An unbounded queue**, described as resilience.
- **A poison record retried for ever** at the head of a partition.
- **One queue for everything.**
- **Scaling workers on backlog** into a database that is the real bottleneck.
- **Every scheduled job at midnight.**

---

## Key takeaways

- A queue in trouble does not error. It gets later. Watch its depth and, above all, the age of its oldest message.
- A backlog grows by the excess of arrivals and shrinks by the spare capacity. Recovery time is backlog ÷ headroom.
- A backlog can be worked oldest-first, newest-first, with stale messages dropped, or with more workers. The right choice depends on what a late message is worth.
- A queue needs a limit. Refusing work at the edge is better than accepting it and being hours late.
- On a log, one record that cannot be handled stops its partition until the reader is given a way past it.
- Urgent work needs its own queue and its own workers. Scheduled work belongs in the database until it is due.

**Next:** 4.7 Event-driven design *(planned)* — what to put in an event, and how forty services can share them without becoming one.

---

## Sources

- AWS Builders' Library: [*Avoiding insurmountable queue backlogs*](https://aws.amazon.com/builders-library/avoiding-insurmountable-queue-backlogs/).
- Apache Kafka documentation: [*Kafka Connect Configs*](https://kafka.apache.org/39/configuration/kafka-connect-configs/) (dead-letter topic).
- AWS documentation: [*Amazon SQS delay queues*](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-delay-queues.html).
