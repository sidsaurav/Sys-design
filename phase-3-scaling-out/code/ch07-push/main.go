// Chapter 3.7 — five ways to tell a client that something has happened.
//
// A server produces an event now and then. Two hundred clients each want
// to hear of it as soon as possible. Everything runs inside this one
// process, on local ports.
//
//	part 1  the same events delivered five ways: polling every second, polling every
//	        five seconds, long polling, server-sent events, and WebSockets
//	part 2  two instances: the event arrives at one; half the clients are connected to the other
//	part 3  a deploy: the instance restarts, and its clients come back
//	part 4  a graceful shutdown, as in Chapter 3.2, with a stream open
//
// Run:
//
//	go run ./phase-3-scaling-out/code/ch07-push                         # all four parts, about 50 seconds
//	go run ./phase-3-scaling-out/code/ch07-push -part 3
//	go run ./phase-3-scaling-out/code/ch07-push -part 1 -events 1       # the same ten seconds, with one event
//	go run ./phase-3-scaling-out/code/ch07-push -serve localhost:8080   # a server to try with curl
//
// The WebSocket part uses github.com/gorilla/websocket; `go run` fetches it
// the first time.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const clients = 200

var beat = flag.Duration("heartbeat", 15*time.Second, "how often each stream is sent a comment line, to keep it open")

// ------------------------------------------------------------ events

type event struct {
	id int
	at time.Time
}

func (e event) String() string { return fmt.Sprintf("%d %d", e.id, e.at.UnixNano()) }

func parse(line string) (event, bool) {
	var id int
	var nanos int64
	if _, err := fmt.Sscanf(line, "%d %d", &id, &nanos); err != nil {
		return event{}, false
	}
	return event{id, time.Unix(0, nanos)}, true
}

// A feed is the list of events so far, and a way to wait for the next.
type feed struct {
	mu      sync.Mutex
	events  []event
	changed chan struct{} // closed, and replaced, whenever an event is added
}

func newFeed() *feed { return &feed{changed: make(chan struct{})} }

func (f *feed) add(e event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	close(f.changed) // wakes everyone who is waiting
	f.changed = make(chan struct{})
}

// after returns the events newer than the given one, and a channel that
// will be closed when there is something newer still.
func (f *feed) after(id int) ([]event, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := sort.Search(len(f.events), func(i int) bool { return f.events[i].id > id })
	return append([]event(nil), f.events[i:]...), f.changed
}

func (f *feed) latest() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return 0
	}
	return f.events[len(f.events)-1].id
}

// ------------------------------------------------------------ one instance of the server

type instance struct {
	feed     *feed
	addr     string
	srv      *http.Server
	requests atomic.Int64 // HTTP requests received
	empty    atomic.Int64 // answered with "nothing new"

	mu       sync.Mutex
	accepted []time.Time // when each stream was opened (for part 3)
}

func start(f *feed, addr string) *instance {
	in := &instance{feed: f}
	in.listen(addr)
	return in
}

func (in *instance) listen(addr string) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	in.addr = ln.Addr().String()
	in.srv = &http.Server{Handler: in.handler()}
	go in.srv.Serve(ln)
}

func after(r *http.Request) int {
	id, _ := strconv.Atoi(r.URL.Query().Get("after"))
	return id
}

