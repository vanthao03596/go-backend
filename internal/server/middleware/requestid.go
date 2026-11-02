// Package middleware holds HTTP middleware shared by all routes.
package middleware

import (
	"context"
	"net/http"
	"uuid"
)

type ctxKey int

const requestIDKey ctxKey = iota

// HeaderRequestID is the header used to propagate and return the request id.
const HeaderRequestID = "X-Request-Id"

// RequestID reads X-Request-Id from the inbound request or generates a UUID,
// stores it in the context and echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" || len(id) > 128 {
			id = uuid.NewV7().String()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFrom returns the request id stored in ctx, or an empty string.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}
