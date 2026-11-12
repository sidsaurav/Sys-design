// Chapter 5.1 — a failover, and what it loses.
//
// Runs against a real PostgreSQL primary with two replicas: the three
// servers of Chapter 3.6. Start them fresh, run this once, and stop them —
// the program promotes replicas, so the cluster is not the same afterwards.
//
//	bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start
//	go run ./phase-5-distributed-data/code/ch01-failover
//	bash phase-3-scaling-out/code/ch06-replicas/cluster.sh stop
//
//	part 1  the primary becomes unreachable and a replica is promoted: what was lost?
//	part 2  the old primary is still running: two primaries
//	part 3  the same failover with synchronous replication, and what that costs
//
// Nothing is killed. "The primary dies" here means that clients and replicas
// can no longer reach it — which is also what a failover tool sees.
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

func exec(pool *pgxpool.Pool, sql string, args ...any) {
	_, err := pool.Exec(ctx, sql, args...)
	must(err)
}

// writers inserts rows as fast as eight clients can, until told to stop, and
// returns the IDs of the writes that the server confirmed.
func writers(pool *pgxpool.Pool, source string, stop *atomic.Bool) (confirmed []int64) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for c := 0; c < 8; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				var id int64
				// A client waits one second for an answer, and then gives up.
				patience, cancel := context.WithTimeout(ctx, time.Second)
				err := pool.QueryRow(patience, "INSERT INTO fo_orders (source) VALUES ($1) RETURNING id", source).Scan(&id)
				cancel()
				if err == nil {
					mu.Lock()
					confirmed = append(confirmed, id)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return confirmed
}

// missing counts how many of the confirmed writes a server does not have.
func missing(pool *pgxpool.Pool, confirmed []int64) (n int) {
	have := map[int64]bool{}
	rows, err := pool.Query(ctx, "SELECT id FROM fo_orders")
	must(err)
	for rows.Next() {
		var id int64
		must(rows.Scan(&id))
		have[id] = true
	}
	for _, id := range confirmed {
		if !have[id] {
			n++
		}
	}
	return n
}

func promote(replica *pgxpool.Pool) {
	exec(replica, "SELECT pg_promote(true, 60)")
	for inRecovery := true; inRecovery; time.Sleep(20 * time.Millisecond) {
		must(replica.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery))
	}
}

