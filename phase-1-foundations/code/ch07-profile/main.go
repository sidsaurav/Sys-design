// Chapter 1.7 — find a bottleneck by profiling instead of guessing.
//
// Run:
//
//	go run ./phase-1-foundations/code/ch07-profile            # the slow handler
//	go tool pprof -top -nodecount=12 cpu.prof                 # where the CPU time went
//	go run ./phase-1-foundations/code/ch07-profile -fixed     # after the fix
//
// The program calls an HTTP handler as fast as it can for three seconds,
// records a CPU profile while it does so, and prints the throughput.
// The handler has one expensive mistake in it. The exercise is to find it
// from the profile before you read the code.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"
)

const emailPattern = `^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`

// Compiled once, when the program starts.
var emailRE = regexp.MustCompile(emailPattern)

type signup struct {
	Email string `json:"email"`
	Valid bool   `json:"valid"`
}

func slowHandler(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	valid := regexp.MustCompile(emailPattern).MatchString(email)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(signup{Email: email, Valid: valid})
}

func fixedHandler(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	valid := emailRE.MatchString(email)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(signup{Email: email, Valid: valid})
}

func main() {
	fixed := flag.Bool("fixed", false, "use the fixed handler")
	flag.Parse()

	handler, name := http.HandlerFunc(slowHandler), "slow handler"
	if *fixed {
		handler, name = http.HandlerFunc(fixedHandler), "fixed handler"
	}

	// Record where the CPU time goes while the load is running.
	out, err := os.Create("cpu.prof")
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()
	if err := pprof.StartCPUProfile(out); err != nil {
		log.Fatal(err)
	}

	// Call the handler directly from one goroutine per core, so that the
	// only thing using CPU is the handler itself.
	const duration = 3 * time.Second
	var handled atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(duration)
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/signup?email=ada@example.com", nil)
			for time.Now().Before(deadline) {
				handler.ServeHTTP(httptest.NewRecorder(), req)
				handled.Add(1)
			}
		}()
	}
	wg.Wait()
	pprof.StopCPUProfile()

	fmt.Printf("%s: %d requests in %v on %d cores = %.0f requests per second\n",
		name, handled.Load(), duration, runtime.NumCPU(), float64(handled.Load())/duration.Seconds())
	fmt.Println("CPU profile written to cpu.prof — read it with: go tool pprof -top -nodecount=12 cpu.prof")
}
