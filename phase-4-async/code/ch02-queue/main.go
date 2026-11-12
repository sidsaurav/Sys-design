// Chapter 4.2 — a queue that does not forget, built from one table.
//
// The queue is a table in PostgreSQL, so every part of it can be read: how
// a job is handed out, how it is hidden while someone works on it, what
// happens when that someone dies, and where a job goes when it cannot be done.
//
//	part 1  workers die in the middle of a job: three ways to hand jobs out
//	part 2  a job takes longer than the queue expected
//	part 3  a job that can never succeed
//	part 4  jobs that must be done in order
//
// Needs the database from Phase 2. The connection string comes from
// DATABASE_URL and defaults to postgres://postgres:postgres@localhost:5432/postgres.
//
//	go run ./phase-4-async/code/ch02-queue           # about 40 seconds
//	go run ./phase-4-async/code/ch02-queue -part 3
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ctx  = context.Background()
	pool *pgxpool.Pool
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

const schema = `
DROP TABLE IF EXISTS q_jobs, q_dead, q_accounts;
CREATE TABLE q_jobs (
    id         bigserial PRIMARY KEY,
    key        text,                              -- jobs with the same key must be done in order (part 4)
    payload    text NOT NULL,
    attempts   int NOT NULL DEFAULT 0,            -- how many times it has been handed out
    visible_at timestamptz NOT NULL DEFAULT now() -- it is not handed out before this moment
);
CREATE INDEX q_jobs_by_key ON q_jobs (key, id);
CREATE TABLE q_dead (id bigint, payload text, attempts int, error text, died_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE q_accounts (id int PRIMARY KEY, value int NOT NULL DEFAULT 0);`

// Three ways to hand a job to a worker. Each takes the oldest job that is
// visible; FOR UPDATE SKIP LOCKED lets many workers ask at once without
// waiting for each other or being given the same row.
const (
	// Forget the job the moment it is handed out.
	takeAndDelete = `
DELETE FROM q_jobs
WHERE id = (SELECT id FROM q_jobs WHERE visible_at <= now() ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id, payload, attempts + 1`

	// Hide the job for a while. If nobody says it is done, it reappears.
	takeAndHide = `
UPDATE q_jobs SET attempts = attempts + 1, visible_at = now() + make_interval(secs => $1)
WHERE id = (SELECT id FROM q_jobs WHERE visible_at <= now() ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id, payload, attempts`

	// The same, but a job is handed out only when no earlier job with the
	// same key is still in the table.
	takeInOrder = `
UPDATE q_jobs SET attempts = attempts + 1, visible_at = now() + make_interval(secs => $1)
WHERE id = (SELECT j.id FROM q_jobs j
            WHERE j.visible_at <= now()
              AND NOT EXISTS (SELECT 1 FROM q_jobs earlier WHERE earlier.key = j.key AND earlier.id < j.id)
            ORDER BY j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1)
RETURNING id, payload, attempts`
)

type job struct {
	id       int64
	payload  string
	attempts int
}

func done(j job) { // acknowledge: the job is finished, forget it
	_, err := pool.Exec(ctx, "DELETE FROM q_jobs WHERE id = $1", j.id)
	must(err)
}

func hide(j job, seconds float64) { // keep it hidden for this long from now
	_, err := pool.Exec(ctx, "UPDATE q_jobs SET visible_at = now() + make_interval(secs => $2) WHERE id = $1", j.id, seconds)
	must(err)
}

func bury(j job, why error) { // give up: move it to where a person will look
	_, err := pool.Exec(ctx, `WITH gone AS (DELETE FROM q_jobs WHERE id = $1 RETURNING id, payload, attempts)
	                          INSERT INTO q_dead (id, payload, attempts, error) SELECT id, payload, attempts, $2 FROM gone`, j.id, why.Error())
	must(err)
}

func fill(n int, payload func(i int) (key, payload string)) {
	_, err := pool.Exec(ctx, schema)
	must(err)
	for i := 1; i <= n; i++ {
		key, p := payload(i)
		_, err := pool.Exec(ctx, "INSERT INTO q_jobs (key, payload) VALUES ($1, $2)", key, p)
		must(err)
	}
}

// ------------------------------------------------------------ workers

var errDied = errors.New("the worker died") // it stops without a word: no acknowledgement, no error report

type workers struct {
	count       int
	take        string        // which of the three statements
	lease       float64       // seconds a job stays hidden once handed out
	heartbeat   time.Duration // if set, a working worker keeps extending the lease this often
	maxAttempts int           // if set, a job that has failed this many times is buried; before that it waits longer each time
	work        func(j job) error
	stop        func() bool
}

