# 6.7 Disaster recovery and resilience testing

[Syllabus](../SYLLABUS.md) · Phase 6 · Previous: [6.6 Multi-region architectures](06-multi-region.md) · Next: [Capstone 6: a payments API](capstone-payments-api.md)

> **Time:** about 30 minutes to read, 45 minutes of practice.
> **You need:** recovery objectives and the restore drill of [Chapter 2.9](../phase-2-data/09-durability-backups-restore.md); the protections of [Chapter 6.3](03-circuit-breakers-bulkheads-degradation.md); zones and regions from Chapters [6.5](05-redundancy-and-failure-domains.md) and [6.6](06-multi-region.md). The example needs only Go, and takes about 25 seconds to run.

**By the end of this chapter you can**

- turn *how long may it be down?* and *how much may it lose?* into numbers that a design has to meet, for each part of a system;
- write a recovery plan as a procedure that has been run, and say what makes plans fail on the day;
- design a fault-injection experiment: a steady state, a hypothesis, a fault, and a guard;
- run a game day, and know what to do with what it finds;
- decide what to break in production, and how to keep the test from becoming the incident.

---

## The problem

*An illustrative recovery plan. Every item on it is of a kind that the incidents in this phase have shown.*

The plan is three pages long, and was written two years ago. It says:

1. **Declare a disaster.** It does not say who may, or on what evidence.
2. **Start the standby environment.** The script that does this was last run when the plan was written. Since then, eleven services have been added.
3. **Restore the database from the latest backup.** Nobody has timed a restore since the data was a third of its present size.
4. **Fetch the database password from the secrets store.** The secrets store runs in the region that the plan is for losing.
5. **Point DNS at the standby.** The DNS account needs a second factor, which is on the phone of an engineer who left in the spring.
6. **Verify.** It does not say how.

Each of these can be found in forty minutes by carrying the plan out. None can be found by reading it, because to its authors each step was obviously possible.

Chapter 6.6 began with a company that had tested the loss of two of its three buildings and not the third. Chapter 2.9 began with one whose backups had been failing unnoticed. Slack's engineers have the summary: "luck is not an availability strategy" ([Slack Engineering, *Disasterpiece Theater*](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)).

Everything in this phase — deadlines, budgets, breakers, limits, spare zones, standby regions — is a claim about what the system will do on a day that has not come yet. This chapter is about finding out, beforehand, whether the claims are true.

---

## The concept

### 1. Recovery objectives are inputs to the design

Chapter 2.9 gave the two numbers: the **recovery point** (RPO) — how much recent data may be lost — and the **recovery time** (RTO) — how long until the service is back. They are not properties to be discovered after the system is built. They are requirements, and they choose the design.

| If the business needs | Then the design needs | From |
|---|---|---|
| To lose at most a day of data | Nightly backups, stored elsewhere | 2.9 |
| To lose at most minutes | A continuous archive of the database's log | 2.9 |
| To lose nothing that was confirmed, when a server or a zone fails | Synchronous replicas in other zones | 5.1, 6.5 |
| To lose nothing when a region fails | Synchronous replication between regions, and its cost on every write | 6.6 |
| To be back within hours | A restore that has been timed | 2.9 |
| To be back within minutes | A standby that is running; a failover that is practised | 5.1, 6.6 |
| Not to stop at all when a zone fails | Capacity already in the other zones | 6.5 |

Three things about using them.

**Ask for each part separately.** The order history and the payment ledger do not need the same answer, and giving everything the strictest one is how budgets are spent without making the important part safe.

**The recovery time is a sum**:

> time to notice + time to decide + time to act + time to check

Engineers tend to measure the third term, because it is the technical one. The first two are often longer. A restore that takes twenty minutes, in an incident in which it took forty to conclude that a restore was needed, has a recovery time of an hour.

**An objective that has not been measured is a hope.** The recovery time of a system is the time that its last drill took.

### 2. A plan is a procedure that has been run

A recovery plan worth having reads like a deploy script with commentary. For each step: the command, what it should print, how to tell that it worked, and what to do if it did not. And around the steps:

