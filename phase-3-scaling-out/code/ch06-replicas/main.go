// Chapter 3.6 — reading from replicas, and what replication lag does.
//
// Runs against a real PostgreSQL primary with two streaming replicas: one
// that keeps up as fast as it can, and one held 300 ms behind. Start them
// first (see cluster.sh in this directory):
//
//	bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start
//	go run ./phase-3-scaling-out/code/ch06-replicas
//	bash phase-3-scaling-out/code/ch06-replicas/cluster.sh stop
//
//	part 1  how far behind is each replica?
//	part 2  a user saves, then reloads: three ways of choosing where to read
//	part 3  one reader, two replicas: time running backwards
//
// The servers' addresses can be changed with PRIMARY_URL, FAST_REPLICA_URL
// and SLOW_REPLICA_URL.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ctx = context.Background()

func connect(env, fallback string) *pgxpool.Pool {
	url := os.Getenv(env)
	if url == "" {
		url = fallback
	}
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the servers first:  bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start", url, err)
	}
	return pool
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// caughtUp reports whether a replica has applied everything up to the
// given position in the primary's write-ahead log.
func caughtUp(replica *pgxpool.Pool, lsn string) (ok bool) {
	must(replica.QueryRow(ctx, "SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn", lsn).Scan(&ok))
	return ok
}

func waitFor(primary *pgxpool.Pool, replicas ...*pgxpool.Pool) {
	var lsn string
	must(primary.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn))
	for _, r := range replicas {
		for !caughtUp(r, lsn) {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

// ------------------------------------------------------------ part 1

// lag changes a row on the primary and then asks the replica for it, over
// and over, until the replica shows the change.
func lag(primary, replica *pgxpool.Pool) time.Duration {
	var written int
	must(primary.QueryRow(ctx, "UPDATE rp_profiles SET version = version + 1 WHERE id = 1 RETURNING version").Scan(&written))
	committed := time.Now()
	for {
		var seen int
		must(replica.QueryRow(ctx, "SELECT version FROM rp_profiles WHERE id = 1").Scan(&seen))
		if seen >= written {
			return time.Since(committed)
		}
	}
}

func part1(primary, fast, slow *pgxpool.Pool) {
	fmt.Println("PART 1 — a row is changed on the primary. How long until each replica shows the change? (30 tries)")
	fmt.Println()
	fmt.Printf("  %-34s %10s %10s %10s\n", "", "shortest", "median", "longest")
	for _, r := range []struct {
		name string
		pool *pgxpool.Pool
	}{{"the replica that keeps up", fast}, {"the replica held 300 ms behind", slow}} {
		var times []time.Duration
		for i := 0; i < 30; i++ {
			times = append(times, lag(primary, r.pool))
			time.Sleep(20 * time.Millisecond)
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		fmt.Printf("  %-34s %10s %10s %10s\n", r.name, ms(times[0]), ms(times[len(times)/2]), ms(times[len(times)-1]))
	}

	fmt.Println()
	fmt.Println("  What the primary itself reports (SELECT application_name, state, replay_lag FROM pg_stat_replication):")
	rows, err := primary.Query(ctx, `SELECT application_name, state, coalesce(replay_lag, '0')::text
	                                 FROM pg_stat_replication ORDER BY application_name`)
	must(err)
	for rows.Next() {
		var name, state, replayLag string
		must(rows.Scan(&name, &state, &replayLag))
		fmt.Printf("    %-8s %-12s replay_lag %s\n", name, state, replayLag)
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 2

// A router decides, for one read by one user, which server to ask.
type router struct {
	name   string
	choose func(u *user) (pool *pgxpool.Pool, fromPrimary bool)
}

type user struct {
	id        int
	wrote     int       // the version this user last saved
	wroteAt   time.Time // when
	wroteUpTo string    // the primary's log position just after that write
}

func part2(primary, replica *pgxpool.Pool) {
	fmt.Println("PART 2 — 150 users each save their profile and then load it 10 times over the next 3 seconds.")
	fmt.Println("         Reads go to the replica that is 300 ms behind, unless the router sends them to the primary.")
	fmt.Println()
	fmt.Printf("  %-66s %14s %22s\n", "where a read goes", "stale reads", "reads on the primary")

	routers := []router{
		{"always the replica", func(u *user) (*pgxpool.Pool, bool) {
			return replica, false
		}},
		{"the primary for 1 second after the user's own write", func(u *user) (*pgxpool.Pool, bool) {
			if time.Since(u.wroteAt) < time.Second {
				return primary, true
			}
			return replica, false
		}},
		{"the replica once it has the user's write; until then, the primary", func(u *user) (*pgxpool.Pool, bool) {
			if caughtUp(replica, u.wroteUpTo) {
				return replica, false
			}
			return primary, true
		}},
	}
	reloads := []time.Duration{20, 100, 200, 400, 700, 1000, 1500, 2000, 2500, 3000} // milliseconds after saving

	for _, rt := range routers {
		var stale, onPrimary, reads atomic.Int64
		var wg sync.WaitGroup
		for id := 1; id <= 150; id++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(id) * 7 * time.Millisecond) // users arrive one after another
				u := &user{id: id}
				// Save, and note how far the primary's log had got.
				must(primary.QueryRow(ctx, `UPDATE rp_profiles SET version = version + 1 WHERE id = $1 RETURNING version`, id).Scan(&u.wrote))
				must(primary.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&u.wroteUpTo))
				u.wroteAt = time.Now()

				for _, after := range reloads {
					time.Sleep(time.Until(u.wroteAt.Add(after * time.Millisecond)))
					pool, fromPrimary := rt.choose(u)
					var seen int
					must(pool.QueryRow(ctx, "SELECT version FROM rp_profiles WHERE id = $1", id).Scan(&seen))
					reads.Add(1)
					if fromPrimary {
						onPrimary.Add(1)
					}
					if seen < u.wrote {
						stale.Add(1) // the user is shown their profile as it was before they saved
					}
				}
			}()
		}
		wg.Wait()
		fmt.Printf("  %-66s %8d (%2d%%) %15d (%2d%%)\n", rt.name, stale.Load(), stale.Load()*100/reads.Load(),
			onPrimary.Load(), onPrimary.Load()*100/reads.Load())
		waitFor(primary, replica)
	}
	fmt.Println()
}

// ------------------------------------------------------------ part 3

func part3(primary, fast, slow *pgxpool.Pool) {
	fmt.Println("PART 3 — a counter goes up on the primary 100 times a second. One reader watches it for 3 seconds.")
	fmt.Println()
	fmt.Printf("  %-54s %8s %28s\n", "the reader's requests go to...", "reads", "times the counter went DOWN")

	for _, c := range []struct {
		name     string
		replicas []*pgxpool.Pool
	}{
		{"the two replicas in turn, as a load balancer would", []*pgxpool.Pool{fast, slow}},
		{"always the same replica (the slow one)", []*pgxpool.Pool{slow}},
	} {
		stop := make(chan struct{})
		var writer sync.WaitGroup
		writer.Add(1)
		go func() {
			defer writer.Done()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					_, err := primary.Exec(ctx, "UPDATE rp_counter SET n = n + 1")
					must(err)
				}
			}
		}()
		time.Sleep(500 * time.Millisecond)

		var reads, backwards int
		var previous int64
		begin := time.Now()
		for i := 0; time.Since(begin) < 3*time.Second; i++ {
			var n int64
			must(c.replicas[i%len(c.replicas)].QueryRow(ctx, "SELECT n FROM rp_counter").Scan(&n))
			reads++
			if n < previous {
				backwards++
			}
			previous = n
			time.Sleep(15 * time.Millisecond)
		}
		close(stop)
		writer.Wait()
		fmt.Printf("  %-54s %8d %28d\n", c.name, reads, backwards)
		waitFor(primary, fast, slow)
	}
}

