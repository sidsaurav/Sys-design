// Package review holds the handler of Chapter 8.5's exercise. It is here to be read and
// not to be run: export has six faults, and exportFixed is the worked solution.
package review

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

// settings are one tenant's preferences for its exports.
type settings struct {
	Tenant  string `json:"tenant"`
	Plan    string `json:"plan"` // "free" or "paid": what the tenant is billed for
	LogoURL string `json:"logo_url"`
	Footer  string `json:"footer"`
}

// export saves the caller's settings, then makes a zip of the tenant's invoices with the
// tenant's logo on the cover, and sends it back.
func export(db *sql.DB, stored *settings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tenant := q.Get("tenant")
		json.NewDecoder(r.Body).Decode(stored)
		rows, err := db.Query(fmt.Sprintf(
			"SELECT id FROM invoices WHERE tenant = '%s' AND status = '%s' ORDER BY %s",
			tenant, q.Get("status"), q.Get("sort")))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		logo, err := http.Get(stored.LogoURL)
		if err == nil {
			defer logo.Body.Close()
			io.Copy(w, logo.Body)
		}
		name := q.Get("name")
		exec.Command("sh", "-c", "zip -r /var/exports/"+name+".zip /var/invoices/"+tenant).Run()
		data, _ := os.ReadFile("/var/exports/" + name + ".zip")
		w.Write(data)
	}
}

// What exportFixed is given by the rest of the service: the tenant of the session
// (Chapter 8.3), a fetcher that refuses internal addresses when it connects (part 1 of this
// chapter's example), and a name that the caller had no part in choosing.
type deps struct {
	tenantOf   func(*http.Request) (string, bool)
	fetch      func(url string) ([]byte, error)
	randomName func() string
	addCover   func(zip string, logo []byte)
}

// sortable is every order that a caller may ask for.
var sortable = map[string]string{"date": "id", "amount": "pence"}

func exportFixed(db *sql.DB, stored *settings, d deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tenant, ok := d.tenantOf(r) // 1: the tenant comes from the session
		if !ok {
			http.Error(w, "sign in", 401)
			return
		}
		var edit struct { // 2: the one setting that this endpoint changes
			Footer *string `json:"footer"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&edit); err != nil {
			http.Error(w, "this endpoint changes the footer, and nothing else", 400)
			return
		}
		if edit.Footer != nil {
			stored.Footer = *edit.Footer
		}
		column, ok := sortable[q.Get("sort")] // 3: a column is chosen from a list…
		if !ok {
			http.Error(w, "sort by date or amount", 400)
			return
		}
		rows, err := db.Query( // …and a value is sent as a parameter
			"SELECT id FROM invoices WHERE tenant = $1 AND status = $2 ORDER BY "+column,
			tenant, q.Get("status"))
		if err != nil {
			http.Error(w, "the export failed", 500) // and the database's own words stay in the log
			return
		}
		defer rows.Close()
		out := filepath.Join("/var/exports", d.randomName()+".zip")    // 5 and 6: the server names the file
		zip := exec.Command("zip", "-r", out, "/var/invoices/"+tenant) // 5: no shell, and arguments as a list
		if err := zip.Run(); err != nil {
			http.Error(w, "the export failed", 500)
			return
		}
		if logo, err := d.fetch(stored.LogoURL); err == nil { // 4: the fetcher that checks every connection
			d.addCover(out, logo) // the logo goes into the file, and is never sent back as it came
		}
		http.ServeFile(w, r, out)
	}
}
