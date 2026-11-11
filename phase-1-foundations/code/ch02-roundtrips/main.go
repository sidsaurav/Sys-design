// Chapter 1.2 — count the round trips a request pays before your handler runs.
//
// Run:
//
//	go run ./phase-1-foundations/code/ch02-roundtrips
//	go run ./phase-1-foundations/code/ch02-roundtrips -oneway 75ms
//	go run ./phase-1-foundations/code/ch02-roundtrips -no-reuse
//	go run ./phase-1-foundations/code/ch02-roundtrips -url https://example.com
//
// With no -url, the program starts an HTTPS server on this machine and talks
// to it over a connection that delays every byte by -oneway in each direction,
// as if the server were far away. The TLS and HTTP exchanges are real, so the
// round trips you see are measured, not assumed. The one thing that cannot be
// slowed down portably is the kernel's TCP handshake; the dialer stands in for
// it by waiting exactly one round trip, which is what a TCP handshake costs.
//
// With -url, it traces a real request to a real site instead.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"time"
)

func main() {
	oneWay := flag.Duration("oneway", 50*time.Millisecond, "simulated one-way network delay")
	url := flag.String("url", "", "trace a real URL instead of the local simulation")
	noReuse := flag.Bool("no-reuse", false, "close the connection after every request (the bug)")
	flag.Parse()

	if *url != "" {
		traceRealSite(*url)
		return
	}

	// The handler does almost no work, so everything we measure is network.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello")
	}))
	defer server.Close()

	rtt := 2 * *oneWay
	fmt.Printf("Simulated network: %v each way, so one round trip (RTT) = %v\n", *oneWay, rtt)
	fmt.Println("The handler itself takes well under a millisecond.")

	for _, v := range []struct {
		name    string
		version uint16
	}{
		{"TLS 1.3", tls.VersionTLS13},
		{"TLS 1.2", tls.VersionTLS12},
	} {
		// Start from the test server's client (it already trusts the test
		// certificate), then make its connections slow and pin the TLS version.
		transport := server.Client().Transport.(*http.Transport).Clone()
		transport.TLSClientConfig.MinVersion = v.version
		transport.TLSClientConfig.MaxVersion = v.version
		// With keep-alives disabled every request opens a new connection.
		// It has the same effect as building a new client per request.
		transport.DisableKeepAlives = *noReuse
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			time.Sleep(rtt) // stands in for SYN -> SYN-ACK: one round trip
			return newSlowConn(conn, *oneWay), nil
		}
		client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

		fmt.Printf("\n%s\n", v.name)
		fmt.Println("  request            connect    TLS     request->1st byte   total   round trips")
		for _, label := range []string{"1st", "2nd", "3rd"} {
			t := tracedGet(client, server.URL)
			if t.reused {
				label += " (reused conn)"
			} else {
				label += " (new conn)"
			}
			fmt.Printf("  %-17s %7s %7s %12s %12s %8.1f\n", label,
				ms(t.connect), ms(t.tlsHandshake), ms(t.firstByte), ms(t.total),
				float64(t.total)/float64(rtt))
		}
		transport.CloseIdleConnections()
	}
}

// timings is how long each phase of one request took.
type timings struct {
	dns, connect, tlsHandshake, firstByte, total time.Duration
	reused                                       bool
}

// tracedGet performs one GET and records when each phase started and ended,
// using the hooks in net/http/httptrace.
func tracedGet(client *http.Client, url string) timings {
	var t timings
	var start, dnsStart, connStart, tlsStart, wrote time.Time

	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:  func(httptrace.DNSDoneInfo) { t.dns = time.Since(dnsStart) },
		// GetConn fires when the client starts looking for a connection,
		// GotConn when it has one (new or reused).
		GetConn: func(string) { connStart = time.Now() },
		GotConn: func(info httptrace.GotConnInfo) {
			t.reused = info.Reused
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
			t.connect = tlsStart.Sub(connStart) - t.dns
		},
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.tlsHandshake = time.Since(tlsStart) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { wrote = time.Now() },
		GotFirstResponseByte: func() { t.firstByte = time.Since(wrote) },
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fatal(err)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	start = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fatal(err)
	}
	// Read the body to the end and close it. If you skip this, the
	// connection cannot be reused and every request pays for a new one.
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	t.total = time.Since(start)
	return t
}

// traceRealSite sends two requests to a real URL so you can compare a new
// connection with a reused one on a real network.
func traceRealSite(url string) {
	client := &http.Client{Timeout: 30 * time.Second}
	fmt.Println("request            DNS    connect    TLS     request->1st byte   total   reused")
	for _, label := range []string{"1st", "2nd"} {
		t := tracedGet(client, url)
		fmt.Printf("%-14s %7s %7s %7s %12s %12s     %v\n", label,
			ms(t.dns), ms(t.connect), ms(t.tlsHandshake), ms(t.firstByte), ms(t.total), t.reused)
	}
}

// slowConn wraps a connection and delays every byte by a fixed time in each
// direction, without making later bytes wait for earlier ones (the way a long
// cable behaves: everything arrives late, nothing arrives slower).
type slowConn struct {
	net.Conn
	delay    time.Duration
	outgoing chan packet
	incoming chan packet
	leftover []byte
}

type packet struct {
	data []byte
	due  time.Time
	err  error
}

func newSlowConn(conn net.Conn, delay time.Duration) *slowConn {
	c := &slowConn{
		Conn:     conn,
		delay:    delay,
		outgoing: make(chan packet, 1024),
		incoming: make(chan packet, 1024),
	}
	// Deliver our writes to the real connection once they are due.
	go func() {
		for p := range c.outgoing {
			time.Sleep(time.Until(p.due))
			if _, err := conn.Write(p.data); err != nil {
				return
			}
		}
	}()
	// Read from the real connection as data arrives, and stamp each chunk
	// with the time at which the application is allowed to see it.
	go func() {
		for {
			buf := make([]byte, 32*1024)
			n, err := conn.Read(buf)
			c.incoming <- packet{data: buf[:n], due: time.Now().Add(delay), err: err}
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *slowConn) Write(b []byte) (int, error) {
	data := make([]byte, len(b))
	copy(data, b)
	c.outgoing <- packet{data: data, due: time.Now().Add(c.delay)}
	return len(b), nil
}

func (c *slowConn) Read(b []byte) (int, error) {
	if len(c.leftover) == 0 {
		p := <-c.incoming
		time.Sleep(time.Until(p.due))
		if len(p.data) == 0 && p.err != nil {
			return 0, p.err
		}
		c.leftover = p.data
	}
	n := copy(b, c.leftover)
	c.leftover = c.leftover[n:]
	return n, nil
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%dms", d.Round(time.Millisecond).Milliseconds())
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
