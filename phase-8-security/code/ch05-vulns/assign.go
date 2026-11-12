// Part 2: a request that names more fields than the caller may change.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
)

type invoice struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Pence  int    `json:"pence"`
	Paid   bool   `json:"paid"`
	Note   string `json:"note"`
}

// bindAll reads the request straight into the stored invoice: every field that the request
// names is written.
func bindAll(stored *invoice) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(stored); err != nil {
			http.Error(w, err.Error(), 400)
		}
	}
}

// noteEdit is everything that this endpoint lets a caller change.
type noteEdit struct {
	Note *string `json:"note"`
}

// bindOne reads the request into a type that has only that field, and refuses a request
// that names any other.
func bindOne(stored *invoice) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var edit noteEdit
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&edit); err != nil || edit.Note == nil {
			http.Error(w, "this endpoint changes the note, and nothing else", 400)
			return
		}
		stored.Note = *edit.Note
	}
}

func part2() {
	const greedy = `{"note":"thanks","paid":true,"pence":1,"tenant":"B"}`
	const honest = `{"note":"thanks"}`
	fmt.Println("PART 2 — a member may edit the note on an invoice. One request also names paid, pence and tenant.")
	fmt.Println()
	fmt.Printf("  %-54s %-7s %-9s %-6s %7s  %s\n", "how the handler reads the body", "status", "note", "paid", "pence", "tenant")
	show := func(what, status string, inv invoice) {
		fmt.Printf("  %-54s %-7s %-9q %-6v %7s  %s\n", what, status, inv.Note, inv.Paid, commas(inv.Pence), inv.Tenant)
	}
	fresh := invoice{ID: "1001", Tenant: "A", Pence: 48000}
	show("before any request", "", fresh)
	try := func(what string, handler func(*invoice) http.HandlerFunc, body string) {
		inv := fresh
		srv := httptest.NewServer(handler(&inv))
		defer srv.Close()
		req, err := http.NewRequest("PATCH", srv.URL+"/invoices/1001", bytes.NewBufferString(body))
		check(err)
		resp, err := http.DefaultClient.Do(req)
		check(err)
		resp.Body.Close()
		show(what, fmt.Sprint(resp.StatusCode), inv)
	}
	try("straight into the stored invoice", bindAll, greedy)
	try("into a type that has the one field that may change", bindOne, greedy)
	try("the same handler, and a request with the note alone", bindOne, honest)
}
