// Package apikey identifies the caller of the management API.
// Phase 2 ships only the context plumbing and a static development
// middleware. Phase 3 adds hashed bearer keys backed by the api_keys table.
package apikey

import (
	"context"
	"net/http"
)

type ctxKey struct{}

// WithID returns a child context carrying the authenticated API key id.
func WithID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IDFromContext returns the API key id set by the auth middleware.
func IDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxKey{}).(int64)
	return id, ok && id > 0
}

// Static attaches a fixed API key id to every request. It exists so the API
// can be exercised locally before real authentication lands in phase 3 and
// must never be enabled outside APP_ENV=dev.
func Static(id int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithID(r.Context(), id)))
		})
	}
}
