// Package app is the composition root: it assembles services, handlers and
// middleware into a server. cmd/api and the integration tests both build the
// application through it so tests exercise the same wiring that ships.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/click"
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

// WithClock replaces the clock used for link expiry, click timestamps and the
// stats window. Production code never sets it; integration tests move it
// forward instead of sleeping.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// New wires every feature onto a server using pool for persistence. The
// returned server is not listening yet; call Run, or use Handler with
// httptest and Shutdown when done so the click buffer is drained.
func New(cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, opts ...Option) *server.Server {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	queries := sqlc.New(pool)
	keySvc := apikey.NewService(apikey.NewRepository(queries))

	clickRepo := click.NewRepository(pool, queries)
	clickBuf := click.NewBuffer(clickRepo, log, click.BufferConfig{
		Size:          cfg.Clicks.BufferSize,
		FlushInterval: cfg.Clicks.FlushInterval,
		MaxBatch:      cfg.Clicks.FlushBatch,
	})

	var linkOpts []link.ServiceOption
	var clickOpts []click.ServiceOption
	if o.now != nil {
		linkOpts = append(linkOpts, link.WithNow(o.now))
		clickOpts = append(clickOpts, click.WithNow(o.now))
	}
	linkOpts = append(linkOpts, link.WithClickSink(clickBuf))
	linkSvc := link.NewService(link.NewRepository(queries), cfg.BaseURL, log, linkOpts...)
	linkHandler := link.NewHandler(linkSvc, log,
		link.WithRedirectRateLimit(middleware.RateLimitByIP(cfg.RateLimit.RedirectPerSecond)))

	clickSvc := click.NewService(clickRepo, log, clickOpts...)
	clickHandler := click.NewHandler(clickSvc, linkSvc, log)

	srvOpts := []server.Option{
		server.WithReadiness("postgres", pool.Ping),
		server.WithFeature(linkHandler.Register),
		server.WithFeature(clickHandler.Register),
		// Order matters: throttle anonymous bursts before paying for a key lookup.
		server.WithAPIMiddleware(
			link.CreateRateLimit(middleware.RateLimitByIP(cfg.RateLimit.CreatePerSecond)),
			apikey.Bearer(keySvc, log),
		),
		// Runs after the listener drains, so every redirect that answered
		// 302 has already enqueued its click.
		server.WithShutdownHook("click buffer", clickBuf.Close),
	}
	if cfg.Clicks.PurgeInterval > 0 {
		srvOpts = append(srvOpts, server.WithWorker("click purger", func(ctx context.Context) error {
			return clickSvc.RunPurger(ctx, cfg.Clicks.Retention, cfg.Clicks.PurgeInterval)
		}))
	}
	return server.New(cfg, log, srvOpts...)
}
