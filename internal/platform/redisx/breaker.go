package redisx

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Circuit breaker defaults. Five consecutive failures is past what a lost
// packet or a restarted connection produces. A one second cooldown means a
// hung server costs at most one probe timeout per second, and service
// recovers within a second of Redis doing so.
const (
	DefaultBreakerThreshold = 5
	DefaultBreakerCooldown  = time.Second
)

// ErrCircuitOpen is returned for calls the breaker refused.
var ErrCircuitOpen = errors.New("redis: circuit breaker open")

// BreakerConfig tunes a Breaker. Zero values use the defaults.
type BreakerConfig struct {
	// Threshold is how many consecutive failures open the circuit.
	Threshold int
	// Cooldown is how long the circuit stays open before a probe.
	Cooldown time.Duration
}

// Breaker stops calls to a Redis server that keeps failing. go-redis already
// fails fast once every pooled dial has failed, but a server that accepts
// connections and then stops answering (overloaded, paused, partitioned
// mid-stream) would otherwise cost every caller a full read timeout.
//
// After Threshold consecutive transport failures the circuit opens and Allow
// refuses with ErrCircuitOpen. Once Cooldown has passed one call is let
// through as a probe while the rest keep failing fast; its success closes the
// circuit and its failure re-opens it. Success is a normal reply, including
// redis.Nil (a cache miss). Error replies count as failures: WRONGPASS,
// NOAUTH, READONLY, OOM or LOADING mean the server answers but cannot serve
// the cache, and every call to it is still a wasted round trip. Cancelled
// callers count as neither.
//
// Wrap whole operations with it (Allow, call, ReportResult). It has the shape
// of a redis.Limiter but must not be installed as Options.Limiter: go-redis
// consults the Limiter for every command, including the HELLO and AUTH that
// set up a new connection inside another command, so a half-open breaker
// there refuses the probe's own handshake and never closes once an outage has
// emptied the pool.
type Breaker struct {
	threshold int32
	cooldown  int64 // nanoseconds
	log       *slog.Logger

	failures  atomic.Int32 // consecutive, reset by any success
	openUntil atomic.Int64 // unix nanoseconds; 0 while closed
}

// NewBreaker returns a closed Breaker that logs its transitions to log.
func NewBreaker(cfg BreakerConfig, log *slog.Logger) *Breaker {
	if cfg.Threshold <= 0 {
		cfg.Threshold = DefaultBreakerThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultBreakerCooldown
	}
	return &Breaker{
		threshold: int32(min(cfg.Threshold, 1<<30)), //nolint:gosec // G115: clamped to int32 range
		cooldown:  int64(cfg.Cooldown),
		log:       log,
	}
}

// Allow returns nil if a call may proceed, or ErrCircuitOpen. A caller that
// was allowed must report the outcome with ReportResult.
func (b *Breaker) Allow() error {
	until := b.openUntil.Load()
	if until == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	if now < until {
		return ErrCircuitOpen
	}
	// Cooldown is over. The caller that moves the deadline forward is the
	// probe; everyone else keeps failing fast until it reports.
	if b.openUntil.CompareAndSwap(until, now+b.cooldown) {
		return nil
	}
	return ErrCircuitOpen
}

// ReportResult records the outcome of an allowed call.
func (b *Breaker) ReportResult(err error) {
	switch {
	case answered(err):
		// Loads first: on the hot path the breaker is closed and healthy,
		// and a plain load does not contend for the cache line.
		if b.failures.Load() != 0 {
			b.failures.Store(0)
		}
		if b.openUntil.Load() != 0 && b.openUntil.Swap(0) != 0 {
			b.log.Info("redis circuit breaker closed: server answered")
		}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller gave up; that says nothing about the server.
	default:
		if b.failures.Add(1) < b.threshold {
			return
		}
		if b.openUntil.Swap(time.Now().UnixNano()+b.cooldown) == 0 {
			b.log.Warn("redis circuit breaker opened: skipping Redis",
				"consecutive_failures", b.failures.Load(), "cooldown", time.Duration(b.cooldown), "err", err)
		}
	}
}

// Open reports whether the circuit is open, for tests and diagnostics.
func (b *Breaker) Open() bool { return b.openUntil.Load() != 0 }

// answered reports whether err means the server served the call.
func answered(err error) bool {
	return err == nil || errors.Is(err, redis.Nil)
}