func main() {
	primary := connect("PRIMARY_URL", "postgres://course@127.0.0.1:54350/postgres")
	a := connect("FAST_REPLICA_URL", "postgres://course@127.0.0.1:54351/postgres")
	b := connect("SLOW_REPLICA_URL", "postgres://course@127.0.0.1:54352/postgres")
	var inRecovery bool
	must(a.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery))
	if !inRecovery {
		log.Fatal("This cluster has been through a failover already. Stop it and start a fresh one:\n  bash phase-3-scaling-out/code/ch06-replicas/cluster.sh stop && bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start")
	}
	exec(primary, "DROP TABLE IF EXISTS fo_orders; CREATE TABLE fo_orders (id bigserial PRIMARY KEY, source text NOT NULL)")
	time.Sleep(time.Second)

	// ---------------------------------------------------------------- part 1
	fmt.Println("PART 1 — eight clients write orders to the primary. Replication is asynchronous, as it is by default.")
	fmt.Println("         Replica B loses its connection to the primary. One and a half seconds later the primary")
	fmt.Println("         becomes unreachable, and a replica must be promoted.")
	fmt.Println()
	var stop atomic.Bool
	done := make(chan []int64)
	go func() { done <- writers(primary, "the primary", &stop) }()
	time.Sleep(time.Second)
	// B is cut off: it stops receiving the primary's log.
	exec(b, "ALTER SYSTEM SET primary_conninfo = 'host=127.0.0.1 port=1'")
	exec(b, "SELECT pg_reload_conf()")
	time.Sleep(1500 * time.Millisecond)
	stop.Store(true) // the primary is gone, as far as anyone can tell
	confirmed := <-done
	time.Sleep(300 * time.Millisecond) // A receives whatever was still on its way

	fmt.Printf("  writes that the primary confirmed to its clients: %d\n\n", len(confirmed))
	fmt.Printf("  %-52s %28s\n", "if the new primary is...", "confirmed writes it lacks")
	fmt.Printf("  %-52s %28d\n", "replica A, which was connected to the end", missing(a, confirmed))
	lostOnB := missing(b, confirmed)
	fmt.Printf("  %-52s %28d\n", "replica B, cut off 1.5 seconds before", lostOnB)

	// ---------------------------------------------------------------- part 2
	fmt.Println()
	fmt.Println("PART 2 — the failover tool promotes replica B. But the old primary never stopped: it had only become")
	fmt.Println("         unreachable from where the tool was looking. For one second, some clients write to each.")
	fmt.Println()
	promote(b)
	stop.Store(false)
	go func() { done <- writers(primary, "the old primary", &stop) }()
	onNew := make(chan []int64)
	go func() { onNew <- writers(b, "the new primary", &stop) }()
	time.Sleep(time.Second)
	stop.Store(true)
	old, new := <-done, <-onNew
	// How many of the new primary's order numbers does the old primary also hold — for other orders?
	var both int
	must(primary.QueryRow(ctx, "SELECT count(*) FROM fo_orders WHERE id = ANY ($1)", new).Scan(&both))
	fmt.Printf("  orders accepted by the old primary:  %6d\n", len(old))
	fmt.Printf("  orders accepted by the new primary:  %6d\n", len(new))
	fmt.Printf("  of the new primary's order numbers, how many the old primary has given to other orders: %d\n", both)
	if both > 0 {
		var id int64
		var onOld, onNew string
		must(primary.QueryRow(ctx, "SELECT id, source FROM fo_orders WHERE id = ANY ($1) ORDER BY id LIMIT 1", new).Scan(&id, &onOld))
		must(b.QueryRow(ctx, "SELECT source FROM fo_orders WHERE id = $1", id).Scan(&onNew))
		fmt.Printf("  for example, order %d: written by %q on one server, and by %q on the other\n", id, onOld, onNew)
	}

	// ---------------------------------------------------------------- part 3
	fmt.Println()
	fmt.Println("PART 3 — the old primary and replica A, now with synchronous replication: a write is confirmed to")
	fmt.Println("         the client only once A has it too.")
	fmt.Println()
	latency := func() time.Duration {
		var times []time.Duration
		for i := 0; i < 200; i++ {
			begin := time.Now()
			exec(primary, "INSERT INTO fo_orders (source) VALUES ('timing')")
			times = append(times, time.Since(begin))
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		return times[len(times)/2]
	}
	async := latency()
	exec(primary, "ALTER SYSTEM SET synchronous_standby_names = 'fast'")
	exec(primary, "SELECT pg_reload_conf()")
	time.Sleep(500 * time.Millisecond)
	sync := latency()
	fmt.Printf("  one write takes: %.2f ms asynchronously, %.2f ms synchronously (median of 200)\n\n",
		float64(async.Microseconds())/1000, float64(sync.Microseconds())/1000)

	stop.Store(false)
	go func() { done <- writers(primary, "the primary", &stop) }()
	time.Sleep(1500 * time.Millisecond)
	// The primary becomes unreachable: A is promoted, at once, with clients still writing.
	promote(a)
	stop.Store(true)
	confirmed = <-done
	fmt.Printf("  the primary becomes unreachable while clients are writing, and A is promoted.\n")
	fmt.Printf("  writes confirmed to clients: %d       of those, missing on A: %d\n\n", len(confirmed), missing(a, confirmed))

	// The old primary is alone now. It may not confirm a write that no standby has.
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	begin := time.Now()
	_, err := primary.Exec(short, "INSERT INTO fo_orders (source) VALUES ('the old primary, alone')")
	cancel()
	fmt.Printf("  a client that can still reach the old primary tries to write. After %.1f s: %v\n", time.Since(begin).Seconds(), err)
	fmt.Println("  The old primary will not confirm a write that its standby has not received — and its standby has left.")
}
