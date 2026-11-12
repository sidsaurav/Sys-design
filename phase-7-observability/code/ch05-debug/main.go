// Chapter 7.5 — one symptom, three causes, and the profile that tells them apart.
//
// This program is two things. Run with -serve, it is a small service with a
// fault built in. Run without, it is the engineer: it starts the service as a
// separate process, puts it under load, and questions it from outside, over
// HTTP, as you would question a live service: its dashboard numbers, a CPU
// profile, and a dump of what every goroutine is doing.
//
//	part 1  the dashboard: four runs of the service, one healthy and three faulty
//	part 2  the same four runs, asked two questions: is it busy, and what is everyone doing?
//	part 3  the leak, watched for six seconds, with a restart in the middle
//
//	go run ./phase-7-observability/code/ch05-debug
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/… on the default mux
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ the service

// The service takes orders. Each order has 200 lines, and each line has a product code.
var codes = func() (list []string) {
	for i := 0; i < 200; i++ {
		list = append(list, fmt.Sprintf("SKU-%05d-%s", i, []string{"s", "m", "l", "xl"}[i%4]))
	}
	return list
}()

const pattern = `^(?i)[a-z]{2,4}-\d{3,6}(-(xs|s|m|l|xl|xxl))?$`

var valid = regexp.MustCompile(pattern) // compiled once, when the program starts

type shop struct {
	fault  string
	prices sync.Mutex // guards the table of prices
	calls  atomic.Int64
}

// database stands for a query to a database on another machine: time passes, and no CPU is used.
func database(d time.Duration) { time.Sleep(d) }

func (s *shop) order(w http.ResponseWriter, r *http.Request) {
	switch s.fault {
	case "cpu":
		// The fault: regexp.MatchString compiles its pattern every time it is called.
		for _, code := range codes {
			if ok, _ := regexp.MatchString(pattern, code); !ok {
				http.Error(w, "bad product code", http.StatusBadRequest)
				return
			}
		}
		database(2 * time.Millisecond)
	case "lock":
		for _, code := range codes {
			if !valid.MatchString(code) {
				http.Error(w, "bad product code", http.StatusBadRequest)
				return
			}
		}
		// The fault: the query is made while the lock is held, so one order at a time can make it.
		s.prices.Lock()
		database(5 * time.Millisecond)
		s.prices.Unlock()
	case "leak":
		result := make(chan []byte) // no buffer: a send waits until somebody receives
		slow := s.calls.Add(1)%20 == 0
		go func() {
			receipt := make([]byte, 512<<10)
			if slow {
				database(200 * time.Millisecond) // one query in twenty is slow
			} else {
				database(2 * time.Millisecond)
			}
			result <- receipt // the fault: if the handler has given up, nobody ever receives
		}()
		select {
		case <-result:
		case <-time.After(50 * time.Millisecond):
			http.Error(w, "the database is slow", http.StatusGatewayTimeout)
			return
		}
	default:
		for _, code := range codes {
			if !valid.MatchString(code) {
				http.Error(w, "bad product code", http.StatusBadRequest)
				return
			}
		}
		database(2 * time.Millisecond)
	}
	fmt.Fprintln(w, "ok")
}

type stats struct {
	Goroutines int     `json:"goroutines"`
	HeapMB     float64 `json:"heap_mb"`
}

func serve(fault string) {
	s := &shop{fault: fault}
	http.HandleFunc("/order", s.order)
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		json.NewEncoder(w).Encode(stats{runtime.NumGoroutine(), float64(m.HeapAlloc) / (1 << 20)})
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(l.Addr()) // tell the parent process where we are
	http.Serve(l, nil)
}

// ------------------------------------------------------------ the engineer

// A service is one running process of this program, started with -serve.
type service struct {
	cmd *exec.Cmd
	url string
}

func start(fault string) (*service, error) {
	cmd := exec.Command(os.Args[0], "-serve", "-fault", fault)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		return nil, err
	}
	return &service{cmd: cmd, url: "http://" + strings.TrimSpace(addr)}, nil
}

func (s *service) stop() {
	s.cmd.Process.Kill()
	s.cmd.Wait()
}

