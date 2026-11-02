package middleware

import (
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// ClientIP resolves the caller's IP into the request context for logging and
// rate limiting. With no trusted proxies the TCP peer address is the client.
// With trusted proxy CIDRs, X-Forwarded-For is walked right to left and the
// first hop outside those ranges is the client; a peer outside the ranges is
// treated as the client directly, so the header cannot be spoofed by anyone
// who reaches the server without going through the proxy.
func ClientIP(trustedProxies []string) func(http.Handler) http.Handler {
	if len(trustedProxies) == 0 {
		return chimw.ClientIPFromRemoteAddr
	}
	return chimw.ClientIPFromXFF(trustedProxies...)
}

// ClientIPFrom returns the resolved client IP, or an empty string if the
// resolver failed closed (for example a malformed X-Forwarded-For chain).
func ClientIPFrom(r *http.Request) string {
	return chimw.GetClientIP(r.Context())
}
