package middleware

import (
	"context"
	"net/http"
	"time"
)

// Timeout attaches a deadline to the request context. Handlers and database
// calls that honour the context stop when the deadline passes.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
