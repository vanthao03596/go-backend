package link

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vanthao03596/go-backend/internal/platform/otelx/otelxtest"
)

// fakeCache is an in-memory Cache with the same Fill/Put semantics as
// RedisCache. Setting err makes every call fail, like an unreachable Redis.
type fakeCache struct {
	mu      sync.Mutex
	entries map[string]Destination
	err     error
	puts    int
}

func newFakeCache() *fakeCache { return &fakeCache{entries: map[string]Destination{}} }

func (c *fakeCache) Get(_ context.Context, code string) (Destination, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return Destination{}, false, c.err
	}
	d, ok := c.entries[code]
	return d, ok, nil
}

func (c *fakeCache) Fill(_ context.Context, code string, d Destination) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if _, exists := c.entries[code]; !exists {
		c.entries[code] = d
	}
	return nil
}

func (c *fakeCache) Put(_ context.Context, code string, d Destination) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	if c.err != nil {
		return c.err
	}
	c.entries[code] = d
	return nil
}

func (c *fakeCache) entry(code string) (Destination, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.entries[code]
	return d, ok
}

func (c *fakeCache) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

var errCacheDown = errors.New("redis: connection refused")

func TestResolveServesRepeatLookupsFromTheCache(t *testing.T) {
	repo, cache := newFakeRepo(), newFakeCache()
	sink := &recordingSink{}
	svc := newTestService(repo, WithCache(cache), WithClickSink(sink))
	l, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/a"})
	if err != nil {
		t.Fatal(err)
	}
	// Create wrote the entry, so even the first redirect is a hit.
	if d, ok := cache.entry(l.Code); !ok || d.LinkID != l.ID || d.URL != l.TargetURL {
		t.Fatalf("entry after create = %+v, %v", d, ok)
	}

	for range 3 {
		target, err := svc.Resolve(t.Context(), l.Code, &Visit{})
		if err != nil || target != "https://example.com/a" {
			t.Fatalf("Resolve = %q, %v", target, err)
		}
	}
	if repo.lookups != 0 {
		t.Errorf("database lookups = %d, want 0", repo.lookups)
	}
	// Clicks carry the link id from the cached entry.
	if len(sink.clicks) != 3 || sink.clicks[0].LinkID != l.ID {
		t.Errorf("clicks = %+v", sink.clicks)
	}
}

func TestResolveFillsTheCacheOnAMiss(t *testing.T) {
	repo := newFakeRepo()
	l := seedLink(t, repo) // created without a cache, so nothing is cached
	cache := newFakeCache()
	svc := newTestService(repo, WithCache(cache))

	for range 2 {
		if _, err := svc.Resolve(t.Context(), l.Code, nil); err != nil {
			t.Fatal(err)
		}
	}
	if repo.lookups != 1 {
		t.Errorf("database lookups = %d, want 1 (the miss)", repo.lookups)
	}
	if d, ok := cache.entry(l.Code); !ok || d.LinkID != l.ID {
		t.Errorf("entry = %+v, %v", d, ok)
	}
}

func TestUnknownCodesAreNegativelyCachedUntilCreated(t *testing.T) {
	repo, cache := newFakeRepo(), newFakeCache()
	svc := newTestService(repo, WithCache(cache))

	for range 2 {
		if _, err := svc.Resolve(t.Context(), "promo", nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown code = %v, want ErrNotFound", err)
		}
	}
	if repo.lookups != 1 {
		t.Errorf("database lookups = %d, want 1", repo.lookups)
	}
	if d, _ := cache.entry("promo"); !d.Missing {
		t.Fatalf("entry = %+v, want a negative entry", d)
	}

	// Creating the alias replaces the negative entry at once rather than
	// leaving it to expire.
	if _, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/promo", Alias: "promo"}); err != nil {
		t.Fatal(err)
	}
	if target, err := svc.Resolve(t.Context(), "promo", nil); err != nil || target != "https://example.com/promo" {
		t.Fatalf("Resolve after create = %q, %v", target, err)
	}
}

