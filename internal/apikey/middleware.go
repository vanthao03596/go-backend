package apikey

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/thaopv/go-backend/internal/server/middleware"
)

// Authenticator resolves a raw bearer token. *Service satisfies it; tests
// substitute a fake.
type Authenticator interface {
	Authenticate(ctx context.Context, raw string) (Key, error)
}

// Bearer authenticates requests with "Authorization: Bearer <key>" and stores
// the key id in the request context for handlers to scope their queries.
// Missing, malformed, unknown and revoked keys all yield the same 401 so the
// response does not reveal which keys exist. The raw key is never logged.
func Bearer(auth Authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w, "missing bearer token")
				return
			}
			k, err := auth.Authenticate(r.Context(), raw)
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(WithID(r.Context(), k.ID)))
			case errors.Is(err, ErrNotFound):
				unauthorized(w, "invalid or revoked API key")
			default:
				log.ErrorContext(r.Context(), "authenticate api key",
					"request_id", middleware.RequestIDFrom(r.Context()), "err", err)
				middleware.WriteProblem(w, http.StatusInternalServerError, "internal error")
			}
		})
	}
}

// bearerToken extracts the credential from an Authorization header value.
// The scheme is matched case-insensitively per RFC 9110.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
	middleware.WriteProblem(w, http.StatusUnauthorized, detail)
}
