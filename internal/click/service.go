package click

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// StatsDays is the length of the daily window the stats endpoint reports.
	StatsDays = 7
	// purgeBatch bounds one DELETE so a large backlog never holds a long lock.
	purgeBatch int32 = 5_000
	// purgeInitialDelay lets the service warm up before the first purge.
	// A process that lives shorter than this never purges; that is fine for
	// tests and crash loops, and "api clicks purge" exists for everything else.
	purgeInitialDelay = time.Minute
)

// Service answers statistics and runs retention. It has no HTTP or SQL.
type Service struct {
	repo           Repository
	log            *slog.Logger
	now            func() time.Time
	tracerProvider trace.TracerProvider
	tracer         trace.Tracer
}

// ServiceOption customises a Service at construction time.
type ServiceOption func(*Service)

// WithNow replaces the clock used to place the stats window and the
// retention cutoff.
func WithNow(now func() time.Time) ServiceOption {
	return func(s *Service) { s.now = now }
}

// WithTracerProvider sets where the purger's spans go. Without it the otel
// global is used, a no-op until telemetry is set up.
func WithTracerProvider(tp trace.TracerProvider) ServiceOption {
	return func(s *Service) { s.tracerProvider = tp }
}

// NewService wires a Service.
func NewService(repo Repository, log *slog.Logger, opts ...ServiceOption) *Service {
	s := &Service{repo: repo, log: log, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.tracerProvider == nil {
		s.tracerProvider = otel.GetTracerProvider()
	}
	s.tracer = s.tracerProvider.Tracer(instrumentationName)
	return s
}

// Stats returns total alongside the last StatsDays UTC days of clicks for
// linkID, oldest first, with zero entries for days that saw none. The caller
// supplies total from the link row because that counter, not the clicks
// table, is authoritative: it survives retention purges.
func (s *Service) Stats(ctx context.Context, linkID, total int64) (Stats, error) {
	today := startOfUTCDay(s.now())
	since := today.AddDate(0, 0, -(StatsDays - 1))
	until := today.AddDate(0, 0, 1)

	counts, err := s.repo.CountByDay(ctx, linkID, since, until)
	if err != nil {
		return Stats{}, err
	}
	byDay := make(map[time.Time]int64, len(counts))
	for _, c := range counts {
		byDay[startOfUTCDay(c.Day)] = c.Clicks
	}

	days := make([]DayCount, 0, StatsDays)
	for d := since; d.Before(until); d = d.AddDate(0, 0, 1) {
		days = append(days, DayCount{Day: d, Clicks: byDay[d]})
	}
	return Stats{Total: total, Days: days}, nil
}

// Purge deletes clicks older than retention in batches and returns how many
// rows went. It stops early with the rows removed so far if ctx ends.
func (s *Service) Purge(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := s.now().Add(-retention)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("purge clicks: %w", err)
		}
		n, err := s.repo.DeleteBefore(ctx, cutoff, purgeBatch)
		total += n
		if err != nil {
			return total, err
		}
		if n < int64(purgeBatch) {
			return total, nil
		}
	}
}

// RunPurger purges on a fixed interval until ctx is cancelled. The first run
// happens purgeInitialDelay after start rather than a full interval later,
// so a service redeployed daily still enforces retention. Failures are
// logged and retried on the next tick; they never stop the loop. It returns
// nil once ctx ends.
func (s *Service) RunPurger(ctx context.Context, retention, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("purge interval must be positive, got %s", interval)
	}
	timer := time.NewTimer(purgeInitialDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.purgeOnce(ctx, retention)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// purgeOnce runs one scheduled purge under a root span, which parents the
// batched DELETE spans.
func (s *Service) purgeOnce(ctx context.Context, retention time.Duration) {
	ctx, span := s.tracer.Start(ctx, "click.purge", trace.WithNewRoot(),
		trace.WithAttributes(attribute.String("shortener.clicks.retention", retention.String())))
	defer span.End()

	start := s.now()
	n, err := s.Purge(ctx, retention)
	span.SetAttributes(attribute.Int64("shortener.clicks.purged", n))
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "clicks purged", "deleted", n, "retention", retention, "duration", s.now().Sub(start))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.log.InfoContext(ctx, "click purge interrupted", "deleted", n, "err", err)
	default:
		span.RecordError(err)
		span.SetStatus(codes.Error, "purge clicks failed")
		s.log.ErrorContext(ctx, "click purge failed", "deleted", n, "err", err)
	}
}

func startOfUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
