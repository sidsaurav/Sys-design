// Part 3: a search box, a query built from what was typed into it, and a database.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

type found struct {
	id               int
	tenant, customer string
}

// joined builds the query by putting the caller's text inside it.
func joined(ctx context.Context, q pgx.Tx, tenant, typed string) ([]found, error) {
	sql := "SELECT id, tenant, customer FROM vn_invoices WHERE tenant = '" + tenant +
		"' AND customer LIKE '%" + typed + "%' ORDER BY id"
	return collect(q.Query(ctx, sql))
}

// bound sends the query and the caller's text separately. The text can only ever be a value.
func bound(ctx context.Context, q pgx.Tx, tenant, typed string) ([]found, error) {
	sql := "SELECT id, tenant, customer FROM vn_invoices WHERE tenant = $1 AND customer LIKE '%' || $2 || '%' ORDER BY id"
	return collect(q.Query(ctx, sql, tenant, typed))
}

func collect(rows pgx.Rows, err error) ([]found, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (f found, err error) {
		err = r.Scan(&f.id, &f.tenant, &f.customer)
		return f, err
	})
}

// sortable is every way that a caller may ask for the list to be ordered. A column's name
// cannot be sent as a parameter, so the caller's word chooses from this map and is never
// itself put into the query.
var sortable = map[string]string{"date": "id", "amount": "pence", "customer": "customer"}

func orderBy(asked string) (string, error) {
	column, ok := sortable[asked]
	if !ok {
		return "", errors.New("refused: not one of date, amount, customer")
	}
	return " ORDER BY " + column, nil
}

// describe says what came back, in terms of whose it is.
func describe(rows []found, err error) string {
	if err != nil {
		msg := err.Error()
		if i := strings.Index(msg, "permission denied"); i >= 0 {
			return "an error: " + strings.TrimSuffix(msg[i:], " (SQLSTATE 42501)")
		}
		return "an error: " + msg
	}
	tenants, users := map[string]bool{}, 0
	for _, r := range rows {
		if r.id == 0 { // not an invoice: a row that the UNION brought in from the users table
			users++
			continue
		}
		tenants[r.tenant] = true
	}
	invoices := len(rows) - users
	what := fmt.Sprintf("%s rows, of %d tenants", commas(invoices), len(tenants))
	if len(tenants) == 1 {
		what = fmt.Sprintf("%s rows, all of tenant %s", commas(invoices), rows[len(rows)-1-users].tenant)
	}
	switch {
	case len(rows) == 0:
		return "0 rows"
	case users > 0:
		return fmt.Sprintf("%s rows of invoices, and %d users with password hashes", commas(invoices), users)
	}
	return what
}

func part3() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres"
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		fmt.Println("PART 3 — skipped: no PostgreSQL to connect to. See phase-2-data/README.md.")
		return
	}
	defer conn.Close(ctx)
	run := func(statements ...string) {
		for _, q := range statements {
			_, err := conn.Exec(ctx, q)
			check(err)
		}
	}
	run(`DROP TABLE IF EXISTS vn_invoices, vn_users`,
		`DROP ROLE IF EXISTS vn_search`,
		`CREATE TABLE vn_invoices (id int PRIMARY KEY, tenant text NOT NULL, customer text NOT NULL, pence int NOT NULL)`,
		`INSERT INTO vn_invoices SELECT 1001 + k,
		   CASE k % 50 WHEN 0 THEN 'A' WHEN 1 THEN 'B' ELSE 'T' || lpad((k % 50 + 1)::text, 2, '0') END,
		   (ARRAY['Alder Ltd', 'Birch & Co', 'Cedar Works', 'Dogwood LLP', 'Elm Street Bakery'])[k / 50 % 5 + 1],
		   1000 + (37 * k) % 9000
		 FROM generate_series(0, 1999) k`,
		`CREATE TABLE vn_users (email text PRIMARY KEY, password_hash text NOT NULL)`,
		`INSERT INTO vn_users VALUES ('ada@a.example', '$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA'),
		   ('bo@b.example', '$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaB'), ('cy@t03.example', '$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaC')`,
		// A role for the search alone: it may read invoices, it sees one tenant's rows, and it
		// has no right to the users table (Chapters 8.3 and 8.6).
		`CREATE ROLE vn_search NOLOGIN`,
		`GRANT SELECT ON vn_invoices TO vn_search`,
		`ALTER TABLE vn_invoices ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY one_tenant ON vn_invoices USING (tenant = current_setting('app.tenant', true))`)

	// as runs one search in a transaction of its own, as the connection's own user or as the limited role.
	as := func(limited bool, search func(context.Context, pgx.Tx, string, string) ([]found, error), typed string) string {
		tx, err := conn.Begin(ctx)
		check(err)
		defer tx.Rollback(ctx)
		if limited {
			_, err = tx.Exec(ctx, "SET LOCAL ROLE vn_search")
			check(err)
			_, err = tx.Exec(ctx, "SELECT set_config('app.tenant', 'B', true)")
			check(err)
		}
		return describe(search(ctx, tx, "B", typed))
	}

	fmt.Println("PART 3 — a search box. The caller is a member of tenant B, and the search should stay inside B.")
	for _, typed := range []string{
		"Birch",
		"' OR 1=1 --",
		"' UNION SELECT 0, email, password_hash FROM vn_users --",
	} {
		fmt.Println()
		fmt.Printf("  typed into the box:  %s\n", typed)
		fmt.Printf("    %-44s %s\n", "joined into the query", as(false, joined, typed))
		fmt.Printf("    %-44s %s\n", "sent as a parameter", as(false, bound, typed))
		fmt.Printf("    %-44s %s\n", "joined, by a role limited to B's invoices", as(true, joined, typed))
	}
	fmt.Println()
	fmt.Println("  \"Sort by\" cannot be a parameter, so the caller's word chooses from a list:")
	for _, asked := range []string{"amount", "pence; DROP TABLE vn_invoices"} {
		clause, err := orderBy(asked)
		if err != nil {
			clause = err.Error()
		}
		fmt.Printf("    %-34q %s\n", asked, strings.TrimSpace(clause))
	}
	run(`DROP TABLE vn_invoices, vn_users`, `DROP ROLE vn_search`)
}