- **Who decides**, on what evidence, and who is next if they cannot be reached.
- **Where the plan is kept**: not in the system that it recovers. Nor are the tools and passwords that it needs.
- **The order.** Systems depend on one another, and after a total loss there is a first thing that must come up with nothing else available. Find the circles: the deploy tool that needs the service registry, which is deployed by the deploy tool.
- **How to get in when the normal way is down**: an emergency account that does not depend on the single sign-on that may be part of the outage, and whose use sets off an alarm (Chapter 8.6).

Plans fail for a small number of reasons, and time causes most of them.

| Why the plan fails | What happened |
|---|---|
| **Drift** | The system changed after the plan was written |
| **Scale** | The data grew. A restore takes time in proportion to it |
| **Access** | The credentials, the person or the second factor are not available |
| **Circles** | A step needs the thing that has failed |
| **People** | The one person who knows is asleep, or has left |

The only defence against all five is to carry the plan out, on a schedule: the restore drill of Chapter 2.9, timed, into an empty environment; the failover of Chapter 6.6, in both directions; and at least once, the whole thing from nothing but the backups and the code.

Google's SRE book has an account of what that buys. In 2012 a faulty deletion job removed the audio for about 600,000 tracks from Google Music, and the recovery meant reading thousands of backup tapes. The team had one thing in its favour: "this recovery effort occurred just weeks after the company's annual disaster recovery testing exercise". The people who ran the tape system "already knew the capabilities and limitations of their subsystems that had been the subjects of DiRT tests and began dusting off a new tool they'd tested during a DiRT exercise" ([Google SRE book, *Data Integrity*](https://sre.google/sre-book/data-integrity/)). DiRT is Google's name for that annual exercise.

### 3. Fault injection: an experiment, not a stunt

