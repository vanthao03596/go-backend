// Package app is the composition root: it assembles services, handlers and
// middleware into a server. cmd/api and the integration tests both build the
// application through it so tests exercise the same wiring that ships.
package app

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/config"
	"github.com/vanthao03596/go-backend/internal/db/sqlc"
	"github.com/vanthao03596/go-backend/internal/link"
	"github.com/vanthao03596/go-backend/internal/server"
	"github.com/vanthao03596/go-backend/internal/server/middleware"
)

// Option customises the assembled application.
type Option func(*options)

type options struct {
	now func() time.Time
}

// WithClock replaces the clock used for link expiry. Production code never
// sets it; integration tests move it forward instead of sleeping.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// New wires every feature onto a server using pool for persistence. The
// returned server is not listening yet; call Run or use Handler with httptest.
func New(cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, opts ...Option) *server.Server {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	queries := sqlc.New(pool)
	keySvc := apikey.NewService(apikey.NewRepository(queries))

	var linkOpts []link.ServiceOption
	if o.now != nil {
		linkOpts = append(linkOpts, link.WithNow(o.now))
	}
	linkSvc := link.NewService(link.NewRepository(queries), cfg.BaseURL, log, linkOpts...)
	linkHandler := link.NewHandler(linkSvc, log,
		link.WithRedirectRateLimit(middleware.RateLimitByIP(cfg.RateLimit.RedirectPerSecond)))

	return server.New(cfg, log,
		server.WithReadiness("postgres", pool.Ping),
		server.WithFeature(linkHandler.Register),
		// Order matters: throttle anonymous bursts before paying for a key lookup.
		server.WithAPIMiddleware(
			link.CreateRateLimit(middleware.RateLimitByIP(cfg.RateLimit.CreatePerSecond)),
			apikey.Bearer(keySvc, log),
		),
	)
}
