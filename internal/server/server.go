// Package server owns the http.Server, routing and lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/vanthao03596/go-backend/internal/config"
)

// ReadinessCheck returns nil when a dependency is ready to serve traffic.
type ReadinessCheck func(ctx context.Context) error

// Feature registers a bounded context's routes. root is the top-level router
// (for things like redirects); api is the Huma instance mounted at /api/v1.
type Feature func(root chi.Router, api huma.API)

// Server wraps http.Server with the application's dependencies.
type Server struct {
	cfg           config.Config
	log           *slog.Logger
	http          *http.Server
	readiness     map[string]ReadinessCheck
	features      []Feature
	apiMiddleware []func(http.Handler) http.Handler
}

// Option customises a Server at construction time.
type Option func(*Server)

// WithReadiness registers a named dependency check for GET /readyz.
func WithReadiness(name string, check ReadinessCheck) Option {
	return func(s *Server) { s.readiness[name] = check }
}

// WithFeature mounts a feature's routes.
func WithFeature(f Feature) Option {
	return func(s *Server) { s.features = append(s.features, f) }
}

// WithAPIMiddleware adds middleware that runs only for /api/v1 routes,
// after the global stack. Authentication lives here.
func WithAPIMiddleware(mw ...func(http.Handler) http.Handler) Option {
	return func(s *Server) { s.apiMiddleware = append(s.apiMiddleware, mw...) }
}

// New builds a Server. It does not start listening.
func New(cfg config.Config, log *slog.Logger, opts ...Option) *Server {
	s := &Server{
		cfg:       cfg,
		log:       log,
		readiness: map[string]ReadinessCheck{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.http = &http.Server{
		Addr:              cfg.Addr(),
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// Handler exposes the root handler, mainly for httptest.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is cancelled, then shuts down gracefully within
// cfg.ShutdownTimeout. It returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", s.http.Addr, "env", s.cfg.Env)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return <-errCh
}
