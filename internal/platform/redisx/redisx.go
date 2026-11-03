// Package redisx builds the go-redis client the service uses as a cache:
// URL configuration with cache-friendly defaults, OpenTelemetry
// instrumentation and slog output, plus a circuit breaker for the calls made
// through it.
package redisx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Defaults for settings the URL leaves unset. The cache sits in front of a
// database that answers a redirect lookup in about a millisecond, so the
// go-redis defaults (5s dial, 5s read, three retries, five dial attempts)
// would turn a sick Redis into multi-second redirects. Failing fast and
// reading Postgres is always the better trade. Each can be overridden in
// REDIS_URL, e.g. ?read_timeout=250ms&max_retries=1.
const (
	DefaultDialTimeout = 250 * time.Millisecond
	// DefaultReadTimeout also applies to writes unless write_timeout is set.
	DefaultReadTimeout = 100 * time.Millisecond
	// DefaultPoolTimeout bounds the wait for a free connection.
	DefaultPoolTimeout = 100 * time.Millisecond
)

// Options are the process-level dependencies of the client.
type Options struct {
	// TracerProvider and MeterProvider receive a span per command and the
	// connection pool metrics. Nil uses the otel globals.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// NewClient parses redisURL and returns a client for it. It does not
// connect: go-redis dials lazily, so an unreachable server only shows up as
// failed commands, which callers treat as cache misses.
func NewClient(redisURL string, opts Options) (*redis.Client, error) {
	o, err := ParseURL(redisURL)
	if err != nil {
		return nil, err
	}

	tp, mp := opts.TracerProvider, opts.MeterProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}

	rdb := redis.NewClient(o)
	// Command arguments are not recorded: cached values hold target URLs.
	// Caller attribution walks the stack on every command, which the
	// redirect path cannot afford.
	err = errors.Join(
		redisotel.InstrumentTracing(rdb,
			redisotel.WithTracerProvider(tp),
			redisotel.WithDBStatement(false),
			redisotel.WithCallerEnabled(false)),
		redisotel.InstrumentMetrics(rdb, redisotel.WithMeterProvider(mp)),
	)
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("instrument redis client: %w", err)
	}
	return rdb, nil
}

// ParseURL is redis.ParseURL plus the package defaults. Parse errors never
// echo the URL, which may carry a password.
func ParseURL(redisURL string) (*redis.Options, error) {
	o, err := redis.ParseURL(redisURL)
	if err != nil {
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	applyDefaults(o)
	return o, nil
}

// applyDefaults fills settings ParseURL left at zero. A zero means the URL
// did not mention the setting: ParseURL maps an explicit 0 or negative
// duration to -1 (no timeout).
func applyDefaults(o *redis.Options) {
	if o.DialTimeout == 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.ReadTimeout == 0 {
		o.ReadTimeout = DefaultReadTimeout
	}
	if o.PoolTimeout == 0 {
		o.PoolTimeout = DefaultPoolTimeout
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = -1 // go-redis: -1 disables retries, 0 means three
	}
	// Not settable from the URL. One attempt per dial, then fail.
	if o.DialerRetries == 0 {
		o.DialerRetries = 1
	}
}

// RouteLogs sends go-redis's internal log lines (failed dials, pool
// warnings) to log instead of stderr. The go-redis logger is process-wide,
// so the composition root calls this once.
func RouteLogs(log *slog.Logger) {
	redis.SetLogger(slogPrinter{log: log})
}

type slogPrinter struct{ log *slog.Logger }

func (p slogPrinter) Printf(ctx context.Context, format string, v ...any) {
	p.log.WarnContext(ctx, "redis client", "detail", fmt.Sprintf(format, v...))
}
