// Package server owns the http.Server, routing and lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/vanthao03596/go-backend/internal/config"
)

// ReadinessCheck returns nil when a dependency is ready to serve traffic.
type ReadinessCheck func(ctx context.Context) error

// Feature registers a bounded context's routes. root is the top-level router
// (for things like redirects); api is the Huma instance mounted at /api/v1.
type Feature func(root chi.Router, api huma.API)

// Worker is a long-running background task started by Run alongside the
// listener. It must return promptly once ctx is cancelled.
type Worker func(ctx context.Context) error

// ShutdownHook runs after the listener has drained, so no handler is still
// producing work, and before Run returns. Use it to flush buffers.
type ShutdownHook func(ctx context.Context) error

type namedWorker struct {
	name string
	run  Worker
}

type namedHook struct {
	name string
	run  ShutdownHook
}

// Server wraps http.Server with the application's dependencies.
type Server struct {
	cfg           config.Config
	log           *slog.Logger
	http          *http.Server
	readiness     map[string]ReadinessCheck
	features      []Feature
	apiMiddleware []func(http.Handler) http.Handler
	workers       []namedWorker
	hooks         []namedHook

	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider

	// Set by Run; nil when the server was only used through Handler.
	stopWorkers context.CancelFunc
	workersDone chan struct{}
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

// WithWorker runs w for as long as the server serves. A worker that returns
// early is logged; it does not stop the server.
func WithWorker(name string, w Worker) Option {
	return func(s *Server) { s.workers = append(s.workers, namedWorker{name: name, run: w}) }
}

// WithShutdownHook registers h to run during Shutdown, after the listener
// has drained and in registration order.
func WithShutdownHook(name string, h ShutdownHook) Option {
	return func(s *Server) { s.hooks = append(s.hooks, namedHook{name: name, run: h}) }
}

// WithTelemetry sets where request spans and HTTP metrics go. Without it the
// otel globals are used, which are no-ops until telemetry is set up.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(s *Server) {
		s.tracerProvider = tp
		s.meterProvider = mp
	}
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
	if s.tracerProvider == nil {
		s.tracerProvider = otel.GetTracerProvider()
	}
	if s.meterProvider == nil {
		s.meterProvider = otel.GetMeterProvider()
	}
	s.http = &http.Server{
		Addr:              cfg.Addr(),
		Handler:           s.instrument(s.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// instrument wraps the router with otelhttp, which opens the server span and
// records the semantic-convention HTTP metrics (request duration and body
// sizes). Health probes are neither traced nor measured: they are frequent
// and would drown the traffic worth looking at.
//
// Incoming W3C trace context is only continued when the TCP peer is one of
// TRUSTED_PROXIES. Every other request starts a new trace that links to the
// caller's, so an internet client can neither force its requests to be
// sampled nor graft spans onto someone else's trace. Only the traceparent
// and tracestate headers are read: the service has no use for W3C baggage,
// and extracting it would put caller-controlled key-values into every
// request context whatever the peer.
func (s *Server) instrument(h http.Handler) http.Handler {
	trusted := make([]netip.Prefix, 0, len(s.cfg.TrustedProxies))
	for _, cidr := range s.cfg.TrustedProxies {
		// Config validation already rejected malformed entries.
		if p, err := netip.ParsePrefix(cidr); err == nil {
			trusted = append(trusted, p)
		}
	}
	return otelhttp.NewHandler(h, "http.server",
		otelhttp.WithTracerProvider(s.tracerProvider),
		otelhttp.WithMeterProvider(s.meterProvider),
		otelhttp.WithPropagators(propagation.TraceContext{}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}),
		otelhttp.WithPublicEndpointFn(func(r *http.Request) bool {
			return !peerIn(r.RemoteAddr, trusted)
		}),
	)
}

// peerIn reports whether the host part of remoteAddr lies in one of prefixes.
func peerIn(remoteAddr string, prefixes []netip.Prefix) bool {
	if len(prefixes) == 0 {
		return false
	}
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	addr := ap.Addr().Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
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
	s.startWorkers(ctx)

	var listenErr error
	select {
	case listenErr = <-errCh:
		if listenErr == nil {
			// ListenAndServe only returns nil after Shutdown; nobody else
			// calls it, so this is unreachable in practice but keeps the
			// select honest.
			return nil
		}
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	err := s.Shutdown(shutdownCtx)
	if listenErr != nil {
		return errors.Join(listenErr, err)
	}
	if err != nil {
		return err
	}
	return <-errCh
}

// Shutdown stops the listener and cancels workers, bounded by ctx, then
// runs the shutdown hooks in order under a fresh cfg.ShutdownTimeout. The
// hooks get their own budget so a slow client draining the listener cannot
// starve the click buffer flush; the worst case is therefore twice the
// timeout. Run calls it; tests that only used Handler call it to release
// the workers and hooks they wired. Calling it twice re-runs the hooks, so
// call it once.
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []error
	if err := s.http.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown: %w", err))
	}
	if s.stopWorkers != nil {
		s.stopWorkers()
		select {
		case <-s.workersDone:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("workers did not stop: %w", ctx.Err()))
		}
	}
	if len(s.hooks) > 0 {
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		defer cancel()
		for _, h := range s.hooks {
			if err := h.run(hookCtx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown hook %s: %w", h.name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (s *Server) startWorkers(ctx context.Context) {
	if len(s.workers) == 0 {
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	s.stopWorkers = cancel
	s.workersDone = make(chan struct{})

	var wg sync.WaitGroup
	for _, w := range s.workers {
		wg.Go(func() {
			s.log.Info("worker started", "worker", w.name)
			err := w.run(workerCtx)
			switch {
			case err != nil && !errors.Is(err, context.Canceled):
				s.log.Error("worker stopped with error", "worker", w.name, "err", err)
			case workerCtx.Err() == nil:
				s.log.Warn("worker stopped early", "worker", w.name)
			default:
				s.log.Info("worker stopped", "worker", w.name)
			}
		})
	}
	go func() {
		wg.Wait()
		close(s.workersDone)
	}()
}
