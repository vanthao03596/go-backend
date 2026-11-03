// Package app is the composition root: it assembles services, handlers and
// middleware into a server. cmd/api and the integration tests both build the
// application through it so tests exercise the same wiring that ships.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/click"
	"github.com/vanthao03596/go-backend/internal/config"
	"github.com/vanthao03596/go-backend/internal/db/sqlc"
	"github.com/vanthao03596/go-backend/internal/link"
	"github.com/vanthao03596/go-backend/internal/platform/redisx"
	"github.com/vanthao03596/go-backend/internal/server"
	"github.com/vanthao03596/go-backend/internal/server/middleware"
)

// Option customises the assembled application.
type Option func(*options)

type options struct {
	now            func() time.Time
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	redis          redis.Cmdable
}

// WithRedis puts a Redis cache in front of redirect lookups, with the TTLs
// from cfg.Cache, behind a circuit breaker. Without it every redirect reads
// Postgres. The caller owns the client and closes it after the server has
// shut down.
func WithRedis(rdb redis.Cmdable) Option {
	return func(o *options) { o.redis = rdb }
}

// WithClock replaces the clock used for link expiry, click timestamps and the
// stats window. Production code never sets it; integration tests move it
// forward instead of sleeping.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithTelemetry sets the providers every component reports spans and metrics
// to. Without it the otel globals are used, which are no-ops unless
// telemetry was set up. The database pool is instrumented separately, by
// db.NewPool.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(o *options) {
		o.tracerProvider = tp
		o.meterProvider = mp
	}
}

// OpenAPI returns the management API description the server built from cfg
// serves. Nothing is connected or started: features only register their
// operations while the server is assembled.
func OpenAPI(cfg config.Config) *huma.OpenAPI {
	return New(cfg, slog.New(slog.DiscardHandler), nil).OpenAPI()
}

// New wires every feature onto a server using pool for persistence. The
// returned server is not listening yet; call Run, or use Handler with
// httptest and Shutdown when done so the click buffer is drained. A nil
// pool builds a server that can describe itself but not serve.
func New(cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, opts ...Option) *server.Server {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.tracerProvider == nil {
		o.tracerProvider = otel.GetTracerProvider()
	}
	if o.meterProvider == nil {
		o.meterProvider = otel.GetMeterProvider()
	}

	queries := sqlc.New(pool)
	keySvc := apikey.NewService(apikey.NewRepository(queries))

	clickRepo := click.NewRepository(pool, queries)
	clickBuf := click.NewBuffer(clickRepo, log, click.BufferConfig{
		Size:           cfg.Clicks.BufferSize,
		FlushInterval:  cfg.Clicks.FlushInterval,
		MaxBatch:       cfg.Clicks.FlushBatch,
		TracerProvider: o.tracerProvider,
		MeterProvider:  o.meterProvider,
	})

	linkOpts := []link.ServiceOption{link.WithMeterProvider(o.meterProvider)}
	clickOpts := []click.ServiceOption{click.WithTracerProvider(o.tracerProvider)}
	if o.now != nil {
		linkOpts = append(linkOpts, link.WithNow(o.now))
		clickOpts = append(clickOpts, click.WithNow(o.now))
	}
	linkOpts = append(linkOpts, link.WithClickSink(clickBuf))
	if cfg.CursorSecret != "" {
		linkOpts = append(linkOpts, link.WithCursorSecret([]byte(cfg.CursorSecret)))
	}
	if o.redis != nil {
		cache := link.GuardCache(
			link.NewRedisCache(o.redis, cfg.Cache.TTL, cfg.Cache.NegativeTTL),
			redisx.NewBreaker(redisx.BreakerConfig{}, log.With("component", "link cache")),
		)
		linkOpts = append(linkOpts, link.WithCache(cache))
	}
	linkSvc := link.NewService(link.NewRepository(queries), cfg.BaseURL, log, linkOpts...)
	linkHandler := link.NewHandler(linkSvc, log,
		link.WithRedirectRateLimit(middleware.RateLimitByIP(cfg.RateLimit.RedirectPerSecond)))

	clickSvc := click.NewService(clickRepo, log, clickOpts...)
	clickHandler := click.NewHandler(clickSvc, linkSvc, log)

	srvOpts := []server.Option{
		server.WithTelemetry(o.tracerProvider, o.meterProvider),
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
	if o.redis != nil {
		srvOpts = append(srvOpts,
			server.WithWorker("link cache repair", linkSvc.RunCacheRepair),
			// The worker stops as shutdown begins; this catches tombstones
			// queued before then and by deletes handled during the drain.
			server.WithShutdownHook("link cache repair", func(ctx context.Context) error {
				linkSvc.FlushCacheRepair(ctx)
				return nil
			}),
		)
	}
	if cfg.Clicks.PurgeInterval > 0 {
		srvOpts = append(srvOpts, server.WithWorker("click purger", func(ctx context.Context) error {
			return clickSvc.RunPurger(ctx, cfg.Clicks.Retention, cfg.Clicks.PurgeInterval)
		}))
	}
	return server.New(cfg, log, srvOpts...)
}
