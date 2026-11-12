// Chapter 7.1 — the same ten seconds of a service, seen through totals, through labelled metrics, and through logs.
//
// A shop's API, as a real HTTP server inside this process: /search, /cart and
// /checkout, used by 2,000 customers. For the whole run, checkout fails for
// the customers whose data is on database shard 7: one customer in twenty.
// Nothing else is wrong.
//
//	part 1  what the totals show, what the metrics show by endpoint, and what the logs show
//	part 2  cardinality: what a label costs
//	part 3  sampling the logs: what is saved, and what is lost
//
//	go run ./phase-7-observability/code/ch01-telemetry
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------ metrics: a counter and a histogram for each set of labels

var buckets = []float64{5, 10, 25, 50, 100, 250, 500, 1000} // upper bounds, in ms. The last bucket has none

type series struct {
	count  int64
	sum    float64
	bucket [9]int64
}

// A registry holds one series for every combination of label values that has been seen.
type registry struct {
	mu     sync.Mutex
	series map[string]*series
}

func newRegistry() *registry { return &registry{series: map[string]*series{}} }

func (r *registry) observe(labels string, ms float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.series[labels]
	if s == nil {
		s = &series{}
		r.series[labels] = s
	}
	s.count++
	s.sum += ms
	i := sort.SearchFloat64s(buckets, ms)
	s.bucket[i]++
}

// total adds up the series whose labels contain every one of the given parts.
func (r *registry) total(parts ...string) (t series) {
	r.mu.Lock()
	defer r.mu.Unlock()
next:
	for labels, s := range r.series {
		for _, p := range parts {
			if !strings.Contains(labels, p) {
				continue next
			}
		}
		t.count += s.count
		t.sum += s.sum
		for i := range t.bucket {
			t.bucket[i] += s.bucket[i]
		}
	}
	return t
}

// quantile estimates a percentile from the buckets, as a monitoring system does: it knows only
// which bucket the answer is in, and assumes that the requests in it are spread evenly.
func (s series) quantile(q float64) float64 {
	rank, seen := q*float64(s.count), 0.0
	for i, n := range s.bucket {
		if seen+float64(n) >= rank && n > 0 {
			lower, upper := 0.0, 2*buckets[len(buckets)-1]
			if i > 0 {
				lower = buckets[i-1]
			}
			if i < len(buckets) {
				upper = buckets[i]
			}
			return lower + (upper-lower)*(rank-seen)/float64(n)
		}
		seen += float64(n)
	}
	return 0
}

// ------------------------------------------------------------ the service

type entry struct { // one line of the log, as it is read back
	Endpoint string  `json:"endpoint"`
	Status   int     `json:"status"`
	Ms       float64 `json:"ms"`
	Customer int     `json:"customer"`
	Shard    int     `json:"shard"`
	Region   string  `json:"region"`
	Version  string  `json:"version"`
}

func serve(metrics *registry, logs *slog.Logger) *httptest.Server {
	rng, mu := rand.New(rand.NewSource(7)), sync.Mutex{}
	between := func(lo, hi int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return time.Duration(lo+rng.Intn(hi-lo)) * time.Millisecond
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var customer int
		fmt.Sscan(r.URL.Query().Get("customer"), &customer)
		shard, status := customer%20, http.StatusOK
		switch r.URL.Path {
		case "/search":
			time.Sleep(between(10, 40))
		case "/cart":
			time.Sleep(between(4, 16))
		case "/checkout":
			if shard == 7 { // the fault: this shard's database is not answering
				time.Sleep(400 * time.Millisecond)
				status = http.StatusInternalServerError
			} else {
				time.Sleep(between(20, 60))
			}
		}
		w.WriteHeader(status)
		ms := float64(time.Since(start).Microseconds()) / 1000
		// The metric has two labels, with three values and two values: six series, whatever happens.
		class := "2xx"
		if status >= 500 {
			class = "5xx"
		}
		metrics.observe("endpoint="+r.URL.Path+",status="+class, ms)
		// The log line has everything, including what must never be a label.
		logs.Info("request", "endpoint", r.URL.Path, "status", status, "ms", ms, "customer", customer, "shard", shard,
			"region", []string{"eu", "us"}[customer/20%2], "version", []string{"v41", "v42", "v42"}[customer/40%3])
	}))
}

