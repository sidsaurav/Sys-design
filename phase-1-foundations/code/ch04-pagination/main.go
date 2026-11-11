// Chapter 1.4 — offset pagination versus cursor pagination when the data
// changes between pages, plus a machine-readable error format.
//
// Run:
//
//	go run ./phase-1-foundations/code/ch04-pagination
//
// The server keeps a list of items, newest first, and offers the same list
// two ways:
//
//	GET /v1/items?limit=3&offset=3     "skip 3, give me 3"
//	GET /v2/items?limit=3&cursor=...   "give me the 3 after the last one I saw"
//
// The walk-through reads page 1, changes the data, then reads page 2.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
)

const (
	defaultLimit = 3 // tiny, so the demo is readable; real APIs use 20 to 100
	maxLimit     = 100
)

type item struct {
	ID    int    `json:"id"`    // also the creation order: a bigger id is newer
	Title string `json:"title"` // (a real table would sort by created_at, then id)
}

// store keeps items sorted newest first.
type store struct {
	mu    sync.Mutex
	items []item
	next  int
}

func (s *store) add(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.items = append([]item{{ID: s.next, Title: title}}, s.items...)
}

func (s *store) remove(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.items {
		if it.ID == id {
			s.items = append(s.items[:i:i], s.items[i+1:]...)
			return
		}
	}
}

// pageByOffset is "ORDER BY id DESC LIMIT limit OFFSET offset".
func (s *store) pageByOffset(offset, limit int) []item {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset > len(s.items) {
		offset = len(s.items)
	}
	end := min(offset+limit, len(s.items))
	return append([]item(nil), s.items[offset:end]...)
}

// pageAfter is "WHERE id < afterID ORDER BY id DESC LIMIT limit".
// It finds its starting point by value, not by position, so it does not
// matter how many rows were added or removed in front of it.
func (s *store) pageAfter(afterID, limit int) []item {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := sort.Search(len(s.items), func(i int) bool { return s.items[i].ID < afterID })
	end := min(start+limit, len(s.items))
	return append([]item(nil), s.items[start:end]...)
}

// A cursor says where the next page starts. Clients must treat it as an
// opaque string: today it holds the last id seen, tomorrow it may hold
// something else, and no client should break when that changes.
type cursor struct {
	AfterID int `json:"after_id"`
}

func encodeCursor(c cursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c)
	return c, err
}

// problem is an error body in the "problem details" format of RFC 9457.
type problem struct {
	Type   string `json:"type"`   // a URI that identifies the kind of problem
	Title  string `json:"title"`  // short, the same for every occurrence
	Status int    `json:"status"` // the HTTP status code, repeated
	Detail string `json:"detail"` // what went wrong this time
}

func writeProblem(w http.ResponseWriter, status int, kind, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(problem{
		Type:   "https://api.example.com/problems/" + kind,
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

// parseLimit applies a default and a maximum. A missing limit is fine, a
// huge one is quietly reduced, and one that is not a number is the client's
// mistake.
func parseLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("limit must be a positive integer, got %q", raw)
	}
	return min(n, maxLimit), nil
}

func newServer(s *store) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/items", func(w http.ResponseWriter, r *http.Request) {
		limit, err := parseLimit(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-parameter", "Invalid query parameter", err.Error())
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		items := s.pageByOffset(offset, limit)
		writeJSON(w, map[string]any{"items": items, "next_offset": offset + len(items)})
	})

	mux.HandleFunc("GET /v2/items", func(w http.ResponseWriter, r *http.Request) {
		limit, err := parseLimit(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-parameter", "Invalid query parameter", err.Error())
			return
		}
		after := int(^uint(0) >> 1) // no cursor: start from the newest item
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			c, err := decodeCursor(raw)
			if err != nil {
				writeProblem(w, http.StatusBadRequest, "invalid-cursor", "Invalid cursor",
					"The cursor is not one this API issued. Start again without a cursor.")
				return
			}
			after = c.AfterID
		}
		items := s.pageAfter(after, limit)
		resp := map[string]any{"items": items}
		// Only offer a next page if this one was full.
		if len(items) == limit {
			resp["next_cursor"] = encodeCursor(cursor{AfterID: items[len(items)-1].ID})
		}
		writeJSON(w, resp)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func main() {
	fmt.Println("=== Offset pagination ===")
	walkThrough(func(base string) (page1, page2 func() []item) {
		return func() []item { return fetch(base + "/v1/items?limit=3&offset=0").Items },
			func() []item { return fetch(base + "/v1/items?limit=3&offset=3").Items }
	})

	fmt.Println("\n=== Cursor pagination ===")
	walkThrough(func(base string) (page1, page2 func() []item) {
		var next string
		return func() []item {
				p := fetch(base + "/v2/items?limit=3")
				next = p.NextCursor
				return p.Items
			}, func() []item {
				return fetch(base + "/v2/items?limit=3&cursor=" + next).Items
			}
	})

	fmt.Println("\n=== A machine-readable error ===")
	srv := httptest.NewServer(newServer(&store{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v2/items?cursor=not-a-real-cursor")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("%s\nContent-Type: %s\n%s", resp.Status, resp.Header.Get("Content-Type"), body)
}

// walkThrough runs the same two scenarios against a fresh server each time:
// items are added between page 1 and page 2, then an item is removed.
func walkThrough(pager func(base string) (page1, page2 func() []item)) {
	for _, scenario := range []struct {
		name   string
		change func(*store)
	}{
		{"two new items arrive between page 1 and page 2", func(s *store) { s.add("new"); s.add("new") }},
		{"item 9 is deleted between page 1 and page 2", func(s *store) { s.remove(9) }},
	} {
		s := &store{}
		for i := 1; i <= 9; i++ {
			s.add("item")
		}
		srv := httptest.NewServer(newServer(s))
		page1, page2 := pager(srv.URL)

		first := page1()
		scenario.change(s)
		second := page2()
		srv.Close()

		fmt.Printf("%s\n  page 1: %v\n  page 2: %v   %s\n", scenario.name, ids(first), ids(second), verdict(first, second))
	}
}

// verdict compares what the client saw with what it should have seen:
// page 1 was 9 8 7, so page 2 should be 6 5 4.
func verdict(first, second []item) string {
	seen := map[int]bool{}
	for _, it := range first {
		seen[it.ID] = true
	}
	for _, it := range second {
		if seen[it.ID] {
			return fmt.Sprintf("<- item %d shown twice", it.ID)
		}
	}
	if len(second) > 0 && second[0].ID != first[len(first)-1].ID-1 {
		return fmt.Sprintf("<- item %d never shown", first[len(first)-1].ID-1)
	}
	return "<- correct"
}

type page struct {
	Items      []item `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func fetch(url string) page {
	resp, err := http.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	var p page
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		log.Fatal(err)
	}
	return p
}

func ids(items []item) []int {
	out := make([]int, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
