package click

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vanthao03596/go-backend/internal/link"
)

// fakeRepo is an in-memory Repository. Store appends each batch; block, when
// non-nil, makes Store wait until the channel is closed.
type fakeRepo struct {
	mu       sync.Mutex
	batches  [][]link.Click
	storeErr error
	block    chan struct{}

	days        []DayCount
	daysErr     error
	gotSince    time.Time
	gotUntil    time.Time
	deleteSizes []int64 // returned per DeleteBefore call, in order
	deleteErr   error
	cutoffs     []time.Time
}

func (f *fakeRepo) Store(_ context.Context, clicks []link.Click) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return f.storeErr
	}
	cp := make([]link.Click, len(clicks))
	copy(cp, clicks)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeRepo) CountByDay(_ context.Context, _ int64, since, until time.Time) ([]DayCount, error) {
	f.gotSince, f.gotUntil = since, until
	return f.days, f.daysErr
}

func (f *fakeRepo) DeleteBefore(_ context.Context, cutoff time.Time, _ int32) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoffs = append(f.cutoffs, cutoff)
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	if len(f.deleteSizes) == 0 {
		return 0, nil
	}
	n := f.deleteSizes[0]
	f.deleteSizes = f.deleteSizes[1:]
	return n, nil
}

func (f *fakeRepo) purgeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cutoffs)
}

func (f *fakeRepo) setDeleteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
}

func (f *fakeRepo) sizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, 0, len(f.batches))
	for _, b := range f.batches {
		out = append(out, len(b))
	}
	return out
}

func (f *fakeRepo) total() int {
	n := 0
	for _, s := range f.sizes() {
		n += s
	}
	return n
}

var discard = slog.New(slog.DiscardHandler)

func click(id int64) link.Click {
	return link.Click{LinkID: id, OccurredAt: time.Now(), Referrer: "https://ref.example/", UserAgent: "ua"}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBufferFlushesOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{}
		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Second, MaxBatch: 100})

		for i := range 3 {
			b.Record(click(int64(i)))
		}
		synctest.Wait()
		if got := repo.sizes(); len(got) != 0 {
			t.Fatalf("stored before the interval elapsed: %v", got)
		}

		synctest.Sleep(time.Second)
		if got := repo.sizes(); !equalInts(got, []int{3}) {
			t.Fatalf("batches after one interval = %v, want [3]", got)
		}

		// An idle tick must not call Store.
		synctest.Sleep(time.Second)
		if got := repo.sizes(); !equalInts(got, []int{3}) {
			t.Fatalf("idle tick stored something: %v", got)
		}
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBufferFlushesWhenBatchIsFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{}
		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Hour, MaxBatch: 2})

		for i := range 5 {
			b.Record(click(int64(i)))
		}
		synctest.Wait()
		if got := repo.sizes(); !equalInts(got, []int{2, 2}) {
			t.Fatalf("batches = %v, want [2 2] before any tick", got)
		}
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := repo.sizes(); !equalInts(got, []int{2, 2, 1}) {
			t.Fatalf("batches after close = %v, want [2 2 1]", got)
		}
	})
}

func TestBufferCloseDrainsAndRejectsLateClicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{}
		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Hour, MaxBatch: 100})
		for i := range 3 {
			b.Record(click(int64(i)))
		}
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := repo.sizes(); !equalInts(got, []int{3}) {
			t.Fatalf("batches = %v, want [3]", got)
		}

		b.Record(click(9))
		if b.Dropped() != 1 {
			t.Errorf("dropped = %d, want 1 after Close", b.Dropped())
		}
		if repo.total() != 3 {
			t.Errorf("late click was stored")
		}
	})
}

func TestBufferDropsWhenFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{block: make(chan struct{})}
		b := NewBuffer(repo, discard, BufferConfig{Size: 2, FlushInterval: time.Hour, MaxBatch: 1})

		// The first click is taken by the flusher, which then blocks in Store.
		b.Record(click(1))
		synctest.Wait()
		// Two fit in the channel, the fourth has nowhere to go.
		b.Record(click(2))
		b.Record(click(3))
		b.Record(click(4))
		if got := b.Dropped(); got != 1 {
			t.Fatalf("dropped = %d, want 1", got)
		}

		close(repo.block)
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if repo.total() != 3 {
			t.Errorf("stored %d clicks, want 3", repo.total())
		}
		if b.Dropped() != 0 {
			t.Errorf("drop counter was not reset after being logged: %d", b.Dropped())
		}
	})
}

func TestBufferRetriesFailedFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{storeErr: errors.New("db down")}
		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Second, MaxBatch: 100})

		b.Record(click(1))
		synctest.Sleep(time.Second)
		if repo.total() != 0 {
			t.Fatal("failed batch must not be recorded")
		}

		// The database comes back before the retry budget is spent: the
		// held batch and the new click land together.
		repo.mu.Lock()
		repo.storeErr = nil
		repo.mu.Unlock()
		b.Record(click(2))
		synctest.Sleep(time.Second)
		if got := repo.sizes(); !equalInts(got, []int{2}) {
			t.Fatalf("batches after recovery = %v, want [2]", got)
		}
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBufferDropsBatchAfterMaxAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{storeErr: errors.New("constraint violation")}
		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Second, MaxBatch: 1})

		b.Record(click(1)) // size trigger: attempt 1
		synctest.Wait()
		synctest.Sleep(maxFlushAttempts * time.Second) // ticks: attempts 2..3, then dropped

		repo.mu.Lock()
		repo.storeErr = nil
		repo.mu.Unlock()
		b.Record(click(2))
		synctest.Wait()
		if got := repo.sizes(); !equalInts(got, []int{1}) {
			t.Fatalf("batches = %v, want [1]: the poisoned batch is gone, the next lands alone", got)
		}
		if repo.batches[0][0].LinkID != 2 {
			t.Errorf("stored click %d, want 2", repo.batches[0][0].LinkID)
		}
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBufferCapsHeldBatchWhileRetrying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{storeErr: errors.New("db down")}
		b := NewBuffer(repo, discard, BufferConfig{Size: 3, FlushInterval: time.Second, MaxBatch: 1})

		b.Record(click(1)) // flush fails, batch held with attempts=1
		synctest.Wait()
		for i := 2; i <= 5; i++ {
			b.Record(click(int64(i)))
			synctest.Wait()
		}
		// Held batch is capped at Size (3): clicks 4 and 5 are dropped.
		repo.mu.Lock()
		repo.storeErr = nil
		repo.mu.Unlock()
		synctest.Sleep(time.Second)
		if got := repo.sizes(); !equalInts(got, []int{3}) {
			t.Fatalf("batches = %v, want [3]", got)
		}
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBufferCloseIsIdempotentAndWorksWithoutTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{}
		idle := NewBuffer(repo, discard, BufferConfig{})
		if err := idle.Close(t.Context()); err != nil {
			t.Fatalf("Close on an idle buffer = %v", err)
		}

		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Hour})
		b.Record(click(1))
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := b.Close(t.Context()); err != nil {
			t.Fatalf("second Close = %v", err)
		}
		if repo.total() != 1 {
			t.Errorf("stored %d, want 1", repo.total())
		}
	})
}

func TestBufferCloseHonoursContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{block: make(chan struct{})}
		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Hour, MaxBatch: 1})
		b.Record(click(1))
		synctest.Wait() // flusher is stuck in Store

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := b.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close = %v, want deadline exceeded", err)
		}
		close(repo.block) // let the goroutine finish so the bubble can exit
		<-b.done
	})
}

func TestBufferSanitisesHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{}
		b := NewBuffer(repo, discard, BufferConfig{FlushInterval: time.Hour, MaxBatch: 1})
		// "a" followed by two-byte runes puts every rune boundary at an odd
		// offset, so a naive cut at the even limit would split a rune.
		b.Record(link.Click{LinkID: 1, Referrer: "https://ref.example/\xff\x00x", UserAgent: "a" + strings.Repeat("é", MaxUserAgentLen)})
		if err := b.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := repo.batches[0][0]
		if got.Referrer != "https://ref.example/x" {
			t.Errorf("referrer = %q", got.Referrer)
		}
		if got.LinkID != 1 {
			t.Errorf("link id = %d", got.LinkID)
		}
		if len(got.UserAgent) != MaxUserAgentLen-1 || !strings.HasSuffix(got.UserAgent, "é") {
			t.Errorf("user agent len = %d, want %d ending on a whole rune", len(got.UserAgent), MaxUserAgentLen-1)
		}
	})
}

func TestSanitizeReferrer(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"https://ref.example/page?token=s#f": "https://ref.example/page",
		"http://user:pw@ref.example/p":       "http://ref.example/p",
		"https://ref.example":                "https://ref.example",
		"ftp://ref.example/x":                "",
		"/relative/path":                     "",
		"not a url at all ://":               "",
	}
	for in, want := range cases {
		if got := sanitizeReferrer(in); got != want {
			t.Errorf("sanitizeReferrer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]struct {
		in, want string
		maxLen   int
	}{
		"empty":        {"", "", 10},
		"short":        {"abc", "abc", 10},
		"exact":        {"abcde", "abcde", 5},
		"truncate":     {"abcdef", "abcde", 5},
		"nul removed":  {"a\x00b", "ab", 10},
		"invalid utf8": {"a\xffb", "ab", 10},
		"rune boundary": {
			"aé", "a", 2, // 'é' needs 2 bytes starting at index 1; cutting at 2 would split it
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := sanitize(tc.in, tc.maxLen); got != tc.want {
				t.Errorf("sanitize(%q, %d) = %q, want %q", tc.in, tc.maxLen, got, tc.want)
			}
		})
	}
}
