// Package server owns the http.Server, routing and lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/thaopv/go-backend/internal/config"
)

// ReadinessCheck returns nil when a dependency is ready to serve traffic.
type ReadinessCheck func(ctx context.Context) error

// Server wraps http.Server with the application's dependencies.
type Server struct {
	cfg       config.Config
	log       *slog.Logger
	http      *http.Server
	readiness map[string]ReadinessCheck
}

// Option customises a Server at construction time.
type Option func(*Server)

// WithReadiness registers a named dependency check for GET /readyz.
func WithReadiness(name string, check ReadinessCheck) Option {
	return func(s *Server) { s.readiness[name] = check }
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
