package click

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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
	return c
}

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
}

var _ link.ClickSink = (*Buffer)(nil)

// NewBuffer prepares a Buffer. The flusher goroutine starts on the first
// Record, so a Buffer that never sees traffic costs nothing and leaks
// nothing. Call Close to flush and stop it.
func NewBuffer(repo Repository, log *slog.Logger, cfg BufferConfig) *Buffer {
	cfg = cfg.withDefaults()
	return &Buffer{
		repo: repo,
		log:  log,
		cfg:  cfg,
		ch:   make(chan link.Click, cfg.Size),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Record implements link.ClickSink. It sanitises the header values and
// enqueues the click, dropping it if the buffer is full or closed.
func (b *Buffer) Record(c link.Click) {
	c.Referrer = sanitizeReferrer(c.Referrer)
	c.UserAgent = sanitize(c.UserAgent, MaxUserAgentLen)

	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		b.dropped.Add(1)
		return
	}
	b.startOnce.Do(func() {
		b.started.Store(true)
		go b.run()
	})
	select {
	case b.ch <- c:
	default:
		b.dropped.Add(1)
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
// for tests and metrics.
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
			f.b.dropped.Add(1)
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
func (f *flusher) flush() {
	b := f.b
	if n := b.dropped.Swap(0); n > 0 {
		b.log.Warn("click buffer dropped clicks", "count", n, "capacity", b.cfg.Size)
	}
	if len(f.batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.FlushTimeout)
	defer cancel()
	err := b.repo.Store(ctx, f.batch)
	switch {
	case err == nil:
	case f.attempts+1 < maxFlushAttempts:
		f.attempts++
		b.log.Warn("flush clicks failed, will retry", "count", len(f.batch), "attempt", f.attempts, "err", err)
		return
	default:
		b.log.Error("flush clicks failed, dropping batch", "count", len(f.batch), "attempts", f.attempts+1, "err", err)
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
