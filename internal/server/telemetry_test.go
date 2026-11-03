package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/baggage"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/vanthao03596/go-backend/internal/config"
	"github.com/vanthao03596/go-backend/internal/platform/otelx/otelxtest"
)

const requestDuration = "http.server.request.duration"

// newTracedServer builds a server reporting to rec with a plain chi route at
// /items/{id} and a Huma operation at /api/v1/things/{id}.
func newTracedServer(t *testing.T, rec *otelxtest.Recorder, trustedProxies ...string) *Server {
	t.Helper()
	cfg := config.Config{
		Env: "test", RequestTimeout: time.Second, ShutdownTimeout: time.Second,
		TrustedProxies: trustedProxies,
	}
	feature := func(root chi.Router, api huma.API) {
		root.Get("/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		huma.Register(api, huma.Operation{
			OperationID: "get-thing", Method: http.MethodGet, Path: "/things/{id}",
		}, func(context.Context, *struct {
			ID string `path:"id"`
		}) (*struct{}, error) {
			return nil, nil //nolint:nilnil // Huma: empty 204
		})
	}
	return New(cfg, slog.New(slog.DiscardHandler),
		WithTelemetry(rec.TracerProvider, rec.MeterProvider),
		WithFeature(feature))
}

func serve(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestServerSpansAreNamedAfterTheRoute(t *testing.T) {
	rec := otelxtest.New(t)
	srv := newTracedServer(t, rec)

	cases := []struct {
		path, span, route string
	}{
		{"/items/42", "GET /items/{id}", "/items/{id}"},
		{"/api/v1/things/7", "GET /api/v1/things/{id}", "/api/v1/things/{id}"},
		// No route matched: the name stays low-cardinality.
		{"/no/such/route", "GET", ""},
	}
	for _, c := range cases {
		serve(srv, httptest.NewRequest(http.MethodGet, c.path, nil))
		spans := rec.SpansNamed(c.span)
		if len(spans) != 1 {
			t.Errorf("GET %s: %d spans named %q, want 1", c.path, len(spans), c.span)
			continue
		}
		s := spans[0]
		if s.SpanKind() != trace.SpanKindServer {
			t.Errorf("GET %s: span kind = %v", c.path, s.SpanKind())
		}
		if got := otelxtest.Attr(s, semconv.HTTPRouteKey).AsString(); got != c.route {
			t.Errorf("GET %s: http.route = %q, want %q", c.path, got, c.route)
		}
	}
}

func TestRequestMetricsCarryTheRoute(t *testing.T) {
	rec := otelxtest.New(t)
	srv := newTracedServer(t, rec)

	serve(srv, httptest.NewRequest(http.MethodGet, "/items/1", nil))
	serve(srv, httptest.NewRequest(http.MethodGet, "/items/2", nil))

	n, _ := rec.HistogramCount(t, requestDuration,
		semconv.HTTPRoute("/items/{id}"), semconv.HTTPResponseStatusCode(http.StatusNoContent))
	if n != 2 {
		t.Errorf("%s{http.route=/items/{id}} count = %d, want 2", requestDuration, n)
	}
}

func TestHealthProbesAreNotTraced(t *testing.T) {
	rec := otelxtest.New(t)
	srv := newTracedServer(t, rec)

	for _, path := range []string{"/healthz", "/readyz"} {
		if got := serve(srv, httptest.NewRequest(http.MethodGet, path, nil)).Code; got != http.StatusOK {
			t.Fatalf("GET %s = %d", path, got)
		}
	}
	if spans := rec.Spans(); len(spans) != 0 {
		t.Errorf("probes produced %d spans, first %q", len(spans), spans[0].Name())
	}
	if _, ok := rec.Metric(t, requestDuration); ok {
		t.Errorf("probes recorded %s", requestDuration)
	}
}

func TestIncomingTraceContextIsTrustedOnlyFromProxies(t *testing.T) {
	const (
		traceID = "0af7651916cd43dd8448eb211c80319c"
		spanID  = "b7ad6b7169203331"
	)
	request := func(remoteAddr string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/items/1", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
		return req
	}

	t.Run("trusted proxy continues the trace", func(t *testing.T) {
		rec := otelxtest.New(t)
		srv := newTracedServer(t, rec, "10.0.0.0/8")
		serve(srv, request("10.1.2.3:5555"))

		s := rec.SpansNamed("GET /items/{id}")[0]
		if got := s.SpanContext().TraceID().String(); got != traceID {
			t.Errorf("trace id = %s, want the caller's %s", got, traceID)
		}
		if got := s.Parent().SpanID().String(); got != spanID {
			t.Errorf("parent = %s, want the caller's span %s", got, spanID)
		}
	})

	t.Run("anyone else starts a new trace linked to theirs", func(t *testing.T) {
		rec := otelxtest.New(t)
		srv := newTracedServer(t, rec, "10.0.0.0/8")
		serve(srv, request("203.0.113.9:5555"))

		s := rec.SpansNamed("GET /items/{id}")[0]
		if got := s.SpanContext().TraceID().String(); got == traceID {
			t.Error("an untrusted caller chose the trace id")
		}
		if s.Parent().IsValid() {
			t.Errorf("span has parent %s, want a root", s.Parent().SpanID())
		}
		links := s.Links()
		if len(links) != 1 || links[0].SpanContext.TraceID().String() != traceID {
			t.Errorf("links = %+v, want one to the caller's trace", links)
		}
	})

	t.Run("no trusted proxies means nobody is trusted", func(t *testing.T) {
		rec := otelxtest.New(t)
		srv := newTracedServer(t, rec)
		serve(srv, request("10.1.2.3:5555"))

		if s := rec.SpansNamed("GET /items/{id}")[0]; s.Parent().IsValid() {
			t.Error("trace context accepted with TRUSTED_PROXIES empty")
		}
	})
}

// Baggage is never extracted, not even from a trusted proxy: the service has
// no use for it, so caller-controlled key-values must not reach the context.
func TestIncomingBaggageIsIgnored(t *testing.T) {
	rec := otelxtest.New(t)
	var members int
	cfg := config.Config{
		Env: "test", RequestTimeout: time.Second, ShutdownTimeout: time.Second,
		TrustedProxies: []string{"10.0.0.0/8"},
	}
	srv := New(cfg, slog.New(slog.DiscardHandler),
		WithTelemetry(rec.TracerProvider, rec.MeterProvider),
		WithFeature(func(root chi.Router, _ huma.API) {
			root.Get("/bag", func(w http.ResponseWriter, r *http.Request) {
				members = baggage.FromContext(r.Context()).Len()
				w.WriteHeader(http.StatusNoContent)
			})
		}))

	req := httptest.NewRequest(http.MethodGet, "/bag", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	req.Header.Set("Baggage", "tenant=admin,role=root")
	serve(srv, req)

	if members != 0 {
		t.Errorf("handler saw %d baggage members, want 0", members)
	}
	if s := rec.SpansNamed("GET /bag"); len(s) != 1 || !s[0].Parent().IsValid() {
		t.Error("trace context from the trusted proxy should still be continued")
	}
}