A drill tests a procedure. A **fault-injection experiment** tests a design: it breaks one thing, on purpose, and checks that the system does what its designers said it would. The practice is usually called chaos engineering, and its definition is sober: "Chaos Engineering is the discipline of experimenting on a system in order to build confidence in the system's capability to withstand turbulent conditions in production" ([*Principles of Chaos Engineering*](https://principlesofchaos.org/)).

The same document gives the method, in four steps: "Start by defining 'steady state' as some measurable output of a system that indicates normal behavior. Hypothesize that this steady state will continue in both the control group and the experimental group. Introduce variables that reflect real world events like servers that crash, hard drives that malfunction, network connections that are severed, etc. Try to disprove the hypothesis by looking for a difference in steady state between the control group and the experimental group" ([*Principles of Chaos Engineering*](https://principlesofchaos.org/)).

Each step is a decision.

**The steady state** is something that users would notice: pages served in time, orders accepted. It is not processor load. Chapter 6.1 is the reason: a service can be busy and useless.

**The hypothesis** is specific enough to be wrong. *The system is resilient* cannot be disproved. Slack requires of its exercises "a specific hypothesis explaining how the failure will be experienced by upstream and downstream systems and by Slack clients" ([Slack Engineering](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)): which requests slow down, by how much, for how long, and which do not notice.

**The fault** is one that really happens. The best-known tool does the simplest thing: "Chaos Monkey is responsible for randomly terminating instances in production to ensure that engineers implement their services to be resilient to instance failures" ([Netflix, *Chaos Monkey*](https://netflix.github.io/chaosmonkey/)). Killing a server is the easy fault, though, and Chapter 6.1 said so: slow is worse than dead. A useful list is longer.

| Fault | What it tests |
|---|---|
| A process or a server is killed | Health checks, restarts, the balancer |
| A dependency is **slow** | Timeouts, bulkheads, deadlines (6.2, 6.3) |
| A dependency returns errors | Breakers, degraded answers, retry budgets (6.2, 6.3) |
| More traffic than the service can do | Limits and shedding (6.4) |
| A zone is unreachable | Placement, spare capacity, static stability (6.5) |
| A region is lost | Failover, and failing back (6.6) |
| A certificate expires; a clock is wrong; a disk fills | The alerts, mostly |

**The guard** is what keeps an experiment from being an outage. The principles are plain about the duty: "it is the responsibility and obligation of the Chaos Engineer to ensure the fallout from experiments are minimized and contained" ([*Principles of Chaos Engineering*](https://principlesofchaos.org/)). In practice: begin where a mistake is cheap; in production, begin with one server, one cell or one per cent of traffic; decide beforehand the measurement at which the experiment stops; and have a way to remove the fault in a second.

**Why in production at all?** Because that is where the system is. The [principles](https://principlesofchaos.org/) again: "Systems behave differently depending on environment and traffic patterns". A test environment has different data, different traffic, a different configuration, and none of the eleven services that were added last year. But production is the last place to try something, not the first. An experiment that you expect to fail is not an experiment. It is an outage with a calendar entry: fix the fault that you already know about first.

### 4. Game days: testing the people and the tools as well

A fault-injection experiment can run unattended. A **game day** is the version with people in it: a scheduled exercise in which a failure is caused, or simulated, and the team that would handle it does so. AWS's guidance: "Conduct game days to regularly exercise your procedures for responding to workload-impacting events and impairments. Involve the same teams who would be responsible for handling production scenarios" ([AWS Well-Architected, *Conduct game days regularly*](https://docs.aws.amazon.com/wellarchitected/latest/reliability-pillar/rel_testing_resiliency_game_days_resiliency.html)).

What a game day tests that an automated experiment does not:

- **Did the alert fire**, and reach somebody?
- **Did the dashboards show** what was wrong, or only that something was?
- **Was the runbook right?**
- **Did people know who was in charge** (Chapter 7.6)?

Slack's list of questions for the review afterwards is worth copying whole: "What was the time to detect and time to resolve? Did any users notice? Did any humans have to intervene? What was terrifying? Was any of our documentation wrong?" ([Slack Engineering](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)).

Two rules make game days useful and not theatre.

**A finding is a bug.** It gets an owner and a date, and the experiment is run again after the fix. A game day that produces a document and no changes has tested nothing.

**They are repeated.** The system that passed last year is not the system that is running now. Slack: "As the sands shift, those initial test results lose value" ([Slack Engineering](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)).

### 5. What to break, in order

For a team that has done none of this, a sensible order is from cheap and likely to expensive and rare:

1. **Kill one server** of each service, in production, in working hours. If this is frightening, everything below is premature.
2. **Make each dependency slow, and then failing**, one at a time: first in a test environment, then for a small share of production traffic.
3. **Restore the database** into an empty environment, and time it (Chapter 2.9).
4. **Take a zone out of service** (Chapter 6.5).
5. **Fail over to the other region, and back** (Chapter 6.6).
6. **Do one of the above without warning the people on call.** The alert, the runbook and the hand-over are part of the system.

---

## A real example

### Run it: four experiments, two findings, one fix

[`code/ch07-gameday/main.go`](code/ch07-gameday/main.go), about 310 lines, is a small game day with real HTTP servers inside one process. A shop has 20 workers and two pages: the cart, which needs nothing else; and the checkout, which needs a payments service and shows recommendations if it can. Its authors had read Chapter 6.3, and protected the call to recommendations with a short timeout and a bulkhead.

Each experiment states a hypothesis, measures the steady state for a second, injects a fault for two seconds, and measures again. A guard checks every quarter of a second: if fewer than half of the recent requests are meeting the standard, it removes the fault at once.

```bash
go run ./phase-6-reliability/code/ch07-gameday
```

```text
A shop with 20 workers. 100 users a second ask for the cart, and 100 for the checkout. Each experiment:
one second of normal running, a fault for two seconds, and a second and a half to recover.

ROUND 1 — the shop as it is

  1. Recommendations takes 2 s to answer.
    hypothesis: checkout does not notice: 99% of checkout pages are served within 200 ms
    before the fault       100.0%
    during it              100.0%
    after it               100.0%
    the hypothesis held

  2. Recommendations answers every request with an error.
    hypothesis: checkout does not notice: 99% of checkout pages are served within 200 ms
    before the fault       100.0%
    during it              100.0%
    after it               100.0%
    the hypothesis held

  3. Payments takes 2 s to answer.
    hypothesis: the cart does not notice: 99% of cart pages are served within 200 ms
    before the fault       100.0%
    during it               25.3%   the guard removed the fault after 0.8 s
    after it               100.0%
    DISPROVED

  4. Payments takes 2 s to answer.
    hypothesis: checkout fails quickly: 99% of checkout requests are answered, yes or no, within 200 ms
    before the fault       100.0%
    during it                0.0%   the guard removed the fault after 0.5 s
    after it               100.0%
    DISPROVED
```

**Experiments 1 and 2 confirm what was designed.** The protection around recommendations works, for slowness and for errors. That is worth knowing, and it was the uninteresting half.

**Experiment 3 finds something that nobody designed.** The cart does not use payments. But the cart and the checkout share the shop's 20 workers, and the call to payments has no limit of its own: when payments is slow, each checkout holds a worker for as long as its user waits, and within a second there is no worker left for a cart. Three cart pages in four were lost. It is row (a) of Chapter 6.3's example again. Recommendations had been protected because it was optional. Nobody had thought of protecting payments, because the checkout cannot work without it.

**Experiment 4 asks a different question of the same fault**, and the answer is worse. A checkout without payments must fail, and nobody disputes that. The hypothesis was about *how*: quickly, so that the user is told, and the worker is released. Not one checkout request was answered within 200 ms.

**The guard did its work.** Both faults were planned to last two seconds, and were withdrawn after 0.8 and 0.5. In production those would be the seconds of real users.

The findings are bugs. They are fixed — the call to payments gets a timeout of 100 ms and a bulkhead of six — and the two experiments are run again:

```text
ROUND 2 — after a change: a call to payments may take 100 ms and no longer, and at most 6 may be in progress

  3. Payments takes 2 s to answer.
    hypothesis: the cart does not notice: 99% of cart pages are served within 200 ms
    before the fault       100.0%
    during it              100.0%
    after it               100.0%
    the hypothesis held

  4. Payments takes 2 s to answer.
    hypothesis: checkout fails quickly: 99% of checkout requests are answered, yes or no, within 200 ms
    before the fault       100.0%
    during it              100.0%
    after it               100.0%
    the hypothesis held
```

The value of the exercise was in the hypothesis that failed. A design review had looked at this service and seen the protected dependency. The experiment looked at what users got.

### How Slack runs a game day

Slack's engineers describe a process that they have run dozens of times, and all the quotations here are from that account ([Slack Engineering, *Disasterpiece Theater*](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)).

**Before.** The hosts write down the fault, the hypothesis, and what to watch: "all the logs, metrics, and alerts that should be monitored, as well as runbooks that may be necessary" ([Slack Engineering](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)). The plan is reviewed by people who know the systems concerned. One serious fault, a cache that could serve stale data after losing and regaining its place, was found at this stage, in the review, before anything had been broken.

**In the test environment first.** "Exercises always begin by inciting the failure in dev." And stopping there is a respectable outcome: "The exercise isn't considered a failure or a waste of time if we don't proceed to production; in fact, some of our most valuable lessons have come from the development environment" ([Slack Engineering](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)).

**Then in production**, announced in a channel that hundreds of engineers read, with the dashboards on a shared screen, and with one instruction that is harder to follow than it sounds: to watch, and not to fix. They wait to see whether the system repairs itself as designed.

**Afterwards**, the review, with the questions quoted in section 4.

What it has produced, in their words: "The majority of them have gone roughly according to plan, expanding our confidence in existing systems and proving the correct functioning of new ones. Some, however, have identified serious vulnerabilities to the availability or correctness of Slack and given us the opportunity to fix them before impacting customers" ([Slack Engineering](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/)).

---

## Trade-offs

**An experiment can cause the outage that it is looking for.** The guard, the small beginning and the working hours all reduce that risk, and none removes it. The comparison is with finding the same fault at three in the morning, at full size, with nobody expecting it.

**Production against a test environment.** A test environment is safe, and different from production in ways that nobody has listed. Most of the value is in production, and most of the *first* findings are in the test environment, cheaply.

**Drills take people's time**, which is the most contested resource there is. A quarter of an hour's fault injection can be automated and run weekly. A regional failover with twenty people cannot. Spend the people on the exercises that test people.

**Automation against attention.** An automated experiment that runs every week, and that nobody looks at, turns into a monitor of the things it happens to cover. Somebody must still ask what has changed, and what is not covered.

**Not everything can be rehearsed.** The loss of a whole provider, or a fault in the recovery tooling itself, may be too costly to try in full. Rehearse the parts, and say plainly which parts have never been run together.

---

## Practice

### Check yourself

1. A team's recovery time objective for its database is 30 minutes. A restore takes 25. Alerts are checked every 15 minutes by a person; the decision to restore needs a manager's approval; and after the restore, an engineer compares row counts by hand. Is the objective met?
2. Which of these is a usable hypothesis for a fault-injection experiment, and what is wrong with the others? (a) *The system tolerates the failure of the cache.* (b) *If one cache server is stopped, 99.9% of page requests are still served within 500 ms, and the database's load rises by less than 20%.* (c) *If the cache is stopped, CPU use stays below 80%.*
3. An engineer proposes the team's first experiment in production: disconnecting the primary database for a minute, to see what happens. What would you say?

<details>
<summary>Answers</summary>

1. Almost certainly not. The recovery time is the sum: up to 15 minutes to notice, some minutes to find and persuade a manager, 25 to restore, and some to check. That is nearer an hour. The cheapest improvements are in the terms that are not the restore: an alert that pages someone at once, a decision that the on-call engineer may take alone when stated conditions are met, and a check that is a script.
2. (b). It names a fault, a measurement that users would notice, a number, and a side effect to watch. (a) cannot be disproved: *tolerates* is not a measurement. (c) is measurable, and measures the wrong thing: a service can keep its CPU below 80% while serving no pages in time (Chapter 6.1).
3. That *to see what happens* is not a hypothesis, and that the primary database is not the place to start. Ask first what the team expects. If nobody can say, the design should be read before it is broken. If the expectation is that it will go badly, fix that first. Then begin smaller — one application server, in a test environment, then in production — with a stated prediction, a guard, and a way to stop. The database failover is worth doing: later, in a test environment first, and as a planned exercise with the people who would handle it.

</details>

### Exercise: plan an experiment

Take Capstone 5's chat system: gateways that hold connections, an API, shards with a leader and two followers in three zones, a fan-out from each shard's change stream, and a presence store.

Plan a game day of three experiments. For each: the fault; the steady state and how it is measured; the hypothesis, with numbers; the guard; and where it is run first.

<details>
<summary>Worked solution</summary>

**Experiment 1: a gateway is killed.**

- *Fault*: stop one gateway server, with its connections.
- *Steady state*: the share of sent messages that are shown on every recipient's device within 2 seconds, as measured by the devices.
- *Hypothesis*: the devices that were connected to it reconnect within 30 seconds, spread over that time and not all at once, and each fetches what it missed. No message is lost, and none is shown twice. For devices on other gateways, the steady state does not change. The API's request rate rises by less than 20% during the reconnections.
- *Guard*: stop if the steady state for *other* gateways' users falls below 99%. This fault cannot be withdrawn, so the guard's action is to stop a second gateway from being killed, and to page.
- *First*: in the test environment, with a few thousand simulated devices. Then one gateway in production, at a quiet hour.

**Experiment 2: a shard's leader is cut off from its followers.**

- *Fault*: block the network between one leader and both followers, leaving the leader reachable by the API.
- *Steady state*: sends confirmed within 500 ms, for each shard separately; and, checked afterwards, every confirmed message present on the shard's new leader.
- *Hypothesis*: the cut-off leader confirms nothing after the fault begins (Chapter 5.1). Within 60 seconds a follower is leading. Sends to that shard fail or wait meanwhile, and succeed on retry with the same ID, with no duplicates. The other fifteen shards do not notice. When the block is removed, the old leader rejoins as a follower.
- *Guard*: stop if any *other* shard's sends fall below 99%, or if the old leader is seen to confirm a write.
- *First*: in the test environment, repeatedly, until it is dull. This is the experiment most likely to find something, and what it finds would be lost messages.

**Experiment 3: the presence store is lost.**

- *Fault*: make the presence store unreachable for five minutes.
- *Steady state*: the same message measure as experiment 1.
- *Hypothesis*: messages are entirely unaffected, since nothing on the path of a message depends on presence. Every user is shown as offline. Within 60 seconds of the store returning, the green dots are right again, with no burst of load above twice the normal rate of heartbeats.
- *Guard*: stop if the message measure falls at all. If it does, the hypothesis is disproved in the most useful way: something on the message path depends on presence, and nobody knew.
- *First*: in the test environment; then in production, for one partition of the presence store.

**For the day itself**: one person runs it, and another only watches the dashboards. The experiments run in this order, cheapest first. Each finding gets an owner before the next experiment starts. And the review asks Slack's questions, most of all the last: was any of the documentation wrong?

</details>

---

## Production tips and common mistakes

- **Write the recovery point and recovery time down for each part of the system**, with the business, and review them when the system changes.
- **Time every drill.** The measured figure is the recovery time. Plot it, because it grows with the data.
- **Keep the plan, the tools and the emergency access outside what they recover**, and test the emergency access too.
- **Every experiment has a hypothesis, a steady state that users would feel, a guard, and a way to stop.**
- **Start small and in working hours**, with the people who built the thing present.
- **Announce it.** An experiment that is mistaken for an incident wastes the response. An incident that is mistaken for an experiment is worse.
- **Turn findings into fixes, and run the experiment again.**
- **Automate what has become dull**, and keep the people for what has not been tried.

**Common mistakes**

- **A plan that has been reviewed and never run.**
- **Measuring the restore and calling it the recovery time.**
- **A hypothesis that cannot fail.**
- **Processor load as the steady state.**
- **Breaking something large first**, to see what happens.
- **No guard**, or a guard that depends on the system under test.
- **Testing only what is expected to pass.** The example's first two experiments held, and a team that had stopped there would have reported that the shop was protected.
- **A game day with no follow-up.**

---

## Key takeaways

- Recovery point and recovery time are requirements, set for each part of a system, and they choose its design.
- The recovery time is noticing, deciding, acting and checking, added together. The technical step is seldom the longest.
- A plan fails through drift, scale, access, circles and people. Only carrying it out finds these, and only doing so regularly keeps it true.
- A fault-injection experiment has a steady state that users would feel, a hypothesis that can be wrong, a fault that really occurs, and a guard that stops it.
- Slow dependencies and lost zones are more informative faults than a killed server.
- Game days test the alerts, the dashboards, the runbooks and the people. Their findings are bugs, to be fixed and tested again.
- The useful result is the hypothesis that fails. Test what you believe is protected.

**Next:** [Capstone 6: a payments API](capstone-payments-api.md) — a service that survives the failure of its dependencies and of a zone: deadlines, idempotency, budgets, breakers, limits and spare capacity, in one service that moves money.

---

## Sources

- [*Principles of Chaos Engineering*](https://principlesofchaos.org/), 2019.
- Richard Crowley, [*Disasterpiece Theater: Slack's process for approachable Chaos Engineering*](https://slack.engineering/disasterpiece-theater-slacks-process-for-approachable-chaos-engineering/), Slack Engineering, 2019.
- Google SRE book, chapter 26, [*Data Integrity: What You Read Is What You Wrote*](https://sre.google/sre-book/data-integrity/), 2016.
- Netflix, [*Chaos Monkey*](https://netflix.github.io/chaosmonkey/), documentation.
- AWS Well-Architected Framework, Reliability Pillar, [*Conduct game days regularly*](https://docs.aws.amazon.com/wellarchitected/latest/reliability-pillar/rel_testing_resiliency_game_days_resiliency.html).
