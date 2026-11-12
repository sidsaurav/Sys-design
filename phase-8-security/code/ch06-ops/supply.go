// Part 4: a build that fetches somebody else's code, and what it checks first.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
)

// A registry serves versions of one package, by name. Whoever controls it, or the
// maintainer's account on it, decides what each name gives.
type registry struct {
	*httptest.Server
	mu       sync.Mutex
	versions map[string]string // version → the code
}

func newRegistry() *registry {
	r := &registry{versions: map[string]string{
		"1.2.0": "tinycsv 1.2.0: reads CSV",
		"1.3.0": "tinycsv 1.3.0: reads CSV, and quoted fields",
	}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.URL.Path == "/tinycsv/versions" {
			var all []string
			for v := range r.versions {
				all = append(all, v)
			}
			sort.Strings(all)
			fmt.Fprint(w, strings.Join(all, "\n"))
			return
		}
		code, ok := r.versions[strings.TrimPrefix(req.URL.Path, "/tinycsv/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		fmt.Fprint(w, code)
	}))
	return r
}

func (r *registry) get(path string) string {
	resp, err := http.Get(r.URL + path)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func digest(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func part4() {
	const stolen = "and posts the build's secrets to evil.example"
	reviewed := newRegistry()
	locked := digest(reviewed.get("/tinycsv/1.3.0")) // written to the lockfile on the day that 1.3.0 was reviewed
	reviewed.Close()

	builds := []struct {
		what  string
		fetch func(r *registry) (code string, err error)
	}{
		{"the newest 1.3.x, at every build", func(r *registry) (string, error) {
			newest := ""
			for _, v := range strings.Split(r.get("/tinycsv/versions"), "\n") {
				if strings.HasPrefix(v, "1.3.") {
					newest = v // the list is sorted
				}
			}
			return r.get("/tinycsv/" + newest), nil
		}},
		{"1.3.0, by its name", func(r *registry) (string, error) {
			return r.get("/tinycsv/1.3.0"), nil
		}},
		{"1.3.0, and the hash in the lockfile", func(r *registry) (string, error) {
			code := r.get("/tinycsv/1.3.0")
			if digest(code) != locked {
				return "", fmt.Errorf("checksum mismatch")
			}
			return code, nil
		}},
	}
	attacks := []struct {
		what string
		do   func(r *registry)
	}{
		{"publishes a new version, 1.3.1", func(r *registry) { r.versions["1.3.1"] = "tinycsv 1.3.1: reads CSV, " + stolen }},
		{"replaces 1.3.0 where it is", func(r *registry) { r.versions["1.3.0"] = "tinycsv 1.3.0: reads CSV, " + stolen }},
	}
	fmt.Println("PART 4 — the build uses a small library, tinycsv, and version 1.3.0 was read before it was adopted.")
	fmt.Println("Then somebody takes over the maintainer's account at the registry.")
	fmt.Println()
	fmt.Printf("  %-36s %-31s %s\n", "how the build asks for the library", attacks[0].what, attacks[1].what)
	for _, b := range builds {
		line := fmt.Sprintf("  %-36s", b.what)
		for i, a := range attacks {
			r := newRegistry()
			a.do(r)
			code, err := b.fetch(r)
			r.Close()
			result := "builds what was reviewed"
			switch {
			case err != nil:
				result = "stops: " + err.Error()
			case strings.Contains(code, stolen):
				result = "BUILDS THE ATTACKER'S CODE"
			}
			if i == 0 {
				line += fmt.Sprintf(" %-31s", result)
			} else {
				line += " " + result
			}
		}
		fmt.Println(line)
	}
}