func (s *service) get(path string) []byte {
	resp, err := http.Get(s.url + path)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

func (s *service) stats() (st stats) {
	json.Unmarshal(s.get("/stats"), &st)
	return st
}

// load is forty clients. Each sends an order, waits for the answer, rests 50 ms, and sends another.
type load struct {
	target atomic.Pointer[service]
	mu     sync.Mutex
	took   []time.Duration
	failed int
	stop   chan struct{}
	done   sync.WaitGroup
}

func newLoad(s *service) *load {
	l := &load{stop: make(chan struct{})}
	l.target.Store(s)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 100}}
	for c := 0; c < 40; c++ {
		l.done.Add(1)
		go func() {
			defer l.done.Done()
			for {
				select {
				case <-l.stop:
					return
				default:
				}
				begin := time.Now()
				resp, err := client.Get(l.target.Load().url + "/order")
				ok := err == nil && resp.StatusCode == http.StatusOK
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				l.mu.Lock()
				l.took = append(l.took, time.Since(begin))
				if !ok {
					l.failed++
				}
				l.mu.Unlock()
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	return l
}

func (l *load) finish() (requests, failed int, median, p99 time.Duration) {
	close(l.stop)
	l.done.Wait()
	sort.Slice(l.took, func(i, j int) bool { return l.took[i] < l.took[j] })
	return len(l.took), l.failed, l.took[len(l.took)/2], l.took[len(l.took)*99/100]
}

// cpuProfile asks the service for a CPU profile of this many seconds, and has `go tool pprof`
// print where the time went. It returns the line that says how busy the process was, and the
// lines for this program's own functions and for what they called.
func (s *service) cpuProfile(seconds float64) (summary string, top []string) {
	raw := s.get(fmt.Sprintf("/debug/pprof/profile?seconds=%g", seconds))
	file, err := os.CreateTemp("", "cpu-*.pprof")
	if err != nil {
		return err.Error(), nil
	}
	defer os.Remove(file.Name())
	file.Write(raw)
	file.Close()
	out, err := exec.Command("go", "tool", "pprof", "-top", "-cum", "-nodecount=400", file.Name()).Output()
	if err != nil {
		return "go tool pprof could not be run: " + err.Error(), nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "Duration:"):
			summary = line
		case strings.Contains(line, " main.") || strings.Contains(line, " regexp.") || strings.Contains(line, " time.Sleep"):
			if len(top) < 5 {
				top = append(top, line)
			}
		}
	}
	return summary, top
}

// A group is a place in this program's code, and how many goroutines are at it.
type group struct {
	where string
	count int
}

const elsewhere = "in the HTTP server and the runtime, not in this program's code"

// goroutines asks the service what every goroutine is doing. The answer has one entry for
// each distinct stack. This groups the stacks by the innermost line of this program in them.
func (s *service) goroutines() (total int, groups []group) {
	count := map[string]int{}
	for _, block := range strings.Split(string(s.get("/debug/pprof/goroutine?debug=1")), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if strings.HasPrefix(lines[0], "goroutine profile: total ") {
			total, _ = strconv.Atoi(strings.TrimPrefix(lines[0], "goroutine profile: total "))
			lines = lines[1:]
		}
		if len(lines) == 0 || !strings.Contains(lines[0], " @ ") {
			continue
		}
		n, _ := strconv.Atoi(strings.Fields(lines[0])[0])
		// Each line after the first is one frame, innermost first: # address function+offset file:line
		where, inner, found := elsewhere, "", false
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			if len(f) < 4 || f[0] != "#" {
				continue
			}
			name := f[2][:strings.LastIndex(f[2], "+")]
			at := name + " " + f[3][strings.LastIndex(f[3], "/")+1:]
			switch ours := strings.HasPrefix(name, "main."); {
			case ours && !found: // the innermost line of this program
				where, found = at, true
				if inner != "" {
					where += ", in " + inner
				}
				continue
			case ours: // and the line of this program that called it
				where += ", called from " + at
			case !found:
				inner = name // the function that this program called, once we reach the program
				continue
			}
			break
		}
		count[where] += n
	}
	for where, n := range count {
		groups = append(groups, group{where, n})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].count != groups[j].count {
			return groups[i].count > groups[j].count
		}
		return groups[i].where < groups[j].where
	})
	return total, groups
}