func TestDeleteWritesATombstone(t *testing.T) {
	repo, cache := newFakeRepo(), newFakeCache()
	svc := newTestService(repo, WithCache(cache))
	l, _ := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/a"})
	if _, err := svc.Resolve(t.Context(), l.Code, nil); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(t.Context(), 1, l.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(t.Context(), l.Code, nil); !errors.Is(err, ErrGone) {
		t.Fatalf("Resolve after delete = %v, want ErrGone", err)
	}
	if repo.lookups != 0 {
		t.Errorf("database lookups = %d, want 0: the tombstone answers", repo.lookups)
	}

	// A failed delete (wrong owner) must not touch the cache.
	other, _ := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/b"})
	if err := svc.Delete(t.Context(), 2, other.Code); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if d, _ := cache.entry(other.Code); d.Deleted {
		t.Error("tombstone written for a delete that did not happen")
	}
}

func TestCachedExpiryIsEnforced(t *testing.T) {
	repo, cache := newFakeRepo(), newFakeCache()
	svc := newTestService(repo, WithCache(cache))
	exp := svc.now().Add(time.Hour)
	l, _ := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/a", ExpiresAt: &exp})

	if _, err := svc.Resolve(t.Context(), l.Code, nil); err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return exp }
	if _, err := svc.Resolve(t.Context(), l.Code, nil); !errors.Is(err, ErrGone) {
		t.Fatalf("Resolve at expiry = %v, want ErrGone", err)
	}
	if repo.lookups != 0 {
		t.Errorf("database lookups = %d, want 0", repo.lookups)
	}
}

func TestCacheFailuresFallBackToTheDatabase(t *testing.T) {
	rec := otelxtest.New(t)
	repo, cache := newFakeRepo(), newFakeCache()
	svc := newTestService(repo, WithCache(cache), WithMeterProvider(rec.MeterProvider))
	l, _ := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/a"})

	if _, err := svc.Resolve(t.Context(), l.Code, nil); err != nil { // hit
		t.Fatal(err)
	}
	if _, err := svc.Resolve(t.Context(), "nope123", nil); !errors.Is(err, ErrNotFound) { // miss, filled
		t.Fatal(err)
	}

	cache.fail(errCacheDown)
	for range 2 {
		target, err := svc.Resolve(t.Context(), l.Code, nil)
		if err != nil || target != "https://example.com/a" {
			t.Fatalf("Resolve with the cache down = %q, %v", target, err)
		}
	}
	if repo.lookups != 3 {
		t.Errorf("database lookups = %d, want 3 (one miss, two errors)", repo.lookups)
	}

	for result, want := range map[string]int64{"hit": 1, "miss": 1, "error": 2} {
		if got := rec.Sum(t, "shortener.links.cache.lookups", attrCacheResult.String(result)); got != want {
			t.Errorf("cache.lookups{result=%s} = %d, want %d", result, got, want)
		}
	}

	// Writes fail too, and are counted; the create itself succeeds.
	if _, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/b"}); err != nil {
		t.Fatalf("Create with the cache down = %v", err)
	}
	if got := rec.Sum(t, "shortener.links.cache.write_errors", attrCacheOperation.String("put")); got != 1 {
		t.Errorf("cache.write_errors{operation=put} = %d, want 1", got)
	}
}

func TestFailedTombstonesAreRepairedWhenTheCacheRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo, cache := newFakeRepo(), newFakeCache()
		svc := newTestService(repo, WithCache(cache))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- svc.RunCacheRepair(ctx) }()

		l, _ := svc.Create(ctx, 1, CreateParams{URL: "https://example.com/a"})
		cache.fail(errCacheDown)
		if err := svc.Delete(ctx, 1, l.Code); err != nil {
			t.Fatalf("Delete with the cache down = %v; the database delete must stand", err)
		}
		// The stale live entry is still there: this is what repair is for.
		if d, _ := cache.entry(l.Code); d.Deleted {
			t.Fatal("tombstone written while the cache was failing")
		}

		time.Sleep(3 * cacheRepairInterval)
		synctest.Wait()
		if d, _ := cache.entry(l.Code); d.Deleted || svc.pendingTombstoneCount() != 1 {
			t.Fatalf("entry = %+v pending = %d while the cache is still down", d, svc.pendingTombstoneCount())
		}

		cache.fail(nil)
		time.Sleep(cacheRepairInterval)
		synctest.Wait()
		if d, _ := cache.entry(l.Code); !d.Deleted {
			t.Fatalf("entry = %+v after recovery, want the tombstone", d)
		}
		if n := svc.pendingTombstoneCount(); n != 0 {
			t.Errorf("pending = %d after repair", n)
		}

		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("RunCacheRepair = %v, want context.Canceled", err)
		}
	})
}

