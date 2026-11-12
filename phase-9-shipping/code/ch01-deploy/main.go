// Chapter 9.1 — a rolling deploy, four ways, with the requests it drops counted.
//
// This program is a very small orchestrator. It starts three copies of a server as real
// processes, sends them steady traffic through a table of addresses, and then replaces each
// copy with a new version while the traffic continues. It does that four times, adding one
// piece of the chapter each time, and counts the requests that failed.
//
//	go run ./phase-9-shipping/code/ch01-deploy
//
// The servers are this same program, started again with -serve. Stopping one uses the TERM
// signal, so this runs on Linux and macOS, and not on Windows.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	warmUp      = 300 * time.Millisecond // how long a new copy takes before it can serve
	work        = 30 * time.Millisecond  // what one request costs
	propagation = 100 * time.Millisecond // how long the table takes to hear that a copy is going
	drain       = 250 * time.Millisecond // how long a polite copy goes on serving after TERM
)

// serve is one copy of the service. It prints its address, so that whoever started it
// knows where it is.
func serve(version string, graceful, drains bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(1)
	}
	fmt.Println(ln.Addr().String())
	started := time.Now()
	var stopping atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(started) < warmUp || stopping.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(started) < warmUp {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		time.Sleep(work)
		fmt.Fprint(w, version)
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	if graceful {
		// Without this, TERM ends the process at once, with requests half answered.
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		go func() {
			<-term
			stopping.Store(true) // the readiness probe fails from now on
			if drains {
				time.Sleep(drain) // go on serving while the news travels
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(ctx) // take no new connections, and finish the requests in flight
			close(done)
		}()
	}
	srv.Serve(ln)
	<-done
}

// A copy is one running process of the service.
type copy struct {
	cmd  *exec.Cmd
	addr string
}

func start(version string, d design) *copy {
	cmd := exec.Command(os.Args[0], "-serve", "-version="+version,
		fmt.Sprintf("-graceful=%v", d.graceful), fmt.Sprintf("-drains=%v", d.drains))
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		fmt.Println("cannot start a copy of the server")
		os.Exit(1)
	}
	addr, _ := bufio.NewReader(out).ReadString('\n')
	go cmd.Wait()
	return &copy{cmd, strings.TrimSpace(addr)}
}

// The table is what a client sees: the addresses to which requests may be sent.
type table struct {
	mu    sync.RWMutex
	addrs []string
}

func (t *table) add(addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.addrs = append(t.addrs, addr)
}

func (t *table) remove(addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, a := range t.addrs {
		if a == addr {
			t.addrs = append(t.addrs[:i], t.addrs[i+1:]...)
			return
		}
	}
}

func (t *table) pick() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.addrs[rand.Intn(len(t.addrs))]
}

func ready(addr string) bool {
	resp, err := http.Get("http://" + addr + "/ready")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// A design is what the deploy does, beyond starting new copies and stopping old ones.
type design struct {
	what      string
	readiness bool // a new copy gets traffic only when it says that it is ready
	graceful  bool // a copy that is told to stop finishes the requests it has
	drains    bool // …and goes on taking new ones until the table has caught up
}

type tally struct {
	sent, notReady, refused, cut, old, new atomic.Int64
}

func deploy(d design) *tally {
	t, tbl := &tally{}, &table{}
	var copies []*copy
	for i := 0; i < 3; i++ {
		c := start("v1", design{graceful: d.graceful, drains: d.drains})
		copies = append(copies, c)
	}
	for _, c := range copies {
		for !ready(c.addr) {
			time.Sleep(10 * time.Millisecond)
		}
		tbl.add(c.addr)
	}

	// Steady traffic, from sixteen clients, for as long as the deploy takes.
	stop := make(chan struct{})
	var clients sync.WaitGroup
	// A new connection for every request, so that the client library retries nothing behind
	// our back and every failure is counted.
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 16; i++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				t.sent.Add(1)
				resp, err := client.Get("http://" + tbl.pick() + "/")
				switch {
				case err != nil && errors.Is(err, syscall.ECONNREFUSED):
					t.refused.Add(1)
				case err != nil:
					t.cut.Add(1)
				default:
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					switch {
					case resp.StatusCode != 200:
						t.notReady.Add(1)
					case string(body) == "v1":
						t.old.Add(1)
					default:
						t.new.Add(1)
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)

	// The rolling deploy: one new copy in, one old copy out, three times.
	for _, old := range copies {
		fresh := start("v2", d)
		for d.readiness && !ready(fresh.addr) {
			time.Sleep(10 * time.Millisecond)
		}
		tbl.add(fresh.addr)
		old.cmd.Process.Signal(syscall.SIGTERM) // told to stop…
		go func() {
			time.Sleep(propagation) // …and, a little later, the table hears of it
			tbl.remove(old.addr)
		}()
		defer fresh.cmd.Process.Kill()
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	clients.Wait()
	return t
}

func main() {
	serving := flag.Bool("serve", false, "be one copy of the service")
	version := flag.String("version", "v1", "the version that a copy reports")
	graceful := flag.Bool("graceful", false, "finish requests in flight when told to stop")
	drains := flag.Bool("drains", false, "go on serving for a moment after being told to stop")
	flag.Parse()
	if *serving {
		serve(*version, *graceful, *drains)
		return
	}
	designs := []design{
		{"1  start the new copy, stop the old one", false, false, false},
		{"2  …and wait until the new copy says it is ready", true, false, false},
		{"3  …and a stopping copy finishes what it has", true, true, false},
		{"4  …and goes on serving until the table knows", true, true, true},
	}
	fmt.Println("Three copies of a service are replaced, one at a time, under steady traffic from 16 clients.")
	fmt.Println()
	fmt.Printf("  %-50s %9s %8s %10s %11s %9s\n", "what the deploy does", "requests", "served", "told 503:", "connection", "cut off")
	fmt.Printf("  %-50s %9s %8s %10s %11s %9s\n", "", "sent", "", "not ready", "refused", "half-way")
	// The four deploys do not share anything, so they are run side by side.
	results := make([]*tally, len(designs))
	var all sync.WaitGroup
	for i, d := range designs {
		all.Add(1)
		go func() {
			defer all.Done()
			results[i] = deploy(d)
		}()
	}
	all.Wait()
	for i, d := range designs {
		t := results[i]
		fmt.Printf("  %-50s %9d %8d %10d %11d %9d\n", d.what, t.sent.Load(), t.old.Load()+t.new.Load(), t.notReady.Load(), t.refused.Load(), t.cut.Load())
	}
}
