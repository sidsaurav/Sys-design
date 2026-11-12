package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// counter counts the bytes that a logger writes.
type counter struct{ n int }

func (c *counter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

// handle is one call through one service, with the logging that the invoicing code has in it.
func handle(log *slog.Logger, id string) {
	ctx := context.Background()
	log.DebugContext(ctx, "request", "id", id, "method", "GET", "path", "/invoices/"+id, "agent", "invoicing-web/4.12 (+https://example.com)", "accept", "application/json", "authorization", "[not logged]")
	log.DebugContext(ctx, "session", "id", id, "tenant", "t-000417", "user", "u-0091345", "role", "accountant")
	log.DebugContext(ctx, "cache", "id", id, "key", "invoice:t-000417:"+id, "hit", false)
	log.DebugContext(ctx, "sql", "id", id, "ms", 3, "statement", "SELECT id, customer_id, amount_pence, currency, status, due_on, note FROM invoices WHERE tenant_id = $1 AND id = $2")
	log.DebugContext(ctx, "sql", "id", id, "ms", 4, "rows", 20, "statement", "SELECT id, description, quantity, unit_pence, tax_rate FROM invoice_lines WHERE invoice_id = $1 ORDER BY position")
	log.DebugContext(ctx, "call", "id", id, "service", "customers", "path", "/customers/c-0031877", "ms", 6, "status", 200)
	log.DebugContext(ctx, "call", "id", id, "service", "payments", "path", "/payments?invoice="+id, "ms", 9, "status", 200)
	log.DebugContext(ctx, "pricing", "id", id, "lines", 20, "net_pence", 480000, "tax_pence", 96000, "rounding", "half-even")
	log.DebugContext(ctx, "cache", "id", id, "key", "invoice:t-000417:"+id, "stored", true, "ttl_s", 300)
	log.DebugContext(ctx, "answer", "id", id, "body", strings.Repeat(`{"description":"Consulting, March","quantity":8,"unit_pence":60000},`, 20))
	log.InfoContext(ctx, "request served", "id", id, "method", "GET", "path", "/invoices/"+id, "tenant", "t-000417", "status", 200, "ms", 31, "bytes", 1840)
}

// fixedTime writes every timestamp at the same width, so that the count does not depend on the
// moment at which the program is run.
func fixedTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		a.Value = slog.StringValue(a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	return a
}

// measure runs a thousand calls at each level, and returns the bytes that one call writes at
// INFO, and the bytes more that it writes at DEBUG.
func measure() (info, more float64) {
	per := func(level slog.Level) float64 {
		var c counter
		log := slog.New(slog.NewJSONHandler(io.Writer(&c), &slog.HandlerOptions{Level: level, ReplaceAttr: fixedTime}))
		for i := 0; i < 1000; i++ {
			handle(log, fmt.Sprintf("inv-%07d", i))
		}
		return float64(c.n) / 1000
	}
	info = per(slog.LevelInfo)
	return info, per(slog.LevelDebug) - info
}