// run starts the workers and returns when stop says so.
func (w workers) run() {
	var wg sync.WaitGroup
	var worker func()
	worker = func() {
		defer wg.Done()
		for !w.stop() {
			var j job
			var err error
			if w.take == takeAndDelete {
				err = pool.QueryRow(ctx, w.take).Scan(&j.id, &j.payload, &j.attempts)
			} else {
				err = pool.QueryRow(ctx, w.take, w.lease).Scan(&j.id, &j.payload, &j.attempts)
			}
			if err != nil { // nothing to do at the moment
				time.Sleep(10 * time.Millisecond)
				continue
			}
			finished := make(chan struct{})
			if w.heartbeat > 0 {
				go func() { // still working on it: keep it hidden
					for {
						select {
						case <-finished:
							return
						case <-time.After(w.heartbeat):
							hide(j, w.lease)
						}
					}
				}()
			}
			err = w.work(j)
			close(finished)
			switch {
			case err == nil:
				if w.take != takeAndDelete {
					done(j)
				}
			case errors.Is(err, errDied):
				wg.Add(1)
				go worker() // a new worker is started in its place
				return
			case w.take == takeAndDelete: // nothing to retry: the job has already been forgotten
			case w.maxAttempts > 0 && j.attempts >= w.maxAttempts:
				bury(j, err)
			case w.maxAttempts > 0:
				hide(j, 0.1*float64(int(1)<<(j.attempts-1))) // wait 0.1 s, 0.2 s, 0.4 s...
			default:
				hide(j, 0) // try again at once
			}
		}
	}
	for i := 0; i < w.count; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
}

// effects counts how many times each job's work actually happened.
type effects struct {
	mu    sync.Mutex
	times map[int64]int
}

func (e *effects) happened(id int64) {
	e.mu.Lock()
	e.times[id]++
	e.mu.Unlock()
}

func (e *effects) count() (once, more int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range e.times {
		if n == 1 {
			once++
		} else {
			more++
		}
	}
	return once, more
}

func left() (n int) {
	must(pool.QueryRow(ctx, "SELECT count(*) FROM q_jobs").Scan(&n))
	return n
}

// ------------------------------------------------------------ part 1