func TestTombstoneQueueIsBounded(t *testing.T) {
	svc := newTestService(newFakeRepo(), WithCache(newFakeCache()))
	for i := range maxPendingTombstones {
		if !svc.queueTombstone(strconv.Itoa(i)) {
			t.Fatalf("queue refused entry %d", i)
		}
	}
	if svc.queueTombstone("one-too-many") {
		t.Error("queue accepted more than maxPendingTombstones")
	}
}

// stubBreaker refuses with refuse and records every reported result.
type stubBreaker struct {
	refuse  error
	reports []error
}

func (b *stubBreaker) Allow() error           { return b.refuse }
func (b *stubBreaker) ReportResult(err error) { b.reports = append(b.reports, err) }

// corruptCache answers every Get with a malformed entry.
type corruptCache struct{ *fakeCache }

func (corruptCache) Get(context.Context, string) (Destination, bool, error) {
	return Destination{}, false, errBadCacheEntry
}

func TestGuardedCacheShortCircuits(t *testing.T) {
	cache := newFakeCache()
	b := &stubBreaker{refuse: errCacheDown}
	g := GuardCache(cache, b)

	if _, _, err := g.Get(t.Context(), "abc"); !errors.Is(err, errCacheDown) {
		t.Errorf("Get while refused = %v", err)
	}
	if err := g.Put(t.Context(), "abc", Destination{Deleted: true}); !errors.Is(err, errCacheDown) || cache.puts != 0 {
		t.Errorf("Put while refused = %v, reached the cache %d times", err, cache.puts)
	}
	if len(b.reports) != 0 {
		t.Errorf("refused calls reported %v", b.reports)
	}

	b.refuse = nil
	cache.fail(errCacheDown)
	_ = g.Fill(t.Context(), "abc", Destination{Missing: true})
	cache.fail(nil)
	_, _, _ = g.Get(t.Context(), "abc")
	_, _, _ = GuardCache(corruptCache{cache}, b).Get(t.Context(), "abc")
	want := []error{errCacheDown, nil, nil} // a malformed entry still means Redis answered
	if len(b.reports) != len(want) || !errors.Is(b.reports[0], want[0]) || b.reports[1] != nil || b.reports[2] != nil {
		t.Errorf("reports = %v, want %v", b.reports, want)
	}
}

func TestFlushCacheRepairWritesQueuedTombstones(t *testing.T) {
	repo, cache := newFakeRepo(), newFakeCache()
	svc := newTestService(repo, WithCache(cache))
	l, _ := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/a"})

	// A delete during the shutdown drain, after the repair worker stopped.
	cache.fail(errCacheDown)
	if err := svc.Delete(t.Context(), 1, l.Code); err != nil {
		t.Fatal(err)
	}
	svc.FlushCacheRepair(t.Context())
	if n := svc.pendingTombstoneCount(); n != 1 {
		t.Fatalf("pending = %d with the cache still down, want 1", n)
	}

	cache.fail(nil)
	svc.FlushCacheRepair(t.Context())
	if d, _ := cache.entry(l.Code); !d.Deleted {
		t.Errorf("entry = %+v, want the tombstone", d)
	}
	if n := svc.pendingTombstoneCount(); n != 0 {
		t.Errorf("pending = %d after the flush", n)
	}
	newTestService(repo).FlushCacheRepair(t.Context()) // no cache: a no-op
}
