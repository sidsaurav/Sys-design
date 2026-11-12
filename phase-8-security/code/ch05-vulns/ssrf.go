// Part 1: a server that fetches a URL for a stranger.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// internal says whether an address is one that a fetch made for a stranger must not reach:
// this machine, the private networks, and the link-local range where a cloud's metadata
// service lives.
func internal(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()
}

// The network of part 1 is real HTTP over loopback, with two tables in front of it. One
// stands in for DNS and one for routing, so that the code can judge addresses as it would
// in production while every byte stays on this machine.
type network struct {
	dns    map[string][]string // name → its answers, in order; the last one repeats
	asked  map[string]int      // how many times each name has been looked up
	routes map[string]string   // "address:port" → the local listener that plays it
}

func (n *network) resolve(host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	answers := n.dns[host]
	if len(answers) == 0 {
		return nil, errors.New("no such host")
	}
	i := n.asked[host]
	n.asked[host]++
	if i >= len(answers) {
		i = len(answers) - 1
	}
	return net.ParseIP(answers[i]), nil
}

// client makes an HTTP client on this network. The guard, if there is one, sees the address
// of every connection at the moment it is made: the first, and each one after a redirect.
func (n *network) client(guard func(net.IP) error) *http.Client {
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ip, err := n.resolve(host)
		if err != nil {
			return nil, err
		}
		if guard != nil {
			if err := guard(ip); err != nil {
				return nil, err
			}
		}
		local, ok := n.routes[net.JoinHostPort(ip.String(), port)]
		if !ok {
			return nil, errors.New("no route to host")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", local)
	}
	return &http.Client{
		Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
}

var errInternal = errors.New("refused: an internal address")

// A fetcher is one version of the code behind "import an invoice from a URL".
type fetcher struct {
	what  string
	fetch func(n *network, raw string) (string, error)
}

func get(c *http.Client, raw string) (string, error) {
	resp, err := c.Get(raw)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(body), err
}

var fetchers = []fetcher{
	{"fetches whatever it is given", func(n *network, raw string) (string, error) {
		return get(n.client(nil), raw)
	}},
	{"refuses a URL that is written with an internal address", func(n *network, raw string) (string, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && internal(ip) {
			return "", errInternal
		}
		return get(n.client(nil), raw)
	}},
	{"looks the name up first, and refuses if the answer is internal", func(n *network, raw string) (string, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if ip, err := n.resolve(u.Hostname()); err != nil || internal(ip) {
			return "", errInternal
		}
		return get(n.client(nil), raw) // …and then the client looks the name up again
	}},
	{"refuses at the moment of connecting, on every connection", func(n *network, raw string) (string, error) {
		return get(n.client(func(ip net.IP) error {
			if internal(ip) {
				return errInternal
			}
			return nil
		}), raw)
	}},
}

// guarded is the fourth fetcher's rule as it is written for a real network: a hook that
// the dialer calls with the address it is about to connect to, after any name has been
// looked up and whatever redirect led there.
func guarded() *net.Dialer {
	return &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil || internal(net.ParseIP(host)) {
			return errInternal
		}
		return nil
	}}
}

func part1() {
	secret := randomHex(16) // what the metadata service hands to anything that asks
	const credentials = "/latest/meta-data/iam/security-credentials/app-role"
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "temporary credentials for app-role: "+secret)
	}))
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "admin panel: "+secret)
	}))
	customer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "invoice 20417, 3 pages")
	}))
	// The attacker's own server sends every visitor somewhere else.
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254"+credentials, http.StatusFound)
			return
		}
		http.Redirect(w, r, "http://"+r.Host+r.URL.Path, http.StatusFound) // come back, and look me up again
	}))
	defer metadata.Close()
	defer admin.Close()
	defer customer.Close()
	defer attacker.Close()
	local := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	newNetwork := func() *network {
		return &network{
			asked: map[string]int{},
			dns: map[string][]string{
				"files.customer.example": {"203.0.113.10"},
				"evil.example":           {"198.51.100.66"},
				"meta.evil.example":      {"169.254.169.254"},                  // the attacker's name, pointed inwards
				"rebind.evil.example":    {"198.51.100.66", "169.254.169.254"}, // one answer, then another
			},
			routes: map[string]string{
				"203.0.113.10:80":    local(customer),
				"198.51.100.66:80":   local(attacker),
				"169.254.169.254:80": local(metadata),
				"10.0.0.5:8080":      local(admin),
			},
		}
	}
	urls := []struct{ what, url string }{
		{"an invoice on a customer's site", "http://files.customer.example/invoice.pdf"},
		{"the metadata service, by its address", "http://169.254.169.254" + credentials},
		{"an internal admin page, by its address", "http://10.0.0.5:8080/admin"},
		{"the attacker's own name for the metadata address", "http://meta.evil.example" + credentials},
		{"a public page that redirects to the metadata service", "http://evil.example/redirect"},
		{"a name that answers differently the second time", "http://rebind.evil.example" + credentials},
	}

	fmt.Println("PART 1 — \"import an invoice from a URL\". Six URLs, and four versions of the code that fetches.")
	fmt.Println()
	fmt.Printf("  %-54s %-9s %-9s %-9s %s\n", "the URL that the caller gives", "1", "2", "3", "4")
	for _, u := range urls {
		line := fmt.Sprintf("  %-54s", u.what)
		for _, f := range fetchers {
			body, err := f.fetch(newNetwork(), u.url)
			cell := "fetched"
			switch {
			case errors.Is(err, errInternal):
				cell = "refused"
			case err != nil:
				cell = "failed"
			case strings.Contains(body, secret):
				cell = "LEAKED"
			}
			line += fmt.Sprintf(" %-9s", cell)
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
	fmt.Println()
	for i, f := range fetchers {
		fmt.Printf("  %d  %s\n", i+1, f.what)
	}

	// The same rule on a real connection: a listener on this machine, and a dialer with the hook.
	_, err := guarded().Dial("tcp", local(metadata))
	fmt.Println()
	fmt.Printf("  The rule of version 4 as a hook on Go's own dialer, tried on a real listener at %s:\n", strings.Split(local(metadata), ":")[0])
	if errors.Is(err, errInternal) {
		fmt.Println("    " + errInternal.Error() + ", before any connection was made")
	} else {
		fmt.Printf("    %v\n", err)
	}
}
