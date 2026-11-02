package click

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 11, 2, 15, 30, 0, 0, time.UTC) }

func TestStatsZeroFillsSevenDays(t *testing.T) {
	repo := &fakeRepo{days: []DayCount{
		{Day: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Clicks: 4},
		{Day: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Clicks: 1},
	}}
	svc := NewService(repo, discard, WithNow(fixedNow))

	st, err := svc.Stats(t.Context(), 7, 123)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 123 {
		t.Errorf("total = %d, want 123 (taken from the link row)", st.Total)
	}
	if len(st.Days) != StatsDays {
		t.Fatalf("days = %d, want %d", len(st.Days), StatsDays)
	}
	wantFirst := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for i, d := range st.Days {
		if want := wantFirst.AddDate(0, 0, i); !d.Day.Equal(want) {
			t.Errorf("days[%d].Day = %v, want %v", i, d.Day, want)
		}
	}
	if st.Days[1].Clicks != 4 || st.Days[6].Clicks != 1 || st.Days[0].Clicks != 0 || st.Days[5].Clicks != 0 {
		t.Errorf("counts = %+v", st.Days)
	}
	if !repo.gotSince.Equal(wantFirst) || !repo.gotUntil.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window = [%v, %v)", repo.gotSince, repo.gotUntil)
	}
}

func TestStatsWindowUsesUTCDay(t *testing.T) {
	// 23:30 in UTC-5 is already the next UTC day; the window must follow UTC.
	local := time.FixedZone("UTC-5", -5*3600)
	now := time.Date(2026, 11, 2, 23, 30, 0, 0, local)
	repo := &fakeRepo{}
	svc := NewService(repo, discard, WithNow(func() time.Time { return now }))

	st, err := svc.Stats(t.Context(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if last := st.Days[StatsDays-1].Day; !last.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last day = %v, want 2026-11-03 UTC", last)
	}
}

func TestStatsPropagatesRepoError(t *testing.T) {
	want := errors.New("boom")
	svc := NewService(&fakeRepo{daysErr: want}, discard, WithNow(fixedNow))
	if _, err := svc.Stats(t.Context(), 1, 0); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestPurgeLoopsUntilShortBatch(t *testing.T) {
	repo := &fakeRepo{deleteSizes: []int64{int64(purgeBatch), int64(purgeBatch), 12}}
	svc := NewService(repo, discard, WithNow(fixedNow))

	n, err := svc.Purge(t.Context(), 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2*int64(purgeBatch)+12 {
		t.Errorf("deleted = %d", n)
	}
	if len(repo.cutoffs) != 3 {
		t.Fatalf("DeleteBefore calls = %d, want 3", len(repo.cutoffs))
	}
	if want := fixedNow().Add(-90 * 24 * time.Hour); !repo.cutoffs[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", repo.cutoffs[0], want)
	}
}

func TestPurgeReturnsPartialCountOnError(t *testing.T) {
	repo := &fakeRepo{deleteErr: errors.New("lock timeout")}
	svc := NewService(repo, discard, WithNow(fixedNow))
	n, err := svc.Purge(t.Context(), time.Hour)
	if err == nil || n != 0 {
		t.Fatalf("Purge = %d, %v; want 0 and an error", n, err)
	}
}

func TestRunPurgerUnderFakeClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{}
		svc := NewService(repo, discard) // default clock: the bubble's fake time

		ctx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() { errCh <- svc.RunPurger(ctx, time.Hour, time.Hour) }()

		synctest.Sleep(purgeInitialDelay - time.Second)
		if n := repo.purgeCalls(); n != 0 {
			t.Fatalf("purged before the initial delay: %d calls", n)
		}
		synctest.Sleep(time.Second)
		if n := repo.purgeCalls(); n != 1 {
			t.Fatalf("after initial delay: %d calls, want 1", n)
		}
		synctest.Sleep(time.Hour)
		if n := repo.purgeCalls(); n != 2 {
			t.Fatalf("after one interval: %d calls, want 2", n)
		}

		// A failing purge is logged and the loop keeps going.
		repo.setDeleteErr(errors.New("transient"))
		synctest.Sleep(time.Hour)
		repo.setDeleteErr(nil)
		synctest.Sleep(time.Hour)
		if n := repo.purgeCalls(); n != 4 {
			t.Fatalf("after a failure: %d calls, want 4", n)
		}

		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("RunPurger = %v, want nil on cancellation", err)
		}
	})
}

func TestRunPurgerStopsDuringInitialDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &fakeRepo{}
		svc := NewService(repo, discard)
		ctx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() { errCh <- svc.RunPurger(ctx, time.Hour, time.Hour) }()
		synctest.Wait()
		cancel()
		if err := <-errCh; err != nil || repo.purgeCalls() != 0 {
			t.Fatalf("RunPurger = %v, calls = %d", err, repo.purgeCalls())
		}
	})
}

func TestRunPurgerRejectsNonPositiveInterval(t *testing.T) {
	svc := NewService(&fakeRepo{}, discard)
	if err := svc.RunPurger(t.Context(), time.Hour, 0); err == nil {
		t.Fatal("expected an error for a zero interval")
	}
}