func load(url string, requests int) {
	var wg sync.WaitGroup
	rng := rand.New(rand.NewSource(1))
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200}}
	tick := time.NewTicker(time.Millisecond) // 1,000 requests a second
	defer tick.Stop()
	for i := 0; i < requests; i++ {
		<-tick.C
		path := "/search"
		switch n := rng.Intn(10); {
		case n == 0:
			path = "/checkout"
		case n < 4:
			path = "/cart"
		}
		customer := rng.Intn(2000)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := client.Get(fmt.Sprintf("%s%s?customer=%d", url, path, customer)); err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

func row(name string, all, failed series) {
	fmt.Printf("  %-12s %9d %9.1f%% %9.0f ms %12.0f ms\n", name, all.count, 100*float64(failed.count)/float64(all.count), all.sum/float64(all.count), all.quantile(0.99))
}

// ------------------------------------------------------------ part 1

func part1() (logged []byte, metrics *registry) {
	var buf bytes.Buffer
	var mu sync.Mutex
	metrics = newRegistry()
	logs := slog.New(slog.NewJSONHandler(lockedWriter{&buf, &mu}, nil))
	server := serve(metrics, logs)
	load(server.URL, 10000)
	server.Close()

	fmt.Println("PART 1 — ten seconds, 10,000 requests. Checkout is failing for one customer in twenty.")
	fmt.Println()
	fmt.Println("  1. The totals")
	fmt.Printf("  %-12s %9s %10s %12s %15s\n", "", "requests", "errors", "mean", "99th percentile")
	row("everything", metrics.total(), metrics.total("status=5xx"))
	fmt.Println()
	fmt.Println("  2. The same metric, by its endpoint label")
	fmt.Printf("  %-12s %9s %10s %12s %15s\n", "", "requests", "errors", "mean", "99th percentile")
	for _, e := range []string{"/search", "/cart", "/checkout"} {
		row(e, metrics.total("endpoint="+e+","), metrics.total("endpoint="+e+",", "status=5xx"))
	}

	// 3. The logs: one JSON object for each request. Ask them what the failures have in common.
	var all, failures []entry
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var e entry
		if json.Unmarshal(line, &e) == nil {
			all = append(all, e)
			if e.Status >= 500 {
				failures = append(failures, e)
			}
		}
	}
	fmt.Println()
	fmt.Printf("  3. The logs: %d lines, of which %d are failures. What do the failures have in common?\n", len(all), len(failures))
	fmt.Printf("  %-12s %-22s %26s %26s\n", "attribute", "commonest value", "share of the failures", "share of all requests")
	for _, attr := range []struct {
		name string
		of   func(e entry) string
	}{
		{"endpoint", func(e entry) string { return e.Endpoint }},
		{"region", func(e entry) string { return e.Region }},
		{"version", func(e entry) string { return e.Version }},
		{"shard", func(e entry) string { return fmt.Sprint(e.Shard) }},
	} {
		in := map[string]int{}
		for _, e := range failures {
			in[attr.of(e)]++
		}
		top, most := "", 0
		for v, n := range in {
			if n > most || n == most && v < top {
				top, most = v, n
			}
		}
		overall := 0
		for _, e := range all {
			if attr.of(e) == top {
				overall++
			}
		}
		fmt.Printf("  %-12s %-22s %25.0f%% %25.0f%%\n", attr.name, top, 100*float64(most)/float64(len(failures)), 100*float64(overall)/float64(len(all)))
	}
	customers := map[int]bool{}
	for _, e := range failures {
		customers[e.Customer] = true
	}
	fmt.Printf("  customers affected: %d of 2,000\n", len(customers))
	return buf.Bytes(), metrics
}

type lockedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// ------------------------------------------------------------ part 2

func part2() {
	fmt.Println("PART 2 — what a label costs. One histogram of request times; each row adds a label.")
	fmt.Println()
	fmt.Printf("  %-54s %14s %16s\n", "labels", "time series", "memory, in this")
	fmt.Printf("  %-54s %14s %16s\n", "", "", "program")
	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	for _, c := range []struct {
		name      string
		customers int
		shards    int
	}{
		{"endpoint (3 values), status (2)", 1, 1},
		{"…and shard (20)", 1, 20},
		{"…and customer (2,000)", 2000, 1},
		{"…and customer (200,000)", 200000, 1},
	} {
		before := heap()
		r := newRegistry()
		for _, e := range []string{"/search", "/cart", "/checkout"} {
			for _, s := range []string{"2xx", "5xx"} {
				for sh := 0; sh < c.shards; sh++ {
					for cu := 0; cu < c.customers; cu++ {
						r.observe(fmt.Sprintf("endpoint=%s,status=%s,shard=%d,customer=%d", e, s, sh, cu), 12)
					}
				}
			}
		}
		used := heap() - before
		size := fmt.Sprintf("%.0f KB", float64(used)/(1<<10))
		if used >= 1<<20 {
			size = fmt.Sprintf("%.0f MB", float64(used)/(1<<20))
		}
		fmt.Printf("  %-54s %14d %16s\n", c.name, len(r.series), size)
		runtime.KeepAlive(r)
	}
}

// ------------------------------------------------------------ part 3

func part3(logged []byte, metrics *registry) {
	fmt.Println("PART 3 — the same log, keeping every failure and one success in a hundred.")
	fmt.Println()
	lines := bytes.Split(bytes.TrimSpace(logged), []byte("\n"))
	rng := rand.New(rand.NewSource(3))
	var kept, keptBytes, failures, keptFailures, successes int
	for _, line := range lines {
		failed := bytes.Contains(line, []byte(`"status":500`))
		if failed {
			failures++
		}
		if failed || rng.Intn(100) == 0 {
			kept++
			keptBytes += len(line) + 1
			if failed {
				keptFailures++
			} else {
				successes++
			}
		}
	}
	fmt.Printf("  %-34s %10s %12s %16s\n", "", "lines", "bytes", "failures in it")
	fmt.Printf("  %-34s %10d %12d %16d\n", "every request", len(lines), len(logged), failures)
	fmt.Printf("  %-34s %10d %12d %16d\n", "failures, and 1 success in 100", kept, keptBytes, keptFailures)
	fmt.Println()
	fmt.Printf("  requests, counted by the metric:                 %d\n", metrics.total().count)
	fmt.Printf("  requests, estimated from the sampled log:        %d   (%d successes × 100, and %d failures)\n", successes*100+keptFailures, successes, keptFailures)
}

func main() {
	logged, metrics := part1()
	fmt.Println()
	part2()
	fmt.Println()
	part3(logged, metrics)
}