func main() {
	primary := connect("PRIMARY_URL", "postgres://course@127.0.0.1:54350/postgres")
	fast := connect("FAST_REPLICA_URL", "postgres://course@127.0.0.1:54351/postgres")
	slow := connect("SLOW_REPLICA_URL", "postgres://course@127.0.0.1:54352/postgres")
	defer primary.Close()
	defer fast.Close()
	defer slow.Close()

	_, err := primary.Exec(ctx, `
		DROP TABLE IF EXISTS rp_profiles, rp_counter;
		CREATE TABLE rp_profiles (id int PRIMARY KEY, version int NOT NULL);
		INSERT INTO rp_profiles SELECT g, 1 FROM generate_series(1, 150) g;
		CREATE TABLE rp_counter (n bigint NOT NULL);
		INSERT INTO rp_counter VALUES (0);`)
	must(err)
	waitFor(primary, fast, slow)

	// A write on a replica is refused: it is read-only.
	_, err = fast.Exec(ctx, "UPDATE rp_counter SET n = n + 1")
	fmt.Printf("A write sent to a replica:  %v\n\n", err)

	part1(primary, fast, slow)
	part2(primary, slow)
	part3(primary, fast, slow)

	_, err = primary.Exec(ctx, "DROP TABLE rp_profiles, rp_counter")
	must(err)
}
