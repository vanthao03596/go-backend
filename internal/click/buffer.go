package click

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/vanthao03596/go-backend/internal/link"
)

// BufferConfig tunes a Buffer. Zero values take the defaults below.
type BufferConfig struct {
	// Size is the channel capacity. When it is full new clicks are dropped
	// rather than slowing the redirect. It also caps how many clicks the
	// flusher holds while a failed batch is being retried.
	Size int
	// FlushInterval bounds how long a click waits before it is written.
	FlushInterval time.Duration
	// MaxBatch flushes early once this many clicks are pending.
	MaxBatch int
	// FlushTimeout bounds one Store call.
	FlushTimeout time.Duration

	// TracerProvider receives one span per flush, which parents the
	// flush's database spans. MeterProvider receives the flush and drop
	// metrics. Nil uses the otel globals.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// Defaults for BufferConfig.
const (
	DefaultBufferSize    = 10_000
	DefaultFlushInterval = time.Second
	DefaultMaxBatch      = 500
	DefaultFlushTimeout  = 5 * time.Second

	// maxFlushAttempts is how many consecutive failures a batch survives
	// before it is dropped. Retries happen on the flush interval, so this
	// rides out a database blip of a couple of seconds.
	maxFlushAttempts = 3
)

func (c BufferConfig) withDefaults() BufferConfig {
	if c.Size <= 0 {
		c.Size = DefaultBufferSize
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = DefaultMaxBatch
	}
	if c.FlushTimeout <= 0 {
		c.FlushTimeout = DefaultFlushTimeout
	}
	if c.TracerProvider == nil {
		c.TracerProvider = otel.GetTracerProvider()
	}
	if c.MeterProvider == nil {
		c.MeterProvider = otel.GetMeterProvider()
	}
	return c
}

// Telemetry attribute keys and values for the buffer's metrics.
const (
	attrFlushOutcome = attribute.Key("shortener.clicks.flush.outcome")
	attrDropReason   = attribute.Key("shortener.clicks.drop_reason")
)

var (
	flushStored  = metric.WithAttributeSet(attribute.NewSet(attrFlushOutcome.String("stored")))
	flushRetry   = metric.WithAttributeSet(attribute.NewSet(attrFlushOutcome.String("retry")))
	flushDropped = metric.WithAttributeSet(attribute.NewSet(attrFlushOutcome.String("dropped")))

	// buffer_full: the enqueue channel was full (traffic outran the flusher).
	// retry_backlog_full: a failed batch is held for retry and has grown to
	// Size, so the database is the problem. flush_failed: a batch was given
	// up after maxFlushAttempts. closed: recorded after Close.
	dropBufferFull       = metric.WithAttributeSet(attribute.NewSet(attrDropReason.String("buffer_full")))
	dropRetryBacklogFull = metric.WithAttributeSet(attribute.NewSet(attrDropReason.String("retry_backlog_full")))
	dropClosed           = metric.WithAttributeSet(attribute.NewSet(attrDropReason.String("closed")))
	dropFlushFailed      = metric.WithAttributeSet(attribute.NewSet(attrDropReason.String("flush_failed")))
)

// Buffer implements link.ClickSink with a bounded channel drained by one
// flusher goroutine. Record never blocks; Close drains what is pending.
//
// Delivery is best effort: a full buffer drops clicks, and a batch whose
// Store keeps failing is dropped after maxFlushAttempts. Both are logged
// with counts. Lifetime totals therefore lag reality by at most one flush
// interval and may undercount under sustained database failure, which is
// the trade the roadmap accepts for keeping the redirect path free of
// writes.
type Buffer struct {
	repo Repository
	log  *slog.Logger
	cfg  BufferConfig

	ch   chan link.Click
	stop chan struct{}
	done chan struct{}

	// mu orders Record against Close: Record holds it shared while it checks
	// closed and sends, Close holds it exclusively while it sets closed. The
	// flusher's final drain therefore sees every click that was accepted.
	mu     sync.RWMutex
	closed bool

	startOnce sync.Once
	started   atomic.Bool
	dropped   atomic.Int64 // since the last flush log line

	tracer    trace.Tracer
	flushSize metric.Int64Histogram
	dropCount metric.Int64Counter
}

var _ link.ClickSink = (*Buffer)(nil)

// NewBuffer prepares a Buffer. The flusher goroutine starts on the first
// Record, so a Buffer that never sees traffic costs nothing and leaks
// nothing. Call Close to flush and stop it.
func NewBuffer(repo Repository, log *slog.Logger, cfg BufferConfig) *Buffer {
	cfg = cfg.withDefaults()
	b := &Buffer{
		repo:   repo,
		log:    log,
		cfg:    cfg,
		ch:     make(chan link.Click, cfg.Size),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		tracer: cfg.TracerProvider.Tracer(instrumentationName),
	}
	meter := cfg.MeterProvider.Meter(instrumentationName)
	var errs [2]error
	b.flushSize, errs[0] = meter.Int64Histogram("shortener.clicks.flush.size",
		metric.WithDescription("Clicks per flush attempt, by outcome: stored, retry (kept for the next tick) or dropped (gave up)."),
		metric.WithUnit("{click}"))
	b.dropCount, errs[1] = meter.Int64Counter("shortener.clicks.dropped",
		metric.WithDescription("Clicks discarded before reaching the database, by reason."),
		metric.WithUnit("{click}"))
	if err := errors.Join(errs[:]...); err != nil {
		otel.Handle(err)
	}
	return b
}

// drop counts one discarded click for the flush log line and the metric.
func (b *Buffer) drop(reason metric.AddOption) {
	b.dropped.Add(1)
	b.dropCount.Add(context.Background(), 1, reason)
}

// Record implements link.ClickSink. It sanitises the header values and
// enqueues the click, dropping it if the buffer is full or closed.
func (b *Buffer) Record(c link.Click) {
	c.Referrer = sanitizeReferrer(c.Referrer)
	c.UserAgent = sanitize(c.UserAgent, MaxUserAgentLen)

	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		b.drop(dropClosed)
		return
	}
	b.startOnce.Do(func() {
		b.started.Store(true)
		go b.run()
	})
	select {
	case b.ch <- c:
	default:
		b.drop(dropBufferFull)
	}
}

