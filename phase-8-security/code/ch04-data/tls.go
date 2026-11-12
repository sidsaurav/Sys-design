// Parts 1 and 2: certificates, and what a client and a server each learn from them.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An authority signs certificates. The company has one of its own. So does the attacker:
// making one takes a few lines, and nobody's permission.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func serial() *big.Int {
	return new(big.Int).SetBytes(randomBytes(16))
}

func newAuthority(name string) *authority {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-72 * time.Hour),
		NotAfter:              time.Now().Add(72 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	check(err)
	cert, err := x509.ParseCertificate(der)
	check(err)
	return &authority{cert, key}
}

func (a *authority) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(a.cert)
	return p
}

// issue makes a certificate for a name, good until the given moment. A server shows it to
// say where it answers. A client shows it to say who it is.
func (a *authority) issue(name string, until time.Time) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     until,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	check(err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var (
	tomorrow  = time.Now().Add(24 * time.Hour)
	yesterday = time.Now().Add(-24 * time.Hour)
)

// A site is an HTTPS server that shows one certificate, and keeps the secrets sent to it.
type site struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func serve(cert tls.Certificate) *site {
	s := &site{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Get("Authorization"))
		s.mu.Unlock()
		fmt.Fprint(w, "balance: 1,204.50")
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are the point here
	s.StartTLS()
	return s
}

func client(cfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
}

// send makes one request with a secret in it, and says what came of it.
func send(c *http.Client, url, secret string) (status string, body string) {
	req, err := http.NewRequest("GET", url, nil)
	check(err)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := c.Do(req)
	if err != nil {
		return "refuses: " + why(err), ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strconv.Itoa(resp.StatusCode), strings.TrimSpace(string(b))
}

// why turns a certificate that failed verification into a few words.
func why(err error) string {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var name x509.HostnameError
	switch {
	case errors.As(err, &unknown):
		return "unknown authority"
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return "expired"
	case errors.As(err, &name):
		return "wrong name"
	}
	return err.Error()
}

func part1(ours, theirs *authority) {
	secret := hex.EncodeToString(randomBytes(16))
	servers := []struct {
		what string
		site *site
	}{
		{"the ledger; a certificate from our authority", serve(ours.issue("ledger.internal", tomorrow))},
		{"an impostor; a certificate from its own authority", serve(theirs.issue("ledger.internal", tomorrow))},
		{"our reports service; its own certificate", serve(ours.issue("reports.internal", tomorrow))},
		{"the ledger; a certificate that ran out yesterday", serve(ours.issue("ledger.internal", yesterday))},
	}
	// The careful client trusts one authority, and expects one name.
	careful := client(&tls.Config{RootCAs: ours.pool(), ServerName: "ledger.internal"})
	// The careless client was told to stop complaining about certificates.
	careless := client(&tls.Config{InsecureSkipVerify: true})

	fmt.Println("PART 1 — is that really the ledger? Two clients send a request, with a secret in it, to whatever")
	fmt.Println("answers at the ledger's address.")
	fmt.Println()
	fmt.Printf("  %-50s %-27s %s\n", "what answers, and what it shows", "the client that verifies", "the one that does not")
	for _, s := range servers {
		a, _ := send(careful, s.site.URL, secret)
		b, _ := send(careless, s.site.URL, secret)
		fmt.Printf("  %-50s %-27s %s\n", s.what, a, b)
	}
	fmt.Println()
	got := 0
	for _, seen := range servers[1].site.seen {
		if seen == "Bearer "+secret {
			got++
		}
	}
	if len(servers[1].site.seen) == 1 && got == 1 {
		fmt.Println("  The impostor received one request, and the caller's secret was in it.")
	} else {
		fmt.Printf("  The impostor received %d requests, %d of them with the caller's secret.\n", len(servers[1].site.seen), got)
	}
	for _, s := range servers {
		s.site.Close()
	}
}

// handshakes collects what a server's own log says about the handshakes that it ended.
type handshakes struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (h *handshakes) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.Write(p)
}

// last waits a moment for the server to write its line, and turns it into a few words.
func (h *handshakes) last() string {
	for i := 0; i < 100; i++ {
		h.mu.Lock()
		line := h.buf.String()
		h.buf.Reset()
		h.mu.Unlock()
		switch {
		case strings.Contains(line, "didn't provide a certificate"):
			return "no certificate"
		case strings.Contains(line, "unknown authority"):
			return "unknown authority"
		case strings.Contains(line, "expired"):
			return "expired"
		case line != "":
			return strings.TrimSpace(line)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "no reason was logged"
}

func part2(ours, theirs *authority) {
	allowed := map[string]bool{"invoices": true}
	refused := &handshakes{}
	ledger := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := r.TLS.PeerCertificates[0].Subject.CommonName // verified in the handshake, before this runs
		if !allowed[who] {
			http.Error(w, who, 403)
			return
		}
		fmt.Fprint(w, who)
	}))
	ledger.TLS = &tls.Config{
		Certificates: []tls.Certificate{ours.issue("ledger.internal", tomorrow)},
		ClientAuth:   tls.RequireAndVerifyClientCert, // no certificate from our authority, no connection
		ClientCAs:    ours.pool(),
	}
	ledger.Config.ErrorLog = log.New(refused, "", 0)
	ledger.StartTLS()
	defer ledger.Close()

	invoices, stranger := ours.issue("invoices", tomorrow), theirs.issue("invoices", tomorrow)
	stale, reports := ours.issue("invoices", yesterday), ours.issue("reports", tomorrow)
	callers := []struct {
		who, shows string
		cert       *tls.Certificate
	}{
		{"the invoice service", "from our authority", &invoices},
		{"a process on the network", "none", nil},
		{"the same process", "from its own authority", &stranger},
		{"the invoice service", "ours, ran out yesterday", &stale},
		{"the reports service", "from our authority", &reports},
	}
	fmt.Println("PART 2 — who is calling the ledger? It asks every caller for a certificate from our authority,")
	fmt.Println("and it serves one caller only, \"invoices\".")
	fmt.Println()
	fmt.Printf("  %-25s %-26s %s\n", "the caller", "the certificate it shows", "what the ledger does")
	for _, c := range callers {
		cfg := &tls.Config{RootCAs: ours.pool(), ServerName: "ledger.internal"}
		if c.cert != nil {
			// Show this certificate whatever the server says it will accept, as an attacker would.
			cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return c.cert, nil }
		}
		status, body := send(client(cfg), ledger.URL, "")
		answer := ""
		switch status {
		case "200":
			answer = fmt.Sprintf("serves it: the caller is %q", body)
		case "403":
			answer = fmt.Sprintf("completes the handshake, then 403 for %q", body)
		default:
			answer = "ends the handshake: " + refused.last()
		}
		fmt.Printf("  %-25s %-26s %s\n", c.who, c.shows, answer)
	}
}
