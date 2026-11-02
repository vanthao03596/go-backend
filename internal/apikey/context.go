package apikey

import "context"

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
