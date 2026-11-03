//go:build integration

package integration

import (
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/vanthao03596/go-backend/internal/app"
	"github.com/vanthao03596/go-backend/internal/db"
	"github.com/vanthao03596/go-backend/internal/platform/otelx/otelxtest"
)

// descendsFrom reports whether s is below ancestor in the span tree spans.
func descendsFrom(s, ancestor sdktrace.ReadOnlySpan, spans []sdktrace.ReadOnlySpan) bool {
	byID := make(map[trace.SpanID]sdktrace.ReadOnlySpan, len(spans))
	for _, sp := range spans {
		byID[sp.SpanContext().SpanID()] = sp
	}
	for cur := s; cur.Parent().IsValid(); {
		if cur.Parent().SpanID() == ancestor.SpanContext().SpanID() {
			return true
		}
		next, ok := byID[cur.Parent().SpanID()]
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

// dbSpansUnder returns the Postgres client spans below parent.
func dbSpansUnder(parent sdktrace.ReadOnlySpan, spans []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.SpanKind() == trace.SpanKindClient &&
			otelxtest.Attr(s, semconv.DBSystemNameKey) == semconv.DBSystemNamePostgreSQL.Value &&
			descendsFrom(s, parent, spans) {
			out = append(out, s)
		}
	}
	return out
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

// TestRedirectIsTraced is the phase 6 acceptance check in code: a redirect is
// one trace holding the HTTP server span and the database spans it caused,
// and the click it counts is flushed under a traced job of its own.
func TestRedirectIsTraced(t *testing.T) {
	t.Parallel()
	rec := otelxtest.New(t)
	tracedPool, err := db.NewPool(t.Context(), dsn, db.PoolOptions{
		MaxConns:       4,
		TracerProvider: rec.TracerProvider,
		MeterProvider:  rec.MeterProvider,
	})
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(tracedPool.Close)
	e := newEnvWith(t, tracedPool, nil, app.WithTelemetry(rec.TracerProvider, rec.MeterProvider))

	key := e.newKey("telemetry")
	l := e.create(key, createRequest{URL: "https://example.com/traced"})
	r := e.send(e.request(http.MethodGet, "/"+l.Code, ""))
	e.expectStatus(r, http.StatusFound, "redirect")

	servers := rec.SpansNamed("GET /{code}")
	if len(servers) != 1 {
		t.Fatalf("server spans named GET /{code}: %d, all spans: %v", len(servers), names(rec.Spans()))
	}
	server := servers[0]
	if got := otelxtest.Attr(server, semconv.HTTPRouteKey).AsString(); got != "/{code}" {
		t.Errorf("http.route = %q", got)
	}
	if got := otelxtest.Attr(server, semconv.HTTPResponseStatusCodeKey).AsInt64(); got != http.StatusFound {
		t.Errorf("http.response.status_code = %d", got)
	}
	queries := dbSpansUnder(server, rec.Spans())
	if len(queries) == 0 {
		t.Fatalf("redirect has no database spans; spans: %v", names(rec.Spans()))
	}
	sawSelect := false
	for _, q := range queries {
		if q.Name() == "SELECT" && q.SpanContext().TraceID() == server.SpanContext().TraceID() {
			sawSelect = true
		}
	}
	if !sawSelect {
		t.Errorf("no SELECT span under the redirect; got %v", names(queries))
	}

	creates := rec.SpansNamed("POST /api/v1/links")
	if len(creates) != 1 || len(dbSpansUnder(creates[0], rec.Spans())) == 0 {
		t.Errorf("create link: %d spans, want 1 with database children", len(creates))
	}

	// The click reaches Postgres through the buffer's own traced flush.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("recorded spans: %q", names(rec.Spans()))
		}
	})
	e.waitFor("traced click flush", func() bool {
		for _, f := range rec.SpansNamed("click.flush") {
			for _, q := range dbSpansUnder(f, rec.Spans()) {
				// pgx quotes the table identifier.
				if q.Name() == `copy_from "clicks"` {
					return true
				}
			}
		}
		return false
	})

	if got := rec.Sum(t, "shortener.redirects", semconv.HTTPResponseStatusCode(http.StatusFound)); got != 1 {
		t.Errorf("shortener.redirects{302} = %d, want 1", got)
	}
	if got := rec.Sum(t, "shortener.links.created", attribute.Bool("shortener.link.custom_alias", false)); got != 1 {
		t.Errorf("shortener.links.created = %d, want 1", got)
	}
	if n, _ := rec.HistogramCount(t, "http.server.request.duration", semconv.HTTPRoute("/{code}")); n != 1 {
		t.Errorf("http.server.request.duration{/{code}} count = %d, want 1", n)
	}
	if n, _ := rec.HistogramCount(t, "db.client.operation.duration"); n == 0 {
		t.Error("no db.client.operation.duration measurements")
	}
	if n, sum := rec.HistogramCount(t, "shortener.clicks.flush.size"); n == 0 || sum != 1 {
		t.Errorf("shortener.clicks.flush.size = %d flushes / %d clicks, want >0 / 1", n, sum)
	}
}
