package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thaopv/go-backend/internal/config"
	"github.com/thaopv/go-backend/internal/server/middleware"
)

func newTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	cfg := config.Config{Env: "test", Port: 0, RequestTimeout: time.Second, ShutdownTimeout: time.Second}
	log := slog.New(slog.DiscardHandler)
	return New(cfg, log, opts...)
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(middleware.HeaderRequestID); got == "" {
		t.Error("missing X-Request-Id header")
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestReadyzReportsFailingDependency(t *testing.T) {
	srv := newTestServer(t, WithReadiness("db", func(context.Context) error {
		return errors.New("down")
	}))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "db unavailable") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestRequestIDIsPropagated(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(middleware.HeaderRequestID, "trace-42")
	srv.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get(middleware.HeaderRequestID); got != "trace-42" {
		t.Errorf("X-Request-Id = %q, want trace-42", got)
	}
}