// Close stops accepting clicks, flushes everything already enqueued and
// waits for the flusher to exit or ctx to end. Calling it again only waits.
func (b *Buffer) Close(ctx context.Context) error {
	b.mu.Lock()
	first := !b.closed
	b.closed = true
	b.mu.Unlock()

	if first {
		close(b.stop)
	}
	if !b.started.Load() {
		return nil
	}
	select {
	case <-b.done:
		if n := b.dropped.Swap(0); n > 0 {
			b.log.Warn("click buffer dropped clicks", "count", n, "capacity", b.cfg.Size)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("click buffer drain: %w", ctx.Err())
	}
}

// Dropped reports clicks discarded since the last flush log line. It exists
// for tests; the shortener.clicks.dropped metric is the operational view.
func (b *Buffer) Dropped() int64 { return b.dropped.Load() }

func (b *Buffer) run() {
	defer close(b.done)

	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	f := flusher{b: b, batch: make([]link.Click, 0, b.cfg.MaxBatch)}
	for {
		select {
		case c := <-b.ch:
			f.add(c)
		case <-ticker.C:
			f.flush()
		case <-b.stop:
			// Close holds the lock while it sets closed, so no Record is
			// mid-send once stop is observed: whatever is in the channel
			// now is everything.
			for {
				select {
				case c := <-b.ch:
					f.add(c)
				default:
					f.flush()
					return
				}
			}
		}
	}
}

// flusher is the flusher goroutine's state: the pending batch and how many
// times its current contents have failed to store.
type flusher struct {
	b        *Buffer
	batch    []link.Click
	attempts int
}

// add appends c and flushes when the batch is full. While a batch is being
// retried the size trigger is suspended so a failing database is not
// hammered on every click; the batch then grows until Size, after which
// clicks are dropped.
func (f *flusher) add(c link.Click) {
	if f.attempts > 0 {
		if len(f.batch) >= f.b.cfg.Size {
			f.b.drop(dropRetryBacklogFull)
			return
		}
		f.batch = append(f.batch, c)
		return
	}
	f.batch = append(f.batch, c)
	if len(f.batch) >= f.b.cfg.MaxBatch {
		f.flush()
	}
}

// flush stores the batch. On failure the batch is kept for the next tick
// until maxFlushAttempts, then dropped.
//
// Each attempt is a root span: flushes run off the request path and batch
// clicks from many requests, so no single request trace could own them. The
// span is what lets the database spans underneath be recorded at all.
func (f *flusher) flush() {
	b := f.b
	if n := b.dropped.Swap(0); n > 0 {
		b.log.Warn("click buffer dropped clicks", "count", n, "capacity", b.cfg.Size)
	}
	if len(f.batch) == 0 {
		return
	}
	size := int64(len(f.batch))
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.FlushTimeout)
	defer cancel()
	ctx, span := b.tracer.Start(ctx, "click.flush",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.Int64("shortener.clicks.flush.size", size),
			attribute.Int("shortener.clicks.flush.attempt", f.attempts+1),
		))
	defer span.End()

	err := b.repo.Store(ctx, f.batch)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "store clicks failed")
	}
	switch {
	case err == nil:
		b.flushSize.Record(ctx, size, flushStored)
	case f.attempts+1 < maxFlushAttempts:
		f.attempts++
		b.flushSize.Record(ctx, size, flushRetry)
		b.log.WarnContext(ctx, "flush clicks failed, will retry", "count", size, "attempt", f.attempts, "err", err)
		return
	default:
		b.flushSize.Record(ctx, size, flushDropped)
		b.dropCount.Add(ctx, size, dropFlushFailed)
		b.log.ErrorContext(ctx, "flush clicks failed, dropping batch", "count", size, "attempts", f.attempts+1, "err", err)
	}
	f.attempts = 0
	clear(f.batch)
	f.batch = f.batch[:0]
}

// sanitizeReferrer reduces a Referer header to scheme, host and path. Query
// strings and fragments are dropped because they routinely carry session
// tokens and addresses, and user info is never kept. Anything that does not
// parse as an absolute http(s) URL is discarded.
func sanitizeReferrer(raw string) string {
	raw = sanitize(raw, MaxReferrerLen)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	clean := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return sanitize(clean.String(), MaxReferrerLen)
}

// sanitize makes a header value safe to store as Postgres text: invalid
// UTF-8 and NUL bytes are removed (Postgres rejects both) and the result is
// cut at max bytes on a rune boundary.
func sanitize(s string, maxLen int) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
