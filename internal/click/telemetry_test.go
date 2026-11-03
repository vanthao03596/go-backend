package click

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/vanthao03596/go-backend/internal/link"
	"github.com/vanthao03596/go-backend/internal/platform/otelx/otelxtest"
)

// spanRepo records the span each Store and DeleteBefore call ran under, which
// is the span the database instrumentation would parent its spans on.
type spanRepo struct {
	*fakeRepo
	mu    sync.Mutex
	spans []trace.SpanContext
}

func (r *spanRepo) Store(ctx context.Context, clicks []link.Click) error {
	r.note(ctx)
	return r.fakeRepo.Store(ctx, clicks)
}

func (r *spanRepo) DeleteBefore(ctx context.Context, cutoff time.Time, limit int32) (int64, error) {
	r.note(ctx)
	return r.fakeRepo.DeleteBefore(ctx, cutoff, limit)
}

func (r *spanRepo) note(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, trace.SpanContextFromContext(ctx))
}

func tracedBuffer(rec *otelxtest.Recorder, repo Repository, cfg BufferConfig) *Buffer {
	cfg.TracerProvider = rec.TracerProvider
	cfg.MeterProvider = rec.MeterProvider
	return NewBuffer(repo, discard, cfg)
}

func TestFlushIsTracedAndMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := otelxtest.New(t)
		repo := &spanRepo{fakeRepo: &fakeRepo{}}
		b := tracedBuffer(rec, repo, BufferConfig{FlushInterval: time.Second, MaxBatch: 100})

		for i := range 3 {
			b.Record(click(int64(i)))
		}
		synctest.Sleep(time.Second)

		spans := rec.SpansNamed("click.flush")
		if len(spans) != 1 {
			t.Fatalf("%d click.flush spans, want 1", len(spans))
		}
		span := spans[0]
		if span.Parent().IsValid() {
			t.Error("flush span should be a root: it batches many requests")
		}
		if got := otelxtest.Attr(span, "shortener.clicks.flush.size").AsInt64(); got != 3 {
			t.Errorf("span flush.size = %d, want 3", got)
		}
		if len(repo.spans) != 1 || repo.spans[0].SpanID() != span.SpanContext().SpanID() {
			t.Errorf("Store ran under %v, want the flush span %v", repo.spans, span.SpanContext())
		}
		if n, sum := rec.HistogramCount(t, "shortener.clicks.flush.size", attrFlushOutcome.String("stored")); n != 1 || sum != 3 {
			t.Errorf("flush.size{stored} = %d flushes / %d clicks, want 1 / 3", n, sum)
		}

		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFailedFlushesAndDropsAreMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := otelxtest.New(t)
		repo := &fakeRepo{storeErr: errors.New("constraint violation")}
		b := tracedBuffer(rec, repo, BufferConfig{FlushInterval: time.Second, MaxBatch: 1})

		b.Record(click(1)) // size trigger: attempt 1
		synctest.Wait()
		synctest.Sleep(maxFlushAttempts * time.Second) // attempts 2 and 3, then dropped

		if n, _ := rec.HistogramCount(t, "shortener.clicks.flush.size", attrFlushOutcome.String("retry")); n != 2 {
			t.Errorf("flush.size{retry} count = %d, want 2", n)
		}
		if n, _ := rec.HistogramCount(t, "shortener.clicks.flush.size", attrFlushOutcome.String("dropped")); n != 1 {
			t.Errorf("flush.size{dropped} count = %d, want 1", n)
		}
		if got := rec.Sum(t, "shortener.clicks.dropped", attrDropReason.String("flush_failed")); got != 1 {
			t.Errorf("dropped{flush_failed} = %d, want 1", got)
		}
		spans := rec.SpansNamed("click.flush")
		if len(spans) != maxFlushAttempts {
			t.Fatalf("%d flush spans, want %d", len(spans), maxFlushAttempts)
		}
		for _, s := range spans {
			if s.Status().Code != codes.Error {
				t.Errorf("failed flush span status = %v, want Error", s.Status())
			}
		}

		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		b.Record(click(2))
		if got := rec.Sum(t, "shortener.clicks.dropped", attrDropReason.String("closed")); got != 1 {
			t.Errorf("dropped{closed} = %d, want 1", got)
		}
	})
}

func TestFullBufferDropsAreMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := otelxtest.New(t)
		repo := &fakeRepo{block: make(chan struct{})}
		b := tracedBuffer(rec, repo, BufferConfig{Size: 2, FlushInterval: time.Hour, MaxBatch: 1})

		b.Record(click(1)) // taken by the flusher, which blocks in Store
		synctest.Wait()
		for i := 2; i <= 4; i++ {
			b.Record(click(int64(i))) // two fit, the fourth does not
		}
		if got := rec.Sum(t, "shortener.clicks.dropped", attrDropReason.String("buffer_full")); got != 1 {
			t.Errorf("dropped{buffer_full} = %d, want 1", got)
		}

		close(repo.block)
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestScheduledPurgeIsTraced(t *testing.T) {
	rec := otelxtest.New(t)
	repo := &spanRepo{fakeRepo: &fakeRepo{deleteSizes: []int64{int64(purgeBatch), 7}}}
	svc := NewService(repo, discard, WithNow(fixedNow), WithTracerProvider(rec.TracerProvider))

	svc.purgeOnce(t.Context(), time.Hour)

	spans := rec.SpansNamed("click.purge")
	if len(spans) != 1 {
		t.Fatalf("%d click.purge spans, want 1", len(spans))
	}
	if got := otelxtest.Attr(spans[0], "shortener.clicks.purged").AsInt64(); got != int64(purgeBatch)+7 {
		t.Errorf("purged attribute = %d", got)
	}
	for i, sc := range repo.spans {
		if sc.SpanID() != spans[0].SpanContext().SpanID() {
			t.Errorf("DELETE %d ran outside the purge span", i)
		}
	}

	repo.setDeleteErr(errors.New("lock timeout"))
	svc.purgeOnce(t.Context(), time.Hour)
	if spans := rec.SpansNamed("click.purge"); spans[1].Status().Code != codes.Error {
		t.Errorf("failed purge span status = %v, want Error", spans[1].Status())
	}
}

// While a failed batch is held for retry, clicks beyond Size are dropped
// because the database is failing, which must not read as a traffic problem.
func TestRetryBacklogDropsHaveTheirOwnReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := otelxtest.New(t)
		repo := &fakeRepo{storeErr: errors.New("db down")}
		b := tracedBuffer(rec, repo, BufferConfig{Size: 3, FlushInterval: time.Second, MaxBatch: 1})

		b.Record(click(1)) // flush fails, batch held for retry
		synctest.Wait()
		for i := 2; i <= 5; i++ {
			b.Record(click(int64(i)))
			synctest.Wait()
		}

		if got := rec.Sum(t, "shortener.clicks.dropped", attrDropReason.String("retry_backlog_full")); got != 2 {
			t.Errorf("dropped{retry_backlog_full} = %d, want 2", got)
		}
		if got := rec.Sum(t, "shortener.clicks.dropped", attrDropReason.String("buffer_full")); got != 0 {
			t.Errorf("dropped{buffer_full} = %d, want 0", got)
		}

		repo.mu.Lock()
		repo.storeErr = nil
		repo.mu.Unlock()
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
