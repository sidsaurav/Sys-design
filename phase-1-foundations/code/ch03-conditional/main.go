// Chapter 1.3 — HTTP semantics you can observe: validators (ETag), conditional
// requests, and what "idempotent" does and does not promise.
//
// Run:
//
//	go run ./phase-1-foundations/code/ch03-conditional          # scripted walk-through
//	go run ./phase-1-foundations/code/ch03-conditional -serve   # serve on :8080 and use curl
//
// The server stores one document. Every change gives it a new version, and
// the version is sent to clients as an ETag. Clients send the ETag back to
// ask "has it changed?" (If-None-Match) or to say "only apply my change if
// nobody else changed it first" (If-Match).
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// store holds one document and its version.
type store struct {
	mu      sync.Mutex
	exists  bool
	body    string
	version int
}

// etag formats the version as an entity tag. Entity tags are quoted strings.
func (s *store) etag() string { return fmt.Sprintf(`"v%d"`, s.version) }

func newServer() http.Handler {
	s := &store{exists: true, body: "first draft", version: 1}
	mux := http.NewServeMux()

	// GET is safe: it changes nothing, so anyone may repeat or cache it.
	mux.HandleFunc("GET /doc", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.exists {
			http.Error(w, "no such document", http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", s.etag())
		// "If you already have this exact version, I will not send it again."
		// (A complete server also accepts a list of tags and "*";
		// see RFC 9110, section 13.1.2.)
		if r.Header.Get("If-None-Match") == s.etag() {
			w.WriteHeader(http.StatusNotModified) // 304, no body
			return
		}
		fmt.Fprint(w, s.body)
	})

	// PUT replaces the whole document. It is idempotent: sending the same
	// PUT twice leaves the same state as sending it once.
	mux.HandleFunc("PUT /doc", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()

		ifMatch := r.Header.Get("If-Match")
		if ifMatch == "" {
			// We refuse blind overwrites: the client must say which
			// version it is replacing.
			http.Error(w, "send If-Match with the ETag you last read", http.StatusPreconditionRequired) // 428
			return
		}
		if !s.exists || ifMatch != s.etag() {
			// Someone else changed the document since this client read it.
			w.Header().Set("ETag", s.etag())
			http.Error(w, "the document has changed; fetch it again", http.StatusPreconditionFailed) // 412
			return
		}
		s.body = string(body)
		s.version++
		w.Header().Set("ETag", s.etag())
		w.WriteHeader(http.StatusNoContent) // 204
	})

	// DELETE is idempotent too. The second DELETE answers 404, but the
	// state of the server is the same as after the first: no document.
	mux.HandleFunc("DELETE /doc", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.exists {
			http.Error(w, "no such document", http.StatusNotFound)
			return
		}
		s.exists = false
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func main() {
	serve := flag.Bool("serve", false, "serve on :8080 instead of running the walk-through")
	flag.Parse()

	if *serve {
		log.Println("listening on http://localhost:8080/doc")
		log.Fatal(http.ListenAndServe("localhost:8080", newServer()))
	}

	srv := httptest.NewServer(newServer())
	defer srv.Close()
	url := srv.URL + "/doc"

	fmt.Println("1. Caching: ask again only if it changed")
	etag := do("GET", url, "", "")
	do("GET", url, "If-None-Match: "+etag, "")

	fmt.Println("\n2. Two people edit the same version")
	fmt.Println("   Alice and Bob have both read " + etag)
	newTag := do("PUT", url, "If-Match: "+etag, "Alice's edit")
	do("PUT", url, "If-Match: "+etag, "Bob's edit") // stale: must fail
	fmt.Println("   Bob fetches the new version and tries again")
	do("GET", url, "", "")
	do("PUT", url, "If-Match: "+newTag, "Alice's edit + Bob's edit")

	fmt.Println("\n3. A write that does not say what it is replacing")
	do("PUT", url, "", "blind overwrite")

	fmt.Println("\n4. Idempotent does not mean 'same response'")
	do("DELETE", url, "", "")
	do("DELETE", url, "", "")
}

// do sends one request, prints one line about it, and returns the ETag of
// the response.
func do(method, url, header, body string) string {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	if name, value, ok := strings.Cut(header, ": "); ok {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	line := fmt.Sprintf("   %-6s /doc  %-22s -> %s", method, header, resp.Status)
	if tag := resp.Header.Get("ETag"); tag != "" {
		line += "  ETag: " + tag
	}
	if resp.StatusCode == http.StatusOK && len(respBody) > 0 {
		line += fmt.Sprintf("  body: %q", strings.TrimSpace(string(respBody)))
	}
	fmt.Println(line)
	return resp.Header.Get("ETag")
}
