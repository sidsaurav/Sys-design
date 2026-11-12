// Chapter 7.4 — the same idea with OpenTelemetry: one call between two services, traced.
//
// This directory is a module of its own, so that the rest of the course does not
// depend on OpenTelemetry. It needs Go 1.23 or later, and the network the first
// time it runs, to fetch the modules.
//
//	cd phase-7-observability/code/ch04-otel && go run .
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func main() {
	// Once, when the program starts: where finished spans go, and how the context crosses the network.
	exporter := tracetest.NewInMemoryExporter() // in production: an OTLP exporter, sending to a collector
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{}) // the W3C traceparent header
	tracer := otel.Tracer("checkout")

	// The service that is called. The wrapper reads traceparent and starts a span for each request.
	var header string
	pricing := httptest.NewServer(otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("traceparent")
		_, span := tracer.Start(r.Context(), "apply discounts") // a span for work inside the service
		defer span.End()
	}), "GET /price"))
	defer pricing.Close()

	// The service that calls. The wrapped transport starts a span for each call and writes traceparent.
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}

	ctx, root := tracer.Start(context.Background(), "POST /checkout")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pricing.URL+"/price", nil) // the context goes with the request
	if err != nil {
		panic(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	root.End()

	fmt.Println("the header that the called service received:")
	fmt.Println("  traceparent:", header)
	fmt.Println()
	spans := exporter.GetSpans()
	sort.Slice(spans, func(i, j int) bool { return spans[i].StartTime.Before(spans[j].StartTime) })
	fmt.Printf("  %-18s %-34s %-18s %s\n", "span", "trace", "span ID", "parent")
	for _, s := range spans {
		fmt.Printf("  %-18s %-34s %-18s %s\n", s.Name, s.SpanContext.TraceID(), s.SpanContext.SpanID(), s.Parent.SpanID())
	}
}
