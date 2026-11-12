// Part 3: the tenant rule as a row-level security policy in PostgreSQL.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// see runs, in one transaction and as one role, two queries that name no tenant.
func see(ctx context.Context, conn *pgx.Conn, role, tenant string) (rows int, found bool) {
	tx, err := conn.Begin(ctx)
	must(err)
	defer tx.Rollback(ctx)
	if role != "" {
		_, err = tx.Exec(ctx, "SET LOCAL ROLE "+role)
		must(err)
	}
	if tenant != "" {
		_, err = tx.Exec(ctx, "SELECT set_config('app.tenant', $1, true)", tenant) // for this transaction only
		must(err)
	}
	must(tx.QueryRow(ctx, "SELECT count(*) FROM az_invoices").Scan(&rows))
	must(tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM az_invoices WHERE id = 1001)").Scan(&found))
	return rows, found
}

func must(err error) {
	if err != nil {
		fmt.Println("stopped:", err)
		os.Exit(1)
	}
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
			must(err)
		}
	}
	run(`DROP TABLE IF EXISTS az_invoices`,
		`DROP ROLE IF EXISTS az_app`,
		`DROP ROLE IF EXISTS az_owner`,
		`CREATE ROLE az_app NOLOGIN`,   // the role that the application's queries run as
		`CREATE ROLE az_owner NOLOGIN`, // the role that runs the migrations, and so owns the table
		`CREATE TABLE az_invoices (id bigint PRIMARY KEY, tenant text NOT NULL, pence int NOT NULL)`)
	rows := [][]any{}
	for _, inv := range newStore(false).all() {
		id, _ := strconv.Atoi(inv.ID)
		rows = append(rows, []any{id, inv.Tenant, inv.Pence})
	}
	_, err = conn.CopyFrom(ctx, pgx.Identifier{"az_invoices"}, []string{"id", "tenant", "pence"}, pgx.CopyFromRows(rows))
	must(err)
	run(`ALTER TABLE az_invoices OWNER TO az_owner`,
		`GRANT SELECT ON az_invoices TO az_app`,
		`ALTER TABLE az_invoices ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY one_tenant ON az_invoices USING (tenant = current_setting('app.tenant', true))`)

	var me string
	var super bool
	must(conn.QueryRow(ctx, "SELECT session_user, rolsuper FROM pg_roles WHERE rolname = session_user").Scan(&me, &super))

	fmt.Println("PART 3 — the same rule in the database: PostgreSQL row-level security, on 2,000 invoices.")
	fmt.Println()
	fmt.Println("  The policy:                       USING (tenant = current_setting('app.tenant', true))")
	fmt.Println("  Two queries that name no tenant:  SELECT count(*) FROM az_invoices   and   … WHERE id = 1001")
	fmt.Println()
	fmt.Printf("  %-58s %10s %28s\n", "who runs them", "rows seen", "invoice 1001, which is A's")
	line := func(who string, rows int, found bool) {
		answer := "not found"
		if found {
			answer = "found"
		}
		fmt.Printf("  %-58s %10s %28s\n", who, commas(rows), answer)
	}
	n, found := see(ctx, conn, "az_app", "B")
	line("the application's role, with the tenant set to B", n, found)
	n, found = see(ctx, conn, "az_app", "A")
	line("the application's role, with the tenant set to A", n, found)
	n, found = see(ctx, conn, "az_app", "")
	line("the application's role, with no tenant set", n, found)
	n, found = see(ctx, conn, "az_owner", "B")
	line("the role that owns the table, with the tenant set to B", n, found)
	run(`ALTER TABLE az_invoices FORCE ROW LEVEL SECURITY`)
	n, found = see(ctx, conn, "az_owner", "B")
	line("the same, after FORCE ROW LEVEL SECURITY", n, found)
	if super {
		n, found = see(ctx, conn, "", "B")
		line(fmt.Sprintf("%s, a superuser, with the tenant set to B", me), n, found)
	}

	// A connection from a pool serves one request and then another. Request 1 is tenant A's.
	// Request 2 has a bug: it never sets a tenant.
	count := func() (n int) {
		must(conn.QueryRow(ctx, "SELECT count(*) FROM az_invoices").Scan(&n))
		return n
	}
	fmt.Println()
	fmt.Println("  One pooled connection, two requests. The first is tenant A's. The second forgets to set a tenant.")
	fmt.Println()
	fmt.Printf("  %-58s %10s %28s\n", "how the first request set its tenant", "it saw", "the second request saw")
	run(`SET ROLE az_app`)
	run(`SELECT set_config('app.tenant', 'A', false)`) // false: for the rest of the session
	first := count()
	fmt.Printf("  %-58s %10d %28d\n", "for the session:      set_config('app.tenant', 'A', false)", first, count())
	run(`RESET app.tenant`)
	tx, err := conn.Begin(ctx)
	must(err)
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant', 'A', true)") // true: until this transaction ends
	must(err)
	must(tx.QueryRow(ctx, "SELECT count(*) FROM az_invoices").Scan(&first))
	must(tx.Commit(ctx))
	fmt.Printf("  %-58s %10d %28d\n", "for its transaction:  set_config('app.tenant', 'A', true)", first, count())

	run(`RESET ROLE`, `DROP TABLE az_invoices`, `DROP ROLE az_app`, `DROP ROLE az_owner`)
}