func (in *instance) handler() http.Handler {
	mux := http.NewServeMux()

	// Polling: "anything after event N?" Answered at once, with or without news.
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		in.requests.Add(1)
		news, _ := in.feed.after(after(r))
		if len(news) == 0 {
			in.empty.Add(1)
		}
		for _, e := range news {
			fmt.Fprintln(w, e)
		}
	})

	// Long polling: the same question, but the answer waits until there is news.
	mux.HandleFunc("/wait", func(w http.ResponseWriter, r *http.Request) {
		in.requests.Add(1)
		timeout := time.After(25 * time.Second)
		for {
			news, changed := in.feed.after(after(r))
			if len(news) > 0 {
				for _, e := range news {
					fmt.Fprintln(w, e)
				}
				return
			}
			select {
			case <-changed:
			case <-timeout:
				in.empty.Add(1)
				return // nothing happened: the client asks again
			case <-r.Context().Done():
				return
			}
		}
	})

	// Server-sent events: one response that never ends. Each event is
	// written to it as it happens.
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		in.requests.Add(1)
		in.mu.Lock()
		in.accepted = append(in.accepted, time.Now())
		in.mu.Unlock()
		last := in.feed.latest() // a new client starts from now...
		if id, err := strconv.Atoi(r.Header.Get("Last-Event-ID")); err == nil {
			last = id // ...a returning one, from where it left off
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		flusher := w.(http.Flusher)
		flusher.Flush()
		heartbeat := time.NewTicker(*beat)
		defer heartbeat.Stop()
		for {
			news, changed := in.feed.after(last)
			for _, e := range news {
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.id, e)
				last = e.id
			}
			flusher.Flush()
			select {
			case <-changed:
			case <-heartbeat.C:
				// A comment line. The client ignores it; a load balancer in
				// between sees traffic and leaves the connection open.
				fmt.Fprint(w, ": still here\n\n")
			case <-r.Context().Done():
				return
			}
		}
	})

	// WebSocket: the connection stops being HTTP and becomes a two-way
	// channel for messages. Here the server only sends.
	upgrader := websocket.Upgrader{}
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		in.requests.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		gone := make(chan struct{})
		go func() { // notice when the client goes away
			defer close(gone)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		last := in.feed.latest()
		for {
			news, changed := in.feed.after(last)
			for _, e := range news {
				if conn.WriteMessage(websocket.TextMessage, []byte(e.String())) != nil {
					return
				}
				last = e.id
			}
			select {
			case <-changed:
			case <-gone:
				return
			}
		}
	})
	return mux
}

// ------------------------------------------------------------ clients

// A tally records what clients received.
type tally struct {
	mu        sync.Mutex
	seen      map[[2]int]bool // (client, event)
	latencies []time.Duration
}

func newTally() *tally { return &tally{seen: map[[2]int]bool{}} }

func (t *tally) got(client int, e event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if key := [2]int{client, e.id}; !t.seen[key] {
		t.seen[key] = true
		t.latencies = append(t.latencies, time.Since(e.at))
	}
}

func (t *tally) summary() (delivered int, mean, p99 time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.latencies) == 0 {
		return 0, 0, 0
	}
	sort.Slice(t.latencies, func(i, j int) bool { return t.latencies[i] < t.latencies[j] })
	var sum time.Duration
	for _, d := range t.latencies {
		sum += d
	}
	return len(t.latencies), sum / time.Duration(len(t.latencies)), t.latencies[len(t.latencies)*99/100]
}

var httpClient = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1000}}

// ask sends one request and returns the events in the answer.
func ask(ctx context.Context, url string) (events []event, err error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		if e, ok := parse(scan.Text()); ok {
			events = append(events, e)
		}
	}
	return events, scan.Err()
}

// poll asks every `every`, whether or not anything has happened.
func poll(ctx context.Context, id int, in *instance, every time.Duration, t *tally) {
	last := 0
	time.Sleep(time.Duration(rand.Int63n(int64(every)))) // clients are not in step with each other
	for ctx.Err() == nil {
		news, _ := ask(ctx, fmt.Sprintf("http://%s/events?after=%d", in.addr, last))
		for _, e := range news {
			t.got(id, e)
			last = e.id
		}
		select {
		case <-time.After(every):
		case <-ctx.Done():
		}
	}
}

// longPoll asks, waits for the answer however long it takes, and asks again.
func longPoll(ctx context.Context, id int, in *instance, t *tally) {
	last := 0
	for ctx.Err() == nil {
		news, _ := ask(ctx, fmt.Sprintf("http://%s/wait?after=%d", in.addr, last))
		for _, e := range news {
			t.got(id, e)
			last = e.id
		}
	}
}