// busy reads the percentage out of pprof's summary line: "Total samples = 5.88s (274.30%)".
func busy(summary string) float64 {
	open, shut := strings.LastIndex(summary, "("), strings.LastIndex(summary, "%")
	if open < 0 || shut < open {
		return 0
	}
	percent, _ := strconv.ParseFloat(strings.TrimSpace(summary[open+1:shut]), 64)
	return percent
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }

// ------------------------------------------------------------ parts 1 and 2

type finding struct {
	fault            string
	requests, failed int
	median, p99      time.Duration
	st               stats
	summary          string
	top              []string
	total            int
	groups           []group
}

func examine(fault string) finding {
	s, err := start(fault)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not start the service:", err)
		os.Exit(1)
	}
	defer s.stop()
	f := finding{fault: fault}
	l := newLoad(s)
	time.Sleep(500 * time.Millisecond)
	f.summary, f.top = s.cpuProfile(2) // two seconds, while the load is running
	f.total, f.groups = s.goroutines()
	f.st = s.stats()
	f.requests, f.failed, f.median, f.p99 = l.finish()
	return f
}

func parts1and2() {
	var findings []finding
	for _, fault := range []string{"none", "cpu", "lock", "leak"} {
		findings = append(findings, examine(fault))
	}
	fmt.Println("PART 1 — the dashboard. The same service four times, with forty clients, for about three seconds each.")
	fmt.Println()
	fmt.Printf("  %-8s %10s %8s %10s %17s %12s %14s\n", "fault", "requests", "failed", "median", "99th percentile", "goroutines", "memory in use")
	for _, f := range findings {
		fmt.Printf("  %-8s %10d %7.1f%% %10s %17s %12d %11.0f MB\n", f.fault, f.requests, 100*float64(f.failed)/float64(f.requests), ms(f.median), ms(f.p99), f.st.Goroutines, f.st.HeapMB)
	}
	fmt.Println()
	fmt.Println("PART 2 — two questions, put to the running process over HTTP.")
	for _, f := range findings {
		fmt.Println()
		fmt.Printf("  fault: %s\n", f.fault)
		fmt.Printf("    Is it busy? A CPU profile of two seconds, on %d CPUs:\n", runtime.NumCPU())
		fmt.Printf("      %s\n", f.summary)
		if busy(f.summary) < 50 {
			fmt.Println("      Less than half of one CPU. The process is not working, so this profile has nothing to say.")
		} else {
			fmt.Printf("      %10s %6s %6s %10s %6s\n", "flat", "flat%", "sum%", "cum", "cum%")
			for _, line := range f.top {
				fmt.Printf("      %s\n", line)
			}
		}
		fmt.Printf("    What is everyone doing? A dump of all %d goroutines, grouped by where they are:\n", f.total)
		for _, g := range f.groups[:min(4, len(f.groups))] {
			fmt.Printf("      %5d  %s\n", g.count, g.where)
		}
	}
}

// ------------------------------------------------------------ part 3

func part3() {
	fmt.Println("PART 3 — the leak, watched for six seconds. After three, somebody restarts the service.")
	fmt.Println()
	s, err := start("leak")
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not start the service:", err)
		os.Exit(1)
	}
	l := newLoad(s)
	fmt.Printf("  %-8s %12s %14s\n", "second", "goroutines", "memory in use")
	for second := 1; second <= 6; second++ {
		time.Sleep(time.Second)
		st := s.stats()
		fmt.Printf("  %-8d %12d %11.0f MB\n", second, st.Goroutines, st.HeapMB)
		if second == 3 {
			_, groups := s.goroutines()
			fresh, err := start("leak")
			if err != nil {
				break
			}
			l.target.Store(fresh)
			s.stop()
			s = fresh
			fmt.Printf("  %-8s the service is restarted. The largest group in its last dump: %d, %s\n", "", groups[0].count, groups[0].where)
		}
	}
	requests, failed, _, _ := l.finish()
	s.stop()
	fmt.Println()
	fmt.Printf("  %d requests, of which %d failed.\n", requests, failed)
}

func main() {
	serving := flag.Bool("serve", false, "be the service")
	fault := flag.String("fault", "none", "the fault that the service has: none, cpu, lock or leak")
	flag.Parse()
	if *serving {
		serve(*fault)
		return
	}
	parts1and2()
	fmt.Println()
	part3()
}
