# 2.9 Durability, backups, and restore

[Syllabus](../SYLLABUS.md) · Phase 2 · Previous: [2.8 Choosing a datastore](08-choosing-a-datastore.md) · Next: [Capstone 2: ticket booking](capstone-ticket-booking.md)

> **Time:** about 45 minutes to read, 60 minutes of practice.
> **You need:** [Chapter 2.1](01-how-databases-store-data.md) (the write-ahead log) and [Chapter 2.6](06-database-as-shared-resource.md). The examples need PostgreSQL's server programs; with the Docker container from [the phase overview](README.md) each is one command.

**By the end of this chapter you can**

- say exactly what a database promises when it confirms a commit, and what weakens the promise;
- list the ways data is lost, and which defence covers which — and why a replica is not a backup;
- define RPO and RTO, and derive a backup design from them;
- explain logical backups, physical backups and point-in-time recovery, and what each can restore;
- run a restore drill, and explain why a backup that has never been restored does not count.

---

## The problem

On 31 January 2017 an engineer at GitLab, working on a replication fault, cleared the data directory on a database server so that replication could start again from scratch. It was the wrong server: "The database directory of the primary database was removed by accident, instead of removing the database directory of the secondary" ([GitLab, *Postmortem of database outage of January 31*](https://about.gitlab.com/blog/postmortem-of-database-outage-of-january-31/)).

A mistake like that is why backups exist, and GitLab had several kinds. Then they went to use them. [The postmortem](https://about.gitlab.com/blog/postmortem-of-database-outage-of-january-31/) again:

- The nightly logical backups: "When we went to look for the pg_dump backups we found out they were not there. The S3 bucket was empty, and there was no recent backup to be found anywhere." The backup job had been running the wrong version of the tool, and failing.
- The alerts for those failures were sent by email, and the emails were being rejected: "This means we were never aware of the backups failing, until it was too late."
- The replica could not help. It was the thing being repaired, and its data had already been wiped.
- What saved most of the data was luck: "A snapshot created manually by one of the engineers roughly 6 hours before the outage."

The cost, from [the same document](https://about.gitlab.com/blog/postmortem-of-database-outage-of-january-31/): "Database data such as projects, issues, snippets, etc. created between January 31st 17:20 UTC and 23:30 UTC has been lost", and the site was down for about 18 hours, much of it spent copying data across slow network disks. On why nobody had noticed earlier, the postmortem is blunt: "Why was the backup procedure not tested on a regular basis? - Because there was no ownership, as a result nobody was responsible for testing this procedure."

Nothing in that story is exotic. A person made a mistake, the backups had quietly stopped working, and the first real restore was attempted during the emergency. This chapter is about each of those links.

---

## The concept

### 1. What "committed" promises

Chapter 2.1 showed what a commit costs: the database writes the change to its write-ahead log (WAL) and forces the log to disk before it answers. That force is the promise. To see what it protects against, follow a write on its way down:

```mermaid
flowchart LR
    app["Application:<br/>COMMIT"] --> buf["Database memory<br/>(WAL buffers)"]
    buf -->|write| os["Operating system<br/>memory (page cache)"]
    os -->|"flush (fsync)"| disk["Disk"]
    disk -.->|"only then"| ok["'COMMIT' confirmed<br/>to the application"]
```

A change that has only reached the database's memory is lost if the database process dies. One that has reached the operating system's memory survives that, but not a power failure. Only what has been flushed to disk survives both. By default PostgreSQL confirms a commit after the last step, so a confirmed commit survives a crash of the database or of the machine.

Three things weaken the promise.

- **Asynchronous commit.** With `synchronous_commit = off` the database confirms first and flushes a moment later. It is much faster, and the manual is exact about the price: "The risk that is taken by using asynchronous commit is of data loss, not data corruption" — transactions confirmed in the last instants before a crash can vanish, within a window of up to "three times wal_writer_delay" (600 ms with default settings) ([PostgreSQL docs, *Asynchronous Commit*](https://www.postgresql.org/docs/current/wal-async-commit.html)). The first example measures exactly this.
- **Switching off flushing altogether.** `fsync = off` "can result in unrecoverable data corruption in the event of a power failure or system crash" ([PostgreSQL docs](https://www.postgresql.org/docs/current/runtime-config-wal.html)). Never on a database you want to keep.
- **Hardware that confirms a flush it has not done.** Some drives and controllers keep "flushed" data in a volatile cache; the manual has a page on it ([*Reliability*](https://www.postgresql.org/docs/current/wal-reliability.html)), and the PostgreSQL wiki records how surprising the operating system's side of this can be ([*Fsync Errors*](https://wiki.postgresql.org/wiki/Fsync_Errors)).

And the promise has an edge. It covers a crash of *this machine with its disk intact*. If the disk is destroyed, or someone deletes the data, the flushed commit is gone like everything else.

### 2. Five ways to lose data

Each needs a different defence, and no single defence covers them all.

| What goes wrong | For example | What saves you |
|---|---|---|
| **The process or the machine crashes** | Out of memory; power cut | The write-ahead log, flushed at commit (section 1) |
| **The disk or the machine is gone** | Hardware failure | A replica on another machine (Chapters 3.6 and 5.1) — or a backup, more slowly |
| **A person or a program destroys data** | `DELETE` without `WHERE`; a bad migration; a bug | A backup that can go back *in time* |
| **The whole site is gone** | Fire; a region-wide outage | A copy somewhere else |
| **Someone destroys it deliberately** | Ransomware; stolen credentials | A copy that those credentials cannot delete |

The third row is the one that catches teams out, because the defence everyone has — a replica — does nothing for it. A replica's job is to apply every change the primary makes, immediately. Google's SRE book: "replication and redundancy are not recoverability", because "Datastores that automatically sync multiple replicas guarantee that a corrupt database row or errant delete are pushed to all of your copies, likely before you can isolate the problem" ([*Site Reliability Engineering*, ch. 26](https://sre.google/sre-book/data-integrity/)).

It is also the common case. [The same chapter](https://sre.google/sre-book/data-integrity/): "When velocity is high and privacy matters, bugs in applications account for the vast majority of data loss and corruption events." Hardware fails now and then. Software changes every day.

> **A replica protects you from losing a machine. A backup protects you from yourself.** You need both.

### 3. Two numbers: RPO and RTO

Backups are designed backwards from two targets. AWS's definitions are the usual ones ([AWS Well-Architected Framework](https://docs.aws.amazon.com/wellarchitected/latest/reliability-pillar/disaster-recovery-dr-objectives.html)):

- **RPO, recovery point objective**: "the maximum acceptable amount of time since the last data recovery point." In plain words: *how much recent data may we lose?*
- **RTO, recovery time objective**: "the maximum acceptable delay between the interruption of service and restoration of service." *How long may we be down?*

```text
              last recovery point          disaster                    service restored
                      │                       │                               │
  ────────────────────●───────────────────────✖───────────────────────────────●────────►  time
                      └──── data lost ────────┘└────────── downtime ──────────┘
                         (must be ≤ RPO)               (must be ≤ RTO)
```

Both are business decisions with engineering prices. GitLab's numbers that day were about six hours of data and eighteen hours of downtime — and nobody had chosen either.

| If your recovery point is… | …the most you lose is |
|---|---|
| Last night's dump | Up to 24 hours |
| The latest archived WAL file | Minutes (section 4) |
| An asynchronous replica | Usually seconds — for a lost machine only |
| A synchronous replica (Chapter 5.1) | Nothing confirmed — for a lost machine only |

RTO is harder to know in advance, because the time to restore grows with the database. There is one way to find out, and it is section 5.

### 4. Three kinds of backup

**A logical backup** is the database written out as SQL: the statements that would recreate it. In PostgreSQL that is `pg_dump`. "Dumps created by pg_dump are internally consistent, meaning, the dump represents a snapshot of the database at the time pg_dump began running. pg_dump does not block other operations on the database while it is working" ([PostgreSQL docs, *SQL Dump*](https://www.postgresql.org/docs/current/backup-dump.html)). A dump is portable — it can be loaded into a newer version, or a single table can be pulled out of it. Its limits: it captures one moment, so everything since is lost; and restoring means inserting every row and rebuilding every index, which on a large database takes a long time.

**A physical backup** is a copy of the database's files (`pg_basebackup`). Restoring is copying them back, with no rows to re-insert and no indexes to rebuild. It is tied to the same major version, and it is the whole server or nothing.

**Continuous archiving** adds the piece that matters most. Besides an occasional physical *base backup*, the server copies every finished WAL file to an archive. Since the WAL records every change, base backup plus WAL can rebuild the database as it was at any moment: "this technique supports point-in-time recovery: it is possible to restore the database to its state at any time since your base backup was taken". The manual's example is the one this chapter is about: "If you want to recover to some previous point in time (say, right before the junior DBA dropped your main transaction table), just specify the required stopping point" ([PostgreSQL docs, *Continuous Archiving and Point-in-Time Recovery*](https://www.postgresql.org/docs/current/continuous-archiving.html)).

Point-in-time recovery (PITR) is the defence for row three of the table. It has two conditions to respect, both on [the same page](https://www.postgresql.org/docs/current/continuous-archiving.html):

- **No gaps.** "you need a continuous sequence of archived WAL files that extends back at least as far as the start time of your backup." One missing file, and recovery stops there.
- **Mind the last file.** "The archive command or function is only invoked on completed WAL segments", so on a quiet database the newest changes can sit unarchived for a long time. The setting `archive_timeout` puts a ceiling on that — and that ceiling is your RPO.

**Managed databases** do all of this for you, and their documentation tells you the numbers. Amazon RDS, for instance: "RDS uploads transaction logs for DB instances to Amazon S3 every five minutes", and "You can restore to any point in time within your backup retention period" ([Amazon RDS docs](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIT.html)). That is an RPO of about five minutes, bought with a setting. You still have to know the retention period, and you still have to practise the restore.

**Where the copies live** matters as much as how they are made. The old rule of thumb is 3-2-1: "Keep 3 copies of any important file: 1 primary and 2 backups", "Keep the files on 2 different media types", and "Store 1 copy offsite" ([Ruggiero and Heckathorn, *Data Backup Options*, US-CERT](https://www.cisa.gov/sites/default/files/publications/data_backup_options.pdf)). For a cloud system, translate "offsite" as *another region and another account*: a backup that the production credentials can delete is not a defence against the fifth row of the table. Object stores can also make backups undeletable for a set period — S3 Object Lock "can help prevent Amazon S3 objects from being deleted or overwritten for a fixed or variable amount of time, or indefinitely" ([Amazon S3 docs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html)).

### 5. A backup is a restore you have rehearsed

The SRE book puts the whole subject in one line: "No one really wants to make backups; what people really want are restores" ([ch. 26](https://sre.google/sre-book/data-integrity/)). GitLab's backup job existed, was scheduled, and had an alert. What it did not have was anyone who had ever restored from it.

So the unit of work is not "take a backup". It is a **restore drill**, run regularly and preferably automatically:

1. Restore the latest backup onto a scratch server — never over anything real.
2. Check that the result is usable: the tables are there, the row counts are plausible, the newest row is recent.
3. Record two numbers: how long it took (your real RTO) and how old the newest restored data is (your real RPO).
4. Alert if any of that fails — **including when the drill does not run at all**. Alerts that fire only when a job reports failure cannot see a job that has stopped running, or whose reports are not arriving.

Between drills, watch the inputs: the age of the newest successful backup; and for WAL archiving, PostgreSQL's `pg_stat_archiver` view, which reports the "Time of the most recent successful archive operation" and the "Number of failed attempts for archiving WAL files" ([PostgreSQL docs](https://www.postgresql.org/docs/current/monitoring-stats.html)). A WAL archive that has been failing for a week is a week-long hole in your recovery.

And give it an owner. The GitLab postmortem's answer to why the procedure was never tested was that nobody was responsible for it.

### 6. Before the backup: make destruction slow

A restore is the last resort; it is slow and it loses something. Cheaper defences sit in front of it. The SRE book describes layers, of which backups are only the second: "The first layer is soft deletion", which "has proven to be an effective defense against inadvertent data deletion scenarios" ([ch. 26](https://sre.google/sre-book/data-integrity/)). Chapter 2.5 covered soft deletes as a modelling choice; this is their operational value. A row that is marked deleted and purged a month later can be brought back with an `UPDATE`.

The same idea applies to people with a database console:

- Run any manual change inside a transaction, read the row count it reports, and only then `COMMIT`.
- Give day-to-day roles no permission to drop or truncate (Chapter 2.6).
- Make the prompt show which server you are on. GitLab's engineer was on the wrong one.

---

## A real example

Both examples start a throwaway PostgreSQL server in a temporary directory, on their own ports, and remove it when they finish. They need the server programs and must not run as root. With the Docker container from the phase overview, pipe the script into the container, as shown; with a local installation, run it with `bash` as an ordinary user.

(The scripts were tested on PostgreSQL 16 on Ubuntu, piped into `bash -s` as the `postgres` user — which is what the `docker exec` lines below do. Docker itself was not available where this chapter was written, so those exact lines are untested; the header of each script gives both ways to run it.)

### Run it: what survives a crash

[`code/ch09-durability/crash.sh`](code/ch09-durability/crash.sh) lets eight clients commit payments as fast as they can, kills the server without warning after three seconds, restarts it, and counts.

```bash
docker exec -i -u postgres course-pg bash -s < phase-2-data/code/ch09-durability/crash.sh
```

```text
Eight clients commit payments for three seconds. Then the server is killed and restarted.

  synchronous_commit = on      44983 confirmed to the clients     44990 in the table after the crash   none lost
  synchronous_commit = off    207027 confirmed to the clients    207014 in the table after the crash   13 LOST
```

- **On** (the default). Every payment that was confirmed is there. So are seven more: they were flushed to disk, but the server died before it could say so. That direction is always possible — it is the "did my request succeed?" uncertainty of Chapter 1.5, and the reason for idempotency keys — and it is the safe direction. *Confirmed* never becomes *missing*.
- **Off.** More than four times the throughput — and thirteen payments that the application was told were safe do not exist. In six runs on the test machine the number varied between 1 and 48.

Read that second line carefully before concluding that thirteen is small. This test kills the *database*, so the only commits lost are those PostgreSQL had not yet handed to the operating system. A power failure would also lose what the operating system had not yet flushed: up to the 600 ms window of section 1 which, at this rate of commits, is tens of thousands of payments.

Asynchronous commit is a legitimate tool: "The user can select the commit mode of each transaction, so that it is possible to have both synchronous and asynchronous commit transactions running concurrently" ([PostgreSQL docs](https://www.postgresql.org/docs/current/wal-async-commit.html)). Use it for page-view counters. Not for payments.

### Run it: a restore drill

[`code/ch09-restore/drill.sh`](code/ch09-restore/drill.sh) plays out a bad day on a small shop database, with WAL archiving switched on, and then restores it two ways.

```bash
docker exec -i -u postgres course-pg bash -s < phase-2-data/code/ch09-restore/drill.sh
```

```text
1. Last night
   the shop has 200,000 orders, worth $20,999,000
   logical backup  (pg_dump)        took 0.3 s, 1.5M
   physical backup (pg_basebackup)  took 0.7 s, 63M

2. The next day
   50,000 more orders arrive: 250,000 orders, worth $26,249,150

3. The mistake:  UPDATE orders SET total_cents = 0;   -- the WHERE clause was forgotten
   by the time anyone notices, 1,000 more orders have arrived: 251,000 orders, worth $104,795
   (the WAL archive holds 13 files, 193M)

4. Restore A — last night's dump, into a new database
   took 0.5 s:  200,000 orders, worth $20,999,000
   every order since the backup is missing

5. Restore B — the base backup, plus the archived WAL replayed up to 2026-11-11 22:47:48.479306+00
   took 1.7 s:  250,000 orders, worth $26,249,150
   exactly the state just before the mistake
   the 1,000 orders placed after the mistake are still in the damaged database: 1000 rows to copy across
```

**Restore A** is what a nightly dump gives you: the database as it was when the dump began. Fifty thousand orders — a day of business — are not in it. That is an RPO of up to 24 hours, seen from the wrong side.

**Restore B** is point-in-time recovery. The base backup is no newer than the dump — they were taken moments apart — but with the archived WAL it can be rolled forward to any instant, and the script stops it one second before the `UPDATE`. Every order is back, with its total. The recovery configuration is three settings added to the restored copy's `postgresql.conf`, plus an empty file named `recovery.signal` (the script also gives the copy its own port and switches archiving off):

```ini
restore_command = 'cp /path/to/wal-archive/%f %p'    # where to fetch archived WAL from
recovery_target_time = '2026-11-11 22:47:48.479306+00'   # stop replaying here
recovery_target_action = 'promote'                   # then open for business
```

Three things to take from it.

- **The restore went into a new directory, on a new port.** The damaged database was not touched. That is why the last line of the output is possible: the thousand orders placed *after* the mistake exist only there, and can be copied across. Restoring over the damaged original would have destroyed them — and, had the restore failed, everything else.
- **Choosing the moment is the hard part.** The script knows the exact time of the mistake. In a real incident you find it from logs, deploy records, and timestamps on rows, and you may restore more than once to get it right.
- **These timings mean nothing for your database** — this one is 60 MB. A logical restore re-inserts every row and rebuilds every index; a physical restore copies files and replays WAL. Both grow with the data, and GitLab's took 18 hours. The only way to know your number is to run the drill on your data.

---

## Trade-offs

**Durability costs throughput.** Flushing at every commit is the slowest thing a database does; relaxing it bought a factor of four here. The honest position is per kind of data: lose nothing confirmed for money and orders; accept a small window for data that can be recomputed or does not matter.

**Tighter RPO and RTO cost money and complexity.** A nightly dump is a cron job. Continuous archiving needs an archive, monitoring, and retention rules. A replica doubles the hardware. Pick the targets first — per system, since a product catalogue and a payments ledger do not need the same ones — and buy only what they require.

**Logical and physical backups are complements.** Physical with WAL gives fast restores and any point in time, for the whole server and the same version. Logical gives portability and single-table restores, for one moment in time. Many teams keep continuous archiving for disasters and an occasional dump for everything else.

**Keeping backups for longer protects against slow-burning damage, and costs storage — and sometimes compliance.** Corruption noticed after a month needs a month-old recovery point. But a backup also contains data that users have since asked you to delete. Decide the retention period deliberately, and write it down.

**Backing up loads the database.** A dump of a large database reads all of it. Take backups from a replica where you can, and outside peak hours where you cannot.

---

## Practice

### Check yourself

Answer before reading on.

1. A system has a primary database with default settings and one asynchronous replica in another zone. What is lost, and what recovers it, when (a) the primary's process is killed; (b) the primary's disk is destroyed; (c) a deploy runs `DELETE FROM users` with no `WHERE`?
2. A team takes a `pg_dump` every night at 02:00; restoring it takes three hours. A bad migration corrupts a table at 17:00 and is noticed at 17:30. What do they lose, and how long are they down, at best?
3. The backup job has reported success every night for a year. Give four reasons the backups might still be useless.

<details>
<summary>Answers</summary>

1. (a) **Nothing confirmed is lost.** The database restarts and replays its write-ahead log. (b) **The last moments of writes** — whatever the asynchronous replica had not yet received, usually under a second. Recovery is promoting the replica (Chapter 5.1). (c) **The replica is no help**: it deleted the same rows a moment later. Only a backup that can go back in time recovers them — point-in-time recovery to just before the deploy, or last night's dump with a day's changes lost.

2. They restore the 02:00 dump: every change since then is gone — **15 hours** of good data, plus whatever was written in the half-hour before anyone noticed, unless it can be salvaged from the damaged database. Downtime is **at least three hours** from the decision to restore, and realistically more: deciding, finding the dump, and checking the result all take time. Their real RPO is 24 hours and their real RTO is over three, whatever the design document says. With WAL archiving they could have recovered to 16:59.

3. Any four of: nobody has restored one, so nobody knows that it loads; the job backs up the wrong database, or an empty one; the dump is truncated or the WAL archive has a gap; it is encrypted with a key that has since been lost; it is stored on the same machine, or in the same account, as the database; restoring takes far longer than the business can tolerate; the tool's version no longer matches the server; "success" is the exit code of a wrapper script, not of the backup. Every one is found by a restore drill, and by nothing else.

</details>

### Exercise: a recovery plan

You run the database for an online shop: orders, payments, customers. It is 400 GB of PostgreSQL on one primary, in a cloud region with three zones. The business has decided:

- at most **5 minutes** of data may be lost, whatever happens;
- after a hardware failure the shop must be back within **15 minutes**;
- after a human or software error that damages data, within **4 hours**.

Design the plan:

1. Which mechanisms, and which of the five kinds of loss each one covers.
2. Where the backups are kept, and for how long.
3. What you monitor and alert on.
4. The restore drill: how often, and what it checks.
5. The first steps of the runbook for *a deploy has just corrupted the orders table*.

<details>
<summary>Worked solution</summary>

**1. Mechanisms.**

| Mechanism | Covers | Meets |
|---|---|---|
| Synchronous commit on the primary (the default) | Process and machine crashes | No confirmed data lost |
| A replica in a second zone, with automatic failover | Loss of a disk, a machine or a zone | 15-minute RTO; seconds of RPO. (For zero loss on failover, make it synchronous — Chapter 5.1 covers the price.) |
| Continuous WAL archiving, with `archive_timeout` of a minute or two, and a base backup every night | Human and software error; also a second line for hardware | 5-minute RPO for *any* failure; restore to any moment |
| A weekly logical dump | Restoring one table; moving between versions; a backup in a second format | — |
| Copies of the base backups and WAL in another region, in another account, undeletable for the retention period | Loss of the site; stolen credentials; ransomware | — |

The replica and the archive are not alternatives. The replica makes hardware failure a fifteen-minute event. The archive is the only thing here that can undo a mistake.

**2. Where, and how long.** Base backups and WAL go to object storage in the same region for fast restores, and are copied to the second region and account. Keep **30 days** of point-in-time recovery — damage is often noticed late — and monthly dumps for a year, if policy allows. Write the retention period down, with the reason, because it also determines how long deleted customer data survives.

**3. Monitoring.**

| Watch | Alert when |
|---|---|
| Age of the newest archived WAL file | More than 5 minutes — the RPO is being broken *now* |
| Failed archive attempts (`pg_stat_archiver`) | Any |
| Age of the newest base backup | More than 26 hours |
| Replica lag | More than a few seconds, sustained |
| Result and duration of the last restore drill | Failed, slower than the RTO, or **not run this week** |
| Free space in the WAL directory | Low — while archiving fails, the primary keeps its WAL files, and the directory "will continue to fill with WAL segment files until the situation is resolved" ([docs](https://www.postgresql.org/docs/current/continuous-archiving.html)) |

**4. The drill.** Weekly and automatic: start a scratch server; restore last night's base backup; replay WAL to a time a few minutes ago; then check that the orders table exists, that its row count is within a few per cent of production's, and that the newest order is less than ten minutes old. Record the duration. For 400 GB, expect the copy to dominate; if the drill takes three hours, the four-hour RTO leaves one hour for people to decide and verify — which is tight, and is exactly the kind of fact a drill exists to discover. Twice a year, do it by hand, with the people who would be on call, from the off-region copy.

**5. Runbook: a deploy has corrupted the orders table.**

1. **Stop the damage.** Roll back or disable the deploy; if writes are still corrupting data, put the affected feature into read-only or maintenance mode.
2. **Do not touch the damaged database.** No fixes in place, no restore over it. It is evidence, and it holds every legitimate write made since the mistake.
3. **Find the moment.** From the deploy log and the application's logs, establish the last time the data was known to be good. Write it down.
4. **Restore to a new server**, to just before that moment, from the base backup and the WAL archive. Check it: row counts, totals, a few known orders.
5. **Decide how to bring it back**: either switch the application to the restored server and copy across the good writes made after the mistake, or — if the damage is confined to one table or column — copy the correct values from the restored server into production. The second is often faster and loses nothing.
6. **Tell people** what was affected and from when to when. Afterwards, write the postmortem (Chapter 7.6): what made the mistake possible, and what would have caught it.

</details>

---

## Production tips and common mistakes

**Tips**

- **Decide RPO and RTO per system**, with the business, before choosing tools.
- **Keep `synchronous_commit` on** for anything you would be sorry to lose, and never turn `fsync` off.
- **Run a replica *and* point-in-time backups.** They cover different failures.
- **Keep a copy in another region and another account**, which production credentials cannot delete.
- **Automate a restore drill** and treat its failure — or its absence — as an incident.
- **Monitor backup age and WAL archiving**, and alert on silence as well as on errors.
- **Restore into a new place.** Never overwrite the damaged database.
- **Know your managed service's numbers**: retention period, how often logs are shipped, how long a restore of your size takes.
- **Give backups an owner**, by name.
- **Make deletion slow**: soft deletes for user data; transactions and row counts for manual changes.

**Common mistakes**

- **Believing that a replica makes backups unnecessary.**
- **Never having restored.** The first restore happens during the outage.
- **Backups in the same account, region or machine** as the database.
- **Alerting only on failure**, so that a job which has stopped running, or whose alerts are not delivered, looks healthy.
- **Nightly dumps as the only backup** for a system that cannot afford to lose a day.
- **A gap in the WAL archive** that nobody noticed, ending recovery at the gap.
- **Turning off durability for speed** on data that matters.
- **Fixing the damaged database in place**, and destroying the evidence and the recent writes.
- **An RTO taken from a design document** rather than from a measured restore.
- **Backups that nobody owns.**

---

## Key takeaways

- A confirmed commit has been flushed to disk and survives a crash. Asynchronous commit and unflushed storage weaken that; a lost disk ends it.
- Data is lost in five ways: crash, hardware loss, human or software error, site loss, and malice. Each needs its own defence.
- A replica copies mistakes faithfully. Only a backup that can go back in time protects against them.
- RPO is how much data you may lose; RTO is how long you may be down. Choose them, then design to them.
- A base backup plus an unbroken WAL archive restores the database to any moment. A dump restores it to the moment the dump was taken.
- A backup counts only once it has been restored. Drill regularly, measure the time, and alert when the drill does not run.
- Restore beside the damaged database, never over it.

**Next:** [Capstone 2: ticket booking](capstone-ticket-booking.md) — never sell the same seat twice.

---

## Sources

- GitLab, [*Postmortem of database outage of January 31*](https://about.gitlab.com/blog/postmortem-of-database-outage-of-january-31/), 2017.
- Betsy Beyer et al. (eds.), *Site Reliability Engineering*, O'Reilly, 2016 — chapter 26, [*Data Integrity: What You Read Is What You Wrote*](https://sre.google/sre-book/data-integrity/).
- PostgreSQL documentation: [SQL Dump](https://www.postgresql.org/docs/current/backup-dump.html); [Continuous Archiving and Point-in-Time Recovery](https://www.postgresql.org/docs/current/continuous-archiving.html); [Asynchronous Commit](https://www.postgresql.org/docs/current/wal-async-commit.html); [Write Ahead Log settings](https://www.postgresql.org/docs/current/runtime-config-wal.html); [Reliability](https://www.postgresql.org/docs/current/wal-reliability.html); [`pg_stat_archiver`](https://www.postgresql.org/docs/current/monitoring-stats.html). PostgreSQL wiki: [*Fsync Errors*](https://wiki.postgresql.org/wiki/Fsync_Errors).
- AWS Well-Architected Framework, Reliability Pillar: [*Disaster recovery (DR) objectives*](https://docs.aws.amazon.com/wellarchitected/latest/reliability-pillar/disaster-recovery-dr-objectives.html).
- Amazon RDS documentation: [*Restoring a DB instance to a specified time*](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIT.html). Amazon S3 documentation: [*Locking objects with Object Lock*](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html).
- Paul Ruggiero and Matthew A. Heckathorn, [*Data Backup Options*](https://www.cisa.gov/sites/default/files/publications/data_backup_options.pdf), US-CERT, 2012.
