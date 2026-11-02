package middleware

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/httprate"
)

// errNoClientIP is returned by the key function when ClientIP failed closed.
var errNoClientIP = errors.New("client ip could not be resolved")

// RateLimitByIP allows requestsPerSecond per client IP over a sliding one
// second window and answers 429 with problem details beyond that. It needs
// ClientIP earlier in the chain. A request whose IP could not be resolved
// (for example a malformed X-Forwarded-For chain behind a trusted proxy) is
// rejected with 400 rather than pooled into a shared bucket that one bad
// actor could exhaust for everyone else.
func RateLimitByIP(requestsPerSecond int) func(http.Handler) http.Handler {
	return httprate.LimitBy(requestsPerSecond, time.Second, clientIPKey,
		httprate.WithLimitHandler(func(w http.ResponseWriter, _ *http.Request) {
			WriteProblem(w, http.StatusTooManyRequests, "rate limit exceeded, retry later")
		}),
		httprate.WithErrorHandler(func(w http.ResponseWriter, _ *http.Request, _ error) {
			WriteProblem(w, http.StatusBadRequest, "client address could not be determined")
		}),
	)
}

// clientIPKey buckets IPv6 callers by /64 so a single host cannot rotate
// through its own prefix to escape the limit.
func clientIPKey(r *http.Request) (string, error) {
	ip := ClientIPFrom(r)
	if ip == "" {
		return "", errNoClientIP
	}
	return httprate.CanonicalizeIP(ip), nil
}
