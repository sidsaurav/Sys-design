# 4.7 Event-driven design

[Syllabus](../SYLLABUS.md) · Phase 4 · Previous: [4.6 Operating queues](06-operating-queues.md) · Next: [Capstone 4](capstone-notification-platform.md)

> **Time:** about 35 minutes to read, 40 minutes of practice.
> **You need:** logs from [Chapter 4.4](04-logs-and-streams.md), the outbox from [Chapter 4.5](05-dual-writes-outbox-cdc.md), and expand–migrate–contract from [Chapter 2.7](../phase-2-data/07-schema-changes.md). The example needs only Go.

**By the end of this chapter you can**

- tell an event from a command, and say what each couples to what;
- decide how much an event should carry;
- change the shape of an event without breaking the services that read it;
- compare choreography with orchestration for a process that spans services;
- explain event time, processing time and late events, which is most of what stream processing asks of you.

---

## The problem

*An illustrative scenario; the example reproduces its three parts.* Two years after putting *order placed* on a log (Chapter 4.4), the company has forty services and two hundred kinds of event. It was meant to decouple them, and in one sense it did: no service calls another.

- The orders team renames a field in *order placed*. Nothing fails. Three weeks later finance notices that revenue on one dashboard has been zero since the release. Eleven services read that event; nobody had a list.
- A customer's payment is declined, and the stock for their order stays reserved for ever. The stock service reserves when it sees *order placed*. No service is responsible for un-reserving, because no service — and no document — describes the whole of what happens to an order.
- The *orders per minute* graph disagrees with the database by a few per cent, in both directions, every day.

None of these is a fault in the log. They are what happens when the *design* of events — what they say, what shape they have, who decides what follows — is left to accumulate.

---

## The concept

### 1. Events and commands

An **event** says that something happened: *order placed*. It is in the past tense, it is a fact, and its sender does not know or care who reads it. A **command** asks for something to be done: *reserve stock for order 123*. It has one intended receiver, and the sender cares about the outcome.

The difference is in what depends on what.

- With a command, the sender knows the receiver. The orders service depends on the stock service.
- With an event, the receiver knows the sender. The stock service depends on the orders service's events.