// stream opens one server-sent-events connection and reads from it until
// it breaks. It returns the ID of the last event it received.
func stream(ctx context.Context, id int, addr string, resumeFrom int, t *tally) (last int, connected bool) {
	last = resumeFrom
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/stream", nil)
	if resumeFrom >= 0 {
		req.Header.Set("Last-Event-ID", strconv.Itoa(resumeFrom))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return last, false
	}
	defer resp.Body.Close()
	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		if data, ok := strings.CutPrefix(scan.Text(), "data: "); ok {
			if e, ok := parse(data); ok {
				t.got(id, e)
				last = e.id
			}
		}
	}
	return last, true
}

func ws(ctx context.Context, id int, in *instance, t *tally) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws://"+in.addr+"/ws", nil)
	if err != nil {
		return
	}
	go func() { <-ctx.Done(); conn.Close() }()
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if e, ok := parse(string(message)); ok {
			t.got(id, e)
		}
	}
}

func ms(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	if d >= 10*time.Millisecond {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

// ------------------------------------------------------------ part 1

func part1(events int) {
	what := "events at random moments"
	if events == 1 {
		what = "event at a random moment"
	}
	fmt.Printf("PART 1 — %d %s in 10 seconds, to be delivered to %d clients.\n\n", events, what, clients)

	type transport struct {
		name string
		run  func(ctx context.Context, id int, in *instance, t *tally)
	}
	transports := []transport{
		{"polling, every second", func(ctx context.Context, id int, in *instance, t *tally) { poll(ctx, id, in, time.Second, t) }},
		{"polling, every 5 seconds", func(ctx context.Context, id int, in *instance, t *tally) { poll(ctx, id, in, 5*time.Second, t) }},
		{"long polling", longPoll},
		{"server-sent events", func(ctx context.Context, id int, in *instance, t *tally) { stream(ctx, id, in.addr, -1, t) }},
		{"WebSocket", ws},
	}

	// The five run side by side, each with its own server and its own clients.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	servers := make([]*instance, len(transports))
	tallies := make([]*tally, len(transports))
	for i, tr := range transports {
		servers[i], tallies[i] = start(newFeed(), ""), newTally()
		for c := 0; c < clients; c++ {
			wg.Add(1)
			go func() { defer wg.Done(); tr.run(ctx, c, servers[i], tallies[i]) }()
		}
	}
	time.Sleep(time.Second) // everyone is connected

	rng := rand.New(rand.NewSource(7))
	moments := make([]time.Duration, events)
	for i := range moments {
		moments[i] = time.Duration(rng.Int63n(int64(10 * time.Second)))
	}
	sort.Slice(moments, func(i, j int) bool { return moments[i] < moments[j] })
	begin := time.Now()
	for i, m := range moments {
		time.Sleep(time.Until(begin.Add(m)))
		e := event{i + 1, time.Now()}
		for _, s := range servers {
			s.feed.add(e)
		}
	}
	time.Sleep(time.Until(begin.Add(15500 * time.Millisecond))) // long enough for the slowest poller
	cancel()
	wg.Wait()

	fmt.Printf("  %-26s %10s %16s %14s %12s %12s\n", "", "delivered", "requests", "\"nothing new\"", "mean delay", "worst 1%")
	for i, tr := range transports {
		delivered, mean, p99 := tallies[i].summary()
		fmt.Printf("  %-26s %5d/%d %16d %14d %12s %12s\n", tr.name, delivered, clients*events,
			servers[i].requests.Load(), servers[i].empty.Load(), ms(mean), ms(p99))
	}
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Printf("PART 2 — two instances. %d clients hold streams: half to each. Five events are published, each by a\n", clients)
	fmt.Println("         request that happens to reach instance 1.")
	fmt.Println()
	fmt.Printf("  %-58s %s\n", "how an instance learns of an event", "clients that received each event")

	for _, shared := range []bool{false, true} {
		one, two := start(newFeed(), ""), start(newFeed(), "")
		// With a broker — Redis pub/sub, for instance — every instance
		// subscribes, and hears of every event wherever it was published.
		publish := func(e event) { one.feed.add(e) }
		if shared {
			publish = func(e event) { one.feed.add(e); two.feed.add(e) }
		}

		ctx, cancel := context.WithCancel(context.Background())
		t := newTally()
		var wg sync.WaitGroup
		for c := 0; c < clients; c++ {
			in := []*instance{one, two}[c%2] // the load balancer spreads the connections
			wg.Add(1)
			go func() { defer wg.Done(); stream(ctx, c, in.addr, -1, t) }()
		}
		time.Sleep(500 * time.Millisecond)
		for id := 1; id <= 5; id++ {
			publish(event{id, time.Now()})
			time.Sleep(200 * time.Millisecond)
		}
		cancel()
		wg.Wait()
		delivered, _, _ := t.summary()
		name := "only from requests it receives itself"
		if shared {
			name = "from a broker that every instance subscribes to"
		}
		fmt.Printf("  %-58s %d of %d\n", name, delivered/5, clients)
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Printf("PART 3 — %d clients hold streams to one instance. An event is published every 100 ms.\n", clients)
	fmt.Println("         Two seconds in, the instance is restarted: it is away for one second.")
	fmt.Println()
	fmt.Printf("  %-48s %14s %20s %17s %17s\n", "when its stream breaks, a client...", "events missed", "attempts while away", "busiest 100 ms", "every client")
	fmt.Printf("  %-48s %14s %20s %17s %17s\n", "", "(per client)", "(all refused)", "after it is back", "back within")

	for _, v := range []struct {
		name   string
		resume bool
		jitter bool
	}{
		{"reconnects at once, and carries on from \"now\"", false, false},
		{"reconnects at once, and says where it left off", true, false},
		{"waits a random moment, backing off; says where", true, true},
	} {
		f := newFeed()
		in := start(f, "")
		ctx, cancel := context.WithCancel(context.Background())
		t := newTally()
		var refused atomic.Int64
		var wg sync.WaitGroup
		for c := 0; c < clients; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(c)))
				last, wait := -1, 100*time.Millisecond
				for ctx.Err() == nil {
					from := -1
					if v.resume {
						from = last
					}
					got, connected := stream(ctx, c, in.addr, from, t)
					last = got
					if ctx.Err() != nil {
						return
					}
					if connected {
						wait = 100 * time.Millisecond // it worked for a while: start again from a short wait
					} else {
						refused.Add(1)
					}
					pause := 20 * time.Millisecond // try again almost at once
					if v.jitter {
						pause = time.Duration(rng.Int63n(int64(wait))) // somewhere between zero and the current wait
						wait = min(2*wait, 2*time.Second)              // and wait longer next time
					}
					time.Sleep(pause)
				}
			}()
		}
		time.Sleep(500 * time.Millisecond)

		// Events keep coming whatever the instance is doing: they come from
		// elsewhere (a database, a broker) and are there when it returns.
		published := 0
		stop := make(chan struct{})
		var publisher sync.WaitGroup
		publisher.Add(1)
		go func() {
			defer publisher.Done()
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					published++
					f.add(event{published, time.Now()})
				}
			}
		}()

		time.Sleep(2 * time.Second)
		in.srv.Close() // the deploy: every stream breaks
		time.Sleep(time.Second)
		in.mu.Lock()
		in.accepted = nil
		in.mu.Unlock()
		back := time.Now()
		in.listen(in.addr) // the new version is up
		time.Sleep(4 * time.Second)
		close(stop)
		publisher.Wait()
		time.Sleep(300 * time.Millisecond)
		cancel()
		wg.Wait()

		// The busiest tenth of a second for new connections after the
		// restart, and how long it was until the last client was back.
		in.mu.Lock()
		buckets := map[int]int{}
		busiest := 0
		var allBack time.Duration
		for _, at := range in.accepted {
			b := int(at.Sub(back) / (100 * time.Millisecond))
			buckets[b]++
			busiest = max(busiest, buckets[b])
			allBack = max(allBack, at.Sub(back))
		}
		in.mu.Unlock()
		delivered, _, _ := t.summary()
		missed := float64(clients*published-delivered) / clients
		fmt.Printf("  %-48s %14.1f %20d %11d conns %17s\n", v.name, missed, refused.Load(), busiest, ms(allBack))
	}
}