func part1() {
	fmt.Println("PART 1 — 200 jobs, 4 workers. Three times, a worker dies in the middle of a job:")
	fmt.Println("         twice before the job's work has happened, and once just after.")
	fmt.Println()
	fmt.Printf("  %-46s %10s %12s %12s %20s\n", "a job, when it is handed to a worker, is...", "done once", "done twice", "never done", "still in the queue")

	for _, v := range []struct {
		name  string
		take  string
		lease float64
	}{
		{"deleted from the queue", takeAndDelete, 0},
		{"hidden, for good", takeAndHide, 365 * 24 * 3600},
		{"hidden for one second", takeAndHide, 1},
	} {
		fill(200, func(i int) (string, string) { return "", fmt.Sprint("job ", i) })
		e := &effects{times: map[int64]int{}}
		var taken atomic.Int64
		begin := time.Now()
		workers{count: 4, take: v.take, lease: v.lease,
			work: func(j job) error {
				n := taken.Add(1)
				time.Sleep(5 * time.Millisecond)
				if n == 40 || n == 120 {
					return errDied // before the work
				}
				e.happened(j.id)
				time.Sleep(5 * time.Millisecond)
				if n == 80 {
					return errDied // after the work, before saying so
				}
				return nil
			},
			stop: func() bool {
				once, more := e.count()
				return once+more == 200 || time.Since(begin) > 3*time.Second
			},
		}.run()
		once, more := e.count()
		fmt.Printf("  %-46s %10d %12d %12d %20d\n", v.name, once, more, 200-once-more, left())
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — 100 jobs, 8 workers. Most jobs take 50 ms; one in ten takes 1.2 seconds. One worker dies.")
	fmt.Println()
	fmt.Printf("  %-46s %22s %30s\n", "a job is hidden for...", "done more than once", "the dead worker's job waited")

	for _, v := range []struct {
		name      string
		lease     float64
		heartbeat time.Duration
	}{
		{"half a second", 0.5, 0},
		{"three seconds", 3, 0},
		{"half a second, renewed while work goes on", 0.5, 200 * time.Millisecond},
	} {
		fill(100, func(i int) (string, string) {
			if i%10 == 0 {
				return "", "slow"
			}
			return "", "quick"
		})
		e := &effects{times: map[int64]int{}}
		var taken atomic.Int64
		var orphan atomic.Int64 // the job that the dead worker was holding
		var diedAt, retakenAt time.Time
		workers{count: 8, take: takeAndHide, lease: v.lease, heartbeat: v.heartbeat,
			work: func(j job) error {
				if j.id == orphan.Load() && retakenAt.IsZero() {
					retakenAt = time.Now()
				}
				if taken.Add(1) == 20 {
					orphan.Store(j.id)
					diedAt = time.Now()
					return errDied
				}
				if j.payload == "slow" {
					time.Sleep(1200 * time.Millisecond)
				} else {
					time.Sleep(50 * time.Millisecond)
				}
				e.happened(j.id)
				return nil
			},
			stop: func() bool { return left() == 0 },
		}.run()
		_, more := e.count()
		fmt.Printf("  %-46s %22d %30s\n", v.name, more, fmt.Sprintf("%.1f s", retakenAt.Sub(diedAt).Seconds()))
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — 100 jobs, 4 workers. One job can never succeed: its payload cannot be read.")
	fmt.Println()
	fmt.Printf("  %-46s %18s %24s %14s\n", "when a job fails, it is...", "other jobs done", "attempts at the bad job", "where it is")

	for _, v := range []struct {
		name        string
		maxAttempts int
	}{
		{"tried again at once, for ever", 0},
		{"tried again after a growing wait, 5 times", 5},
	} {
		fill(100, func(i int) (string, string) {
			if i == 7 {
				return "", `{"user": 41, "email": ` // cut off in the middle
			}
			return "", `{"user": 41, "email": "ana@example.com"}`
		})
		e := &effects{times: map[int64]int{}}
		var attempts atomic.Int64
		begin := time.Now()
		workers{count: 4, take: takeAndHide, lease: 5, maxAttempts: v.maxAttempts,
			work: func(j job) error {
				if !strings.HasSuffix(j.payload, "}") {
					attempts.Add(1)
					return errors.New("unexpected end of JSON input")
				}
				time.Sleep(10 * time.Millisecond)
				e.happened(j.id)
				return nil
			},
			stop: func() bool { return time.Since(begin) > 3*time.Second },
		}.run()
		once, _ := e.count()
		where := "in the queue, still being tried"
		var dead int
		must(pool.QueryRow(ctx, "SELECT count(*) FROM q_dead").Scan(&dead))
		if dead == 1 {
			where = "set aside"
		}
		fmt.Printf("  %-46s %18d %24d   %s\n", v.name, once, attempts.Load(), where)
	}
	var id int64
	var payload, problem string
	var attempts int
	must(pool.QueryRow(ctx, "SELECT id, payload, attempts, error FROM q_dead").Scan(&id, &payload, &attempts, &problem))
	fmt.Printf("\n  SELECT * FROM q_dead:   id %d | %s | %d attempts | %s\n", id, payload, attempts, problem)
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — 1,000 jobs, 8 workers. Each job says \"set account A to value N\". For each account the values")
	fmt.Println("         were queued in order, 1 to 50, so every account should end at 50.")
	fmt.Println()
	fmt.Printf("  %-46s %26s %14s\n", "", "accounts with a wrong value", "jobs a second")

	for _, v := range []struct {
		name     string
		take     string
		accounts int
	}{
		{"20 accounts; any worker takes any job", takeAndHide, 20},
		{"20 accounts; one job per account at a time", takeInOrder, 20},
		{"1 account; one job per account at a time", takeInOrder, 1},
	} {
		per := 1000 / v.accounts
		fill(1000, func(i int) (string, string) {
			account, value := (i-1)/per+1, (i-1)%per+1 // an account's 50 changes are next to each other in the queue
			return fmt.Sprint(account), fmt.Sprint(account, " ", value)
		})
		_, err := pool.Exec(ctx, "INSERT INTO q_accounts SELECT g FROM generate_series(1, $1) g", v.accounts)
		must(err)
		begin := time.Now()
		workers{count: 8, take: v.take, lease: 5,
			work: func(j job) error {
				var account, value int
				fmt.Sscan(j.payload, &account, &value)
				time.Sleep(time.Duration(1+rand.Intn(4)) * time.Millisecond)
				_, err := pool.Exec(ctx, "UPDATE q_accounts SET value = $2 WHERE id = $1", account, value)
				return err
			},
			stop: func() bool { return left() == 0 },
		}.run()
		var wrong int
		must(pool.QueryRow(ctx, "SELECT count(*) FROM q_accounts WHERE value <> $1", per).Scan(&wrong))
		fmt.Printf("  %-46s %20d of %-3d %14.0f\n", v.name, wrong, v.accounts, 1000/time.Since(begin).Seconds())
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	flag.Parse()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	var err error
	pool, err = pgxpool.New(ctx, url+"?pool_max_conns=16")
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the database of Phase 2 first: see phase-2-data/README.md", url, err)
	}
	defer pool.Close()
	for i, run := range []func(){part1, part2, part3, part4} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
	_, err = pool.Exec(ctx, "DROP TABLE IF EXISTS q_jobs, q_dead, q_accounts")
	must(err)
}