Events turn the dependency around; they do not remove it. And they can be misused. Martin Fowler names the commonest misuse: "when an event is used as a passive-aggressive command. This happens when the source system expects the recipient to carry out an action, and ought to use a command message to show that intention, but styles the message as an event instead" ([Fowler, *What do you mean by "Event-Driven"?*](https://martinfowler.com/articles/201701-event-driven.html)). If the sender would consider it a failure for nobody to act, it is a command, whatever it is called.

### 2. How much an event carries

Fowler's article separates two kinds of event that are often confused.

**A notification** says *order 123 changed* and little more. A reader that needs details asks the orders service. The event is small and never stale; every reader then calls back, and the orders service must be up for them to do anything.

**An event that carries the state** — Fowler's *event-carried state transfer* — includes what readers need: the items, the totals, the address. Readers work without calling back, and keep working when the orders service is down. The price is that the data in the event is a copy, as of one moment (Chapter 4.3's old news), and that the event's shape is now something many services depend on.

Carry what readers need to act, an ID to fetch the rest, and a version to recognise old news. Do not carry a copy of the whole row because it was there.

### 3. The shape of an event is a public interface

Chapter 2.7 changed a table's shape while old and new code were both running. An event's shape is harder: its readers are other teams' services, deployed on other days, and — on a log — old events stay to be read again by code written years later.

Two directions matter, and the registry that most Kafka users rely on defines them. "BACKWARD compatibility means that consumers using the new schema can read data produced with the last schema." "FORWARD compatibility means that data produced with a new schema can be read by consumers using the last schema, even though they may not be able to use the full capabilities of the new schema" ([Confluent docs, *Schema Evolution and Compatibility*](https://docs.confluent.io/platform/current/schema-registry/fundamentals/schema-evolution.html)).

In practice, both are kept by a short list of rules:

| Change | Safe? | Why |
|---|---|---|
| Add an optional field, with a documented default | Yes | Old readers ignore it; new readers of old events use the default |
| Remove a field that no reader uses | Only after proving that | Some reader always does |
| Rename a field | **No** | To an old reader, the old field has vanished and an unknown one has appeared |
| Change a field's type or meaning | **No** | Cents to euros, a number to text, UTC to local time |
| Make an optional field required | **No** | Old events do not have it |

When a breaking change is truly needed, it is a new event — *order placed, version 2* — published **alongside** the old one until every reader has moved: Chapter 2.7's expand, migrate, contract, with other teams' deploys in the middle.

Three things make the rules hold:

- **Readers must be tolerant**: ignore fields they do not know. A reader that rejects unknown fields turns every safe change into an outage.
- **A schema registry** checks each new version of an event against the rules before a producer can use it. The check is mechanical, and belongs in the build.
- **Know your readers.** Eleven services read that event; there should have been a list.

### 4. Who decides what happens next

A process that crosses services — pay, reserve, ship — can be coordinated in two ways. The names are from the literature on sagas (Chapter 5.7): "Choreography - each local transaction publishes domain events that trigger local transactions in other services", and "Orchestration - an orchestrator (object) tells the participants what local transactions to execute" ([microservices.io, *Pattern: Saga*](https://microservices.io/patterns/data/saga.html)).

**Choreography** needs no central component. Each service knows only its own reaction, and a new reaction can be added without touching anyone. What it lacks is a place where the process is written down. Fowler again: "it can be hard to see such a flow as it's not explicit in any program text. Often the only way to figure out this flow is from monitoring a live system", and so "it's very easy to make nicely decoupled systems with event notification, without realizing that you're losing sight of that larger-scale flow" ([Fowler](https://martinfowler.com/articles/201701-event-driven.html)).

**Orchestration** puts the process in one component, which sends commands and decides what follows from each reply. The flow can be read, tested and changed in one place — including what to undo when a step fails. The orchestrator must be reliable (its own state must survive a crash), and every step now depends on it.

A workable rule: **choreography for reactions, orchestration for processes.** When other services merely want to know — analytics, email, search — publish an event and let them react. When there is a sequence with an outcome that someone is accountable for — an order is either fulfilled or cleanly cancelled — give it an owner.

### 5. Stream processing, in one sitting

Reading a log and computing something continuously — counts per minute, a running total, a join of two streams — is **stream processing**. Most of what makes it hard is one fact: events do not arrive in the order they happened.

Kafka's stream library distinguishes two clocks. "Event time - The point in time when an event or data record occurred". "Processing time - The point in time when the event or data record happens to be processed by the stream processing application… The processing time may be milliseconds, hours, or days etc. later than the original event time" ([Kafka documentation, *Streams: Core Concepts*](https://kafka.apache.org/39/streams/core-concepts/)). A phone with no signal, a consumer that was down for an hour, a replay of last week: in each, processing time says *now* and event time says *then*.

Counting *per minute* means choosing which minute an event belongs to.

- **By processing time** is easy, and wrong whenever anything is delayed: events are counted in the minute they arrived.
- **By event time** is right, and raises a question that has no perfect answer: *when is a minute finished?* An event for 10:04 may still arrive at 10:07. Wait longer, and the count is more complete and later. Stop waiting, and it is prompt and some events miss it.

Every stream processor makes this trade explicit: a **window** (the minute), an allowance for lateness, and a rule for events that arrive after it — drop them, or issue a correction. There is no setting that gives complete, prompt and final at once.

The rest of stream processing — state kept between events, joins, exactly-once within the pipeline — builds on this and on Chapters 4.3 and 4.4; frameworks such as Kafka Streams and Flink package it.

---

## A real example

### Run it: a renamed field, a declined payment, and late events

[`code/ch07-events/main.go`](code/ch07-events/main.go) — about 275 lines, no dependencies.

```bash
go run ./phase-4-async/code/ch07-events
```

```text
PART 1 — a consumer written for version 1 of "order placed" adds up revenue. The producer changes
         the event. 1,000 orders of 49.99 each: the answer should be 49,990.00.

  the producer now sends...                                errors    revenue counted
  version 1, as before                                          0           49990.00   correct
  a new field: currency                                         0           49990.00   correct
  total_cents renamed to amount_cents                           0               0.00   WRONG, and nothing failed
  total_cents as text: "49.99"                               1000               0.00   every event fails: loud, and stops the consumer
  a new field — to a consumer that rejects unknowns          1000               0.00   every event fails: loud, and stops the consumer

  A version-2 consumer reading a version-1 event finds no currency, and uses the documented default: USD.
```

The consumer is ordinary Go, decoding JSON into a struct.

- **A new field** is ignored. This is the one change that is always safe.
- **A renamed field** is the opening scenario. The decoder finds no `total_cents`, leaves the number at zero, and reports success a thousand times. Revenue: 0.00.
- **A changed type** fails every event. That is the better failure: it is noticed in a minute, not in three weeks.
- **The last row** is the same safe change as the second, sent to a consumer configured to reject fields it does not recognise. It fails every event too. A strict reader makes *every* change a breaking one.

```text
PART 2 — an order needs a payment, stock, and a shipment. 100 orders; 10 payments are declined.

  CHOREOGRAPHY — order 9 succeeds; order 10 is declined:
    orders         event:   order placed
    stock          event:   stock reserved
    payments       event:   payment taken
    shipping       event:   order shipped

    orders         event:   order placed
    stock          event:   stock reserved
    payments       event:   payment declined

    shipped: 90      stock still held for orders that were never paid: 10

  ORCHESTRATION — order 9 succeeds; order 10 is declined:
    orchestrator   command: charge payment
    payments       reply:   taken
    orchestrator   command: reserve stock
    stock          reply:   reserved
    orchestrator   command: ship
    shipping       reply:   shipped

    orchestrator   command: charge payment
    payments       reply:   declined
    orchestrator   the order is cancelled; nothing else was started

    shipped: 90      stock still held for orders that were never paid: 0
```

In the choreographed version each service is reasonable. Stock reserves when an order exists; payments charges when an order exists; shipping waits for both. Nobody wrote the reaction to *payment declined*, because it was nobody's job to think about the order as a whole — and ten reservations are never released. The fix is one more reaction, in the stock service. The point is that nothing in any program would have told you it was missing.

In the orchestrated version the sequence is written down — payment first, then stock — and the declined case is three lines in one place. It costs more messages, and a component that every order now passes through.

```text
PART 3 — orders are counted per minute, for five minutes. One order in ten reaches the counter late —
         by up to 90 seconds — because the phone it came from had no signal.

  minute           true     counted on arrival           by when it happened,           by when it happened,
                                                  waiting 30 s for stragglers   waiting 120 s for stragglers
  1                 600                    547                            577                            600
  2                 600                    593                            572                            600
  3                 600                    620                            588                            600
  4                 600                    599                            580                            600
  5                 600                    592                            578                            600
  after                                     49                                                              
  too late                                                                105                              0
  published                            at once         30 s after each minute        120 s after each minute
```

Exactly 600 orders happened in every minute.

- **Counted on arrival**, no minute is right, and the errors go both ways — minute 3 is over, because it received minute 2's stragglers. This is the dashboard that never agrees with the database.
- **By event time, waiting 30 seconds**, each minute is short by the orders that took longer than that: 105 in all arrived too late to be counted.
- **Waiting 120 seconds**, every count is exact, and every count is two minutes old when it is published.

The three columns are the trade of section 5: prompt and wrong, fairly prompt and nearly right, right and late.

---

## Trade-offs

**Events decouple in time and in knowledge, and couple in shape.** A producer need not know its readers or wait for them. Every reader, though, depends on what the event contains — and the producer can no longer change it freely.

**Small events are always fresh and make readers call back; full events make readers independent and can be stale.** Choose per event, by whether readers need to act without the producer.

**Strict schemas catch mistakes early; tolerant readers survive change.** Use both: check compatibility strictly where events are *published*, and read tolerantly where they are consumed.

**Choreography is easy to extend and hard to see; orchestration is easy to see and is one more thing that must not fail.** A process with an outcome deserves an owner. A fan-out of interested parties does not need one.

**Event time is correct and makes you wait; processing time is immediate and is wrong when anything is delayed.** Decide how late a result may be, and what happens to an event that is later still.

**An event-driven system is harder to follow than a call graph.** The reasons for anything that happened are spread across services and time. Carry an ID for the whole process on every event (Chapter 7.4 uses it), and keep a catalogue of events, with their owners and readers.

---

## Practice

### Check yourself

Answer before reading on.

1. The orders service publishes a message called `OrderPlaced`, and the on-call runbook says: *if the warehouse has not acknowledged an order within 10 minutes, page the orders team.* Is `OrderPlaced` an event? What follows?
2. A team wants to change `price` in an event from cents (an integer) to a decimal string, "to be clearer". Three services read the event. What is the safe way to do it?
3. A fraud rule fires when a card is used more than five times in a minute. During a network problem, events from one region arrive forty seconds late. What happens if the rule counts by processing time? By event time?

<details>
<summary>Answers</summary>

1. **No — it is a command in disguise.** The orders team will be paged if nobody acts on it, so they expect one particular receiver to do one particular thing. That is Fowler's passive-aggressive command. Two honest designs: make it explicit — a `ShipOrder` command to the warehouse, with a reply, owned by a process that tracks each order to completion (orchestration); or keep the event and make the *warehouse* accountable for the alarm, since reacting is its responsibility. What is not sound is an event whose sender secretly depends on a reaction.

2. **Do not change the field. Add one.** Publish `price_decimal` alongside `price`, in the same event, documented, with `price` unchanged. The three readers move to the new field in their own releases. When all three have — confirmed with their owners, and by checking that nothing still reads `price` — stop publishing the old field, which is itself a change to announce. If the event is on a log that is replayed, old events will never have `price_decimal`, so readers need a rule for that too: compute it from `price`. Changing the type in place fails every reader that has not been redeployed at the same instant — or, in a language that converts silently, charges someone a hundred times too much.

3. **By processing time**, the delayed events all arrive together, forty seconds' worth in one burst. Cards that were used once every ten seconds appear to be used five times in a second, and the rule fires on honest customers; meanwhile real bursts in that region during the delay were not seen when they happened. **By event time**, each use is placed in the minute it occurred, so the counts are right — provided the rule waits at least forty seconds before treating a minute as complete, which means fraud is detected forty seconds later. The choice is between false alarms and missed bursts on one side, and a delay on the other; most fraud systems take the delay, and bound it.

</details>

### Exercise: review an event

A team proposes this event for the log, published whenever anything about a customer changes. Review it. List what is wrong, and write the event you would publish instead.

```json
{
  "type": "CustomerUpdated",
  "data": {
    "id": 4411,
    "name": "Ana Lima",
    "email": "ana@example.com",
    "password_hash": "$2b$12$Kix...",
    "address": "12 Rua Nova, Lisboa",
    "marketing_opt_in": true,
    "internal_risk_score": 0.82,
    "updated": "06/03/2026 14:05"
  }
}
```

<details>
<summary>Worked solution</summary>

**What is wrong**

| Problem | Why it matters |
|---|---|
| It is a copy of the database row | Every reader now depends on the table's columns. The next migration (Chapter 2.7) is a breaking change to a public interface |
| `password_hash` and `internal_risk_score` | Secrets and internal judgements, sent to every service that subscribes — and kept in the log for as long as it retains (Chapter 8.4). Data that is published cannot be recalled |
| *Updated* — but what changed? | A reader that cares only about the address must compare with a copy it has kept. The event says that something happened, not what |
| No event ID | Readers cannot recognise a duplicate (Chapter 4.3) |
| No version of the customer | Readers cannot recognise old news: an update delivered late overwrites a newer one |
| No version of the event's format | There is no way to introduce a second shape later |
| `"updated": "06/03/2026 14:05"` | The sixth of March or the third of June? In which time zone? |
| No key is named | Without the customer's ID as the key (Chapter 4.4), two updates to one customer can be read out of order |

**What to publish instead**: specific events, each saying what happened and carrying what readers of *that* fact need.

```json
{
  "event_id": "0190a3f2-7c1e-7a55-b1d6-3c9e5f2a8d41",
  "type": "customer.address_changed",
  "format_version": 1,
  "occurred_at": "2026-03-06T14:05:12Z",
  "customer_id": 4411,
  "customer_version": 17,
  "address": { "line1": "12 Rua Nova", "city": "Lisboa", "country": "PT" }
}
```

- **`event_id`**, unique, from the outbox (Chapter 4.5): for recognising duplicates.
- **`type`** names one fact. There are others — `customer.email_changed`, `customer.marketing_preference_changed` — and a service subscribes to the ones it needs.
- **`format_version`**: the shape can evolve by the rules of section 3, and a breaking change becomes version 2, published alongside.
- **`occurred_at`** in UTC, in a format with one reading.
- **`customer_version`**, increasing with every change to the customer: for recognising old news.
- **The payload is designed**: the address, structured, and nothing else. No password hash and no risk score, because no reader of an address change needs them.
- Published **with `customer_id` as the key**.

If some reader really does need the whole current customer — a search index, a cache — give it a separate, **compacted** topic (Chapter 4.4) of customer snapshots, with its own carefully chosen fields. That is a different product from the events, with a different purpose, and it should be a deliberate one.

</details>

---

## Production tips and common mistakes

**Tips**

- **Name events in the past tense, for one fact each.** If the sender needs a reaction, send a command and own the outcome.
- **Design the payload.** What readers need to act, an ID for the rest, and versions for the event and for the thing it describes.
- **Only ever add optional fields.** Anything else is a new version, published alongside the old.
- **Check compatibility automatically**, before a producer's change can be released.
- **Read tolerantly.** Ignore what you do not recognise.
- **Keep a catalogue**: every event, its owner, its schema, its readers.
- **Give every multi-step process an owner** — a service, or an orchestrator — and write down what is undone when a step fails.
- **Put event time in every event**, in UTC, set by whoever witnessed the fact.
- **In stream processing, choose the allowance for lateness on purpose**, and count what arrives after it.
- **Carry one ID for the whole process** on every event it causes.

**Common mistakes**

- **Publishing the database row.**
- **Renaming a field.**
- **Readers that fail on unknown fields.**
- **One enormous `Updated` event** for every change.
- **Commands dressed as events**, and alarms when nobody reacts.
- **A process that exists only as the sum of forty reactions.**
- **Counting by arrival time** and wondering why the totals drift.
- **Secrets and personal data in events**, retained for as long as the log.
- **No list of who reads what.**

---

## Key takeaways

- An event states a fact and its sender does not depend on a reaction. A command asks for an action and its sender does. Calling a command an event does not change which it is.
- An event's shape is a public interface with readers you do not deploy. Add optional fields; never rename, retype or remove; publish breaking changes as a new version alongside the old.
- Choreography spreads a process across reactions and leaves it written nowhere. Orchestration writes it in one place, at the price of a central component. Give processes with outcomes an owner.
- Events arrive later than they happen, and out of order. Counting by event time is correct, and forces a choice between waiting and being incomplete.
- Carry IDs and versions on every event, and keep a catalogue of events and their readers.

**Next:** [Capstone 4](capstone-notification-platform.md) — a notification platform: several channels, retries, deduplication, rate limits for each provider, and what users have asked not to receive.

---

## Sources

- Martin Fowler, [*What do you mean by "Event-Driven"?*](https://martinfowler.com/articles/201701-event-driven.html), 2017.
- Confluent documentation: [*Schema Evolution and Compatibility*](https://docs.confluent.io/platform/current/schema-registry/fundamentals/schema-evolution.html).
- Chris Richardson, [*Pattern: Saga*](https://microservices.io/patterns/data/saga.html), microservices.io.
- Apache Kafka documentation: [*Streams: Core Concepts*](https://kafka.apache.org/39/streams/core-concepts/).