// ------------------------------------------------------------ part 4

func part4() {
	fmt.Println("PART 4 — one client holds a stream; an event is published every 100 ms. The server is asked to shut")
	fmt.Println("         down gracefully, as in Chapter 3.2, and is given two seconds to do it.")
	fmt.Println()
	fmt.Printf("  %-20s %25s   %-28s %s\n", "the stream is", "Shutdown returned after", "saying", "and one second later, the stream is")

	for _, v := range []struct {
		name string
		run  func(ctx context.Context, in *instance, t *tally)
	}{
		{"server-sent events", func(ctx context.Context, in *instance, t *tally) { stream(ctx, 0, in.addr, -1, t) }},
		{"a WebSocket", func(ctx context.Context, in *instance, t *tally) { ws(ctx, 0, in, t) }},
	} {
		f := newFeed()
		in := start(f, "")
		ctx, cancel := context.WithCancel(context.Background())
		t := newTally()
		done := make(chan struct{})
		go func() { defer close(done); v.run(ctx, in, t) }()
		stop := make(chan struct{})
		go func() {
			for id := 1; ; id++ {
				select {
				case <-stop:
					return
				case <-time.After(100 * time.Millisecond):
					f.add(event{id, time.Now()})
				}
			}
		}()
		time.Sleep(500 * time.Millisecond)

		deadline, cancelDeadline := context.WithTimeout(context.Background(), 2*time.Second)
		begin := time.Now()
		err := in.srv.Shutdown(deadline)
		took := time.Since(begin)
		cancelDeadline()

		before, _, _ := t.summary()
		time.Sleep(time.Second)
		after, _, _ := t.summary()

		saying := "(no error)"
		if err != nil {
			saying = err.Error()
		}
		state := "closed"
		if after > before {
			state = fmt.Sprintf("still open: %d more events arrived", after-before)
		}
		fmt.Printf("  %-20s %25s   %-28s %s\n", v.name, ms(took), saying, state)

		close(stop)
		cancel()
		in.srv.Close()
		<-done
	}
}

func main() {
	part := flag.Int("part", 0, "run only this part (1 to 4)")
	events := flag.Int("events", 8, "part 1: how many events happen in the ten seconds")
	serve := flag.String("serve", "", "instead of the experiments, serve at this address (say, localhost:8080) with an event every 2 seconds")
	flag.Parse()
	log.SetOutput(io.Discard) // the HTTP server logs every broken stream; that is expected here

	if *serve != "" { // something to point curl at
		f := newFeed()
		in := start(f, *serve)
		fmt.Printf("An event every 2 seconds. In another terminal, try:\n\n")
		fmt.Printf("  curl -N http://%s/stream\n", in.addr)
		fmt.Printf("  curl -N -H 'Last-Event-ID: 1' http://%s/stream\n", in.addr)
		fmt.Printf("  curl 'http://%s/wait?after=0'\n", in.addr)
		for id := 1; ; id++ {
			time.Sleep(2 * time.Second)
			f.add(event{id, time.Now()})
		}
	}
	for i, run := range []func(){func() { part1(*events) }, part2, part3, part4} {
		if *part == 0 || *part == i+1 {
			run()
			fmt.Println()
		}
	}
}
