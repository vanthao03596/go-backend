package apikey

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeAuth struct {
	valid string
	err   error
}

func (f fakeAuth) Authenticate(_ context.Context, raw string) (Key, error) {
	if f.err != nil {
		return Key{}, f.err
	}
	if raw == f.valid {
		return Key{ID: 7, Name: "ok"}, nil
	}
	return Key{}, ErrNotFound
}

func newBearerHandler(auth Authenticator) (http.Handler, *int64) {
	var seen int64
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = IDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	return Bearer(auth, slog.New(slog.DiscardHandler))(next), &seen
}

func TestBearerAcceptsValidKey(t *testing.T) {
	h, seen := newBearerHandler(fakeAuth{valid: "sk_good"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer sk_good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if *seen != 7 {
		t.Errorf("context key id = %d, want 7", *seen)
	}
}

func TestBearerRejects(t *testing.T) {
	h, seen := newBearerHandler(fakeAuth{valid: "sk_good"})
	cases := map[string]string{
		"missing header": "",
		"wrong scheme":   "Basic c2tfZ29vZA==",
		"empty token":    "Bearer   ",
		"unknown key":    "Bearer sk_other",
		"no space":       "Bearersk_good",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			*seen = 0
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q", ct)
			}
			if !strings.Contains(rec.Body.String(), `"status":401`) {
				t.Errorf("body = %s", rec.Body.String())
			}
			if *seen != 0 {
				t.Error("next handler ran for a rejected request")
			}
		})
	}
}

func TestBearerMasksBackendErrors(t *testing.T) {
	h, _ := newBearerHandler(fakeAuth{err: errors.New("SQLSTATE 57P01 terminating connection")})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer sk_whatever")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SQLSTATE") {
		t.Errorf("backend error leaked: %s", rec.Body.String())
	}
}
