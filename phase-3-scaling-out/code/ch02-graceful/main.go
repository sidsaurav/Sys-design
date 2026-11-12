// Chapter 3.2 — a server that can be stopped without dropping a request.
//
// The shutdown sequence every service behind a load balancer needs:
//
//  1. told to stop (SIGTERM, or Ctrl-C)  →  start failing the health check,
//     and keep serving;
//  2. wait until the load balancer has noticed and stopped sending requests;
//  3. finish the requests already in progress, up to a deadline;
//  4. exit.
//
// Run it. In a second terminal, start a slow request; then press Ctrl-C in
// the first terminal while the request is still running:
//
//	go run ./phase-3-scaling-out/code/ch02-graceful
//
//	curl -s localhost:8080/slow &                                      # takes 3 seconds
//	curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/healthz    # 200
//	                                                                   # ...now press Ctrl-C in the first terminal
//	curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/healthz    # 503: "send me no more"
//	wait                                                               # the slow request still completes
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	notice := flag.Duration("notice", 2*time.Second, "how long the load balancer needs to notice a failing health check")
	deadline := flag.Duration("deadline", 10*time.Second, "how long requests in progress are given to finish")
	flag.Parse()

	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprintln(w, "done, after 3 seconds")
	})
	srv := &http.Server{Addr: *addr, Handler: mux}

	// ctx is cancelled when the process is told to stop.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	ready.Store(true) // only now: a real service would first warm up and check its own configuration
	log.Printf("serving on %s", *addr)

	<-ctx.Done()
	log.Printf("told to stop: failing the health check, still serving")
	ready.Store(false)

	time.Sleep(*notice) // interval × threshold of the load balancer's health check, and a little more
	log.Printf("finishing requests in progress (up to %v)", *deadline)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("some requests did not finish in time: %v", err)
	}
	log.Printf("stopped")
}
