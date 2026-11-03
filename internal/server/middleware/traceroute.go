package middleware

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// TraceRoute names the request's server span after the matched chi route
// ("GET /{code}") and records http.route on the span and on otelhttp's
// request metrics.
//
// otelhttp derives both from http.Request.Pattern after the handler returns,
// but chi sets Pattern on its own copy of the request, which otelhttp never
// sees. Without this middleware every span would be named after the method
// alone and the duration histogram would carry no route. Install it first on
// the root chi router, with otelhttp wrapping the router: it reads chi's
// routing context, which is complete once the handler has returned and is
// recycled as soon as the router's ServeHTTP exits.
//
// Requests that matched no route keep the method-only name, so unknown paths
// cannot inflate span-name or metric cardinality.
func TraceRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		rctx := chi.RouteContext(r.Context())
		if rctx == nil {
			return
		}
		pattern := rctx.RoutePattern()
		if pattern == "" {
			return
		}
		route := semconv.HTTPRoute(pattern)
		if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
			span.SetName(r.Method + " " + pattern)
			span.SetAttributes(route)
		}
		if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
			labeler.Add(route)
		}
	})
}
