// Capstone 5 — the send path of the worked design, on PostgreSQL.
//
// The model in main.go checks the design as a whole. This file checks the one
// step of it that a model cannot vouch for: that a real database, with many
// senders at once, gives each message of a conversation one position — none
// missing, none given twice, and none appearing behind a reader's back — and
// what that costs.
//
//	part 1  16 senders, 8,000 messages, 20 conversations. One send in ten is
//	        repeated. A reader follows each conversation, asking again and
//	        again for "everything after the last number I saw".
//	part 2  the price: the same senders across 200 conversations, and then
//	        all in one
//
//	go run ./phase-5-distributed-data/code/capstone-chat -sql
//	go run ./phase-5-distributed-data/code/capstone-chat -sql -without counter
//
// Needs the database from Phase 2 (DATABASE_URL, as before).
package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
DROP TABLE IF EXISTS chat_messages, chat_conversations;
DROP SEQUENCE IF EXISTS chat_seq;
CREATE TABLE chat_conversations (
    id       bigint PRIMARY KEY,
    last_seq bigint NOT NULL DEFAULT 0     -- the number of its newest message. This row is also the lock
);
CREATE TABLE chat_messages (
    conversation_id bigint NOT NULL,
    seq             bigint NOT NULL,        -- its position in the conversation: 1, 2, 3, …
    sender_id       bigint NOT NULL,
    client_id       uuid   NOT NULL,        -- chosen by the sender's device; the same on every attempt
    body            text   NOT NULL,
    sent_at         timestamptz NOT NULL DEFAULT now(),   -- for display. Never for ordering
    PRIMARY KEY (conversation_id, seq),
    UNIQUE (conversation_id, client_id)
);
CREATE SEQUENCE chat_seq;                   -- used only by -without counter
INSERT INTO chat_conversations (id) SELECT g FROM generate_series(1, 200) g;`

// One statement. The UPDATE locks the conversation's row, so that one message
// at a time is being numbered in a conversation, and the lock is held until
// that message is committed. If the message is already stored, the unique
// constraint on client_id rejects the INSERT, and the UPDATE is undone with
// it: no number is used up.
const sendSQL = `
WITH next AS (
    UPDATE chat_conversations SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq
)
INSERT INTO chat_messages (conversation_id, seq, sender_id, client_id, body)
SELECT $1, last_seq, $2, $3, $4 FROM next
RETURNING seq`

// -without counter: the number comes from a sequence shared by all
// conversations, as the value of an auto-incrementing column would.
const sendBySequenceSQL = `
INSERT INTO chat_messages (conversation_id, seq, sender_id, client_id, body)
VALUES ($1, nextval('chat_seq'), $2, $3, $4)
RETURNING seq`

// send stores a message and returns its position. Sending the same message
// again — the same client_id — returns the same position and stores nothing.
func send(conv, sender int64, clientID, body string) (seq int64, err error) {
	statement := sendSQL
	if without["counter"] {
		statement = sendBySequenceSQL
	}
	err = pool.QueryRow(ctx, statement, conv, sender, clientID, body).Scan(&seq)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation: it is already stored
		err = pool.QueryRow(ctx, `SELECT seq FROM chat_messages WHERE conversation_id = $1 AND client_id = $2`,
			conv, clientID).Scan(&seq)
	}
	return seq, err
}

// newID makes the kind of ID a device would: a random UUID.
func newID() string {
	var b [16]byte
	_, err := crand.Read(b[:])
	must(err)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func count(query string, args ...any) (n int64) {
	must(pool.QueryRow(ctx, query, args...).Scan(&n))
	return n
}

func part1() {
	const senders, messages, convs = 16, 8000, 20
	fmt.Println("PART 1 — 16 senders, 8,000 messages, 20 conversations; one send in ten is repeated.")
	fmt.Println("         A reader follows each conversation: \"everything after the last number I saw\".")
	fmt.Println()

	// The readers.
	var (
		done    atomic.Bool
		readers sync.WaitGroup
		seen    [convs + 1]int64
	)
	for c := 1; c <= convs; c++ {
		readers.Add(1)
		go func(c int) {
			defer readers.Done()
			var cursor int64
			for last := false; ; {
				rows, err := pool.Query(ctx, `SELECT seq FROM chat_messages WHERE conversation_id = $1 AND seq > $2 ORDER BY seq`, c, cursor)
				must(err)
				for rows.Next() {
					must(rows.Scan(&cursor))
					seen[c]++
				}
				must(rows.Err())
				if last {
					return
				}
				last = done.Load() // after the senders have finished, look once more
			}
		}(c)
	}

	// The senders.
	var (
		wg                  sync.WaitGroup
		next                atomic.Int64
		repeated, different atomic.Int64
	)
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(s)))
			for next.Add(1) <= messages {
				conv, id := int64(1+r.Intn(convs)), newID()
				var again int64
				switch r.Intn(20) {
				case 0: // the device gives up waiting and sends again, while the first attempt is still on its way
					second := make(chan int64)
					go func() {
						seq, err := send(conv, int64(s), id, "hello")
						must(err)
						second <- seq
					}()
					seq, err := send(conv, int64(s), id, "hello")
					must(err)
					again = <-second
					if again != seq {
						different.Add(1)
					}
					repeated.Add(1)
				case 1: // the confirmation is lost, and the device sends again afterwards
					seq, err := send(conv, int64(s), id, "hello")
					must(err)
					again, err = send(conv, int64(s), id, "hello")
					must(err)
					if again != seq {
						different.Add(1)
					}
					repeated.Add(1)
				default:
					_, err := send(conv, int64(s), id, "hello")
					must(err)
				}
			}
		}(s)
	}
	wg.Wait()
	done.Store(true)
	readers.Wait()

	stored := count(`SELECT count(*) FROM chat_messages WHERE conversation_id <= $1`, convs)
	var passed int64
	for c := 1; c <= convs; c++ {
		passed += count(`SELECT count(*) FROM chat_messages WHERE conversation_id = $1`, c) - seen[c]
	}
	fmt.Printf("  messages sent %d, of which %d were sent twice; the database holds %d\n\n", messages, repeated.Load(), stored)
	fmt.Println("CHECKS")
	fmt.Printf("  %-66s %6d\n", "messages stored twice, because the send was repeated", stored-messages)
	fmt.Printf("  %-66s %6d\n", "repeated sends that were answered with a different number", different.Load())
	if without["counter"] {
		fmt.Printf("  %-66s %6s\n", "numbers missing from a conversation's sequence", "n/a")
	} else {
		fmt.Printf("  %-66s %6d\n", "numbers missing from a conversation's sequence",
			count(`SELECT coalesce(sum(newest - n), 0)::bigint FROM (SELECT max(seq) AS newest, count(*) AS n FROM chat_messages GROUP BY conversation_id) c`))
	}
	fmt.Printf("  %-66s %6d\n", "messages that a reader's cursor went past without seeing", passed)
}

// rate sends 6,000 messages from 16 senders into the first n conversations.
func rate(n int) (perSecond float64, median, p99 time.Duration) {
	const senders, messages = 16, 6000
	var (
		wg   sync.WaitGroup
		next atomic.Int64
		mu   sync.Mutex
		took []time.Duration
	)
	start := time.Now()
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(100 + s)))
			var mine []time.Duration
			for next.Add(1) <= messages {
				t := time.Now()
				_, err := send(int64(1+r.Intn(n)), int64(s), newID(), "hello")
				must(err)
				mine = append(mine, time.Since(t))
			}
			mu.Lock()
			took = append(took, mine...)
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	elapsed := time.Since(start)
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	return messages / elapsed.Seconds(), took[len(took)/2], took[len(took)*99/100]
}

func sendPath() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	var err error
	pool, err = pgxpool.New(ctx, url+"?pool_max_conns=60")
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Fatalf("cannot connect to %s (%v).\nStart the database of Phase 2 first: see phase-2-data/README.md", url, err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, schema)
	must(err)

	part1()

	fmt.Println()
	fmt.Println("PART 2 — the price of one order per conversation: 16 senders, 6,000 messages.")
	fmt.Println()
	fmt.Printf("  %-34s %18s %16s %18s\n", "", "messages a second", "a send, median", "99th percentile")
	for _, n := range []int{200, 1} {
		name := fmt.Sprintf("spread over %d conversations", n)
		if n == 1 {
			name = "all in one conversation"
		}
		perSecond, median, p99 := rate(n)
		fmt.Printf("  %-34s %18.0f %13.2f ms %15.2f ms\n", name, perSecond, ms(median), ms(p99))
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
