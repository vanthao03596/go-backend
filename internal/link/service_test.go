package link

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// fakeRepo is an in-memory Repository for service tests.
type fakeRepo struct {
	links      map[string]Link
	nextID     int64
	insertErrs []error // consumed in order; nil means success
}

// recordingSink captures every click the service emits.
type recordingSink struct {
	clicks []Click
}

func (r *recordingSink) Record(c Click) { r.clicks = append(r.clicks, c) }

func newFakeRepo() *fakeRepo { return &fakeRepo{links: map[string]Link{}} }

func (f *fakeRepo) Insert(_ context.Context, code, target string, keyID int64, exp *time.Time) (Link, error) {
	if len(f.insertErrs) > 0 {
		err := f.insertErrs[0]
		f.insertErrs = f.insertErrs[1:]
		if err != nil {
			return Link{}, err
		}
	}
	if _, dup := f.links[code]; dup {
		return Link{}, ErrCodeTaken
	}
	f.nextID++
	l := Link{ID: f.nextID, Code: code, TargetURL: target, APIKeyID: keyID, ExpiresAt: exp, CreatedAt: time.Unix(f.nextID, 0)}
	f.links[code] = l
	return l, nil
}

func (f *fakeRepo) GetByCode(_ context.Context, code string) (Link, error) {
	l, ok := f.links[code]
	if !ok {
		return Link{}, ErrNotFound
	}
	return l, nil
}

func (f *fakeRepo) GetByCodeForKey(_ context.Context, code string, keyID int64) (Link, error) {
	l, ok := f.links[code]
	if !ok || l.APIKeyID != keyID || l.DeletedAt != nil {
		return Link{}, ErrNotFound
	}
	return l, nil
}

func (f *fakeRepo) List(_ context.Context, keyID int64, cursor Cursor, limit int32) ([]Link, error) {
	var out []Link
	for _, l := range f.links {
		if l.APIKeyID != keyID || l.DeletedAt != nil {
			continue
		}
		if l.CreatedAt.After(cursor.CreatedAt) || (l.CreatedAt.Equal(cursor.CreatedAt) && l.ID >= cursor.ID) {
			continue
		}
		out = append(out, l)
	}
	// newest first
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) SoftDelete(_ context.Context, code string, keyID int64) error {
	l, ok := f.links[code]
	if !ok || l.APIKeyID != keyID || l.DeletedAt != nil {
		return ErrNotFound
	}
	now := time.Now()
	l.DeletedAt = &now
	f.links[code] = l
	return nil
}

func newTestService(repo Repository, opts ...ServiceOption) *Service {
	s := NewService(repo, "https://sho.rt/", slog.New(slog.DiscardHandler), opts...)
	s.now = func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestCreateGeneratesCode(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)

	l, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Code) != CodeLength {
		t.Errorf("code %q has length %d", l.Code, len(l.Code))
	}
	if got, want := svc.ShortURL(l.Code), "https://sho.rt/"+l.Code; got != want {
		t.Errorf("ShortURL = %q, want %q", got, want)
	}
}

func TestCreateRetriesOnCollision(t *testing.T) {
	repo := newFakeRepo()
	repo.insertErrs = []error{ErrCodeTaken, ErrCodeTaken, nil}
	svc := newTestService(repo)

	if _, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com"}); err != nil {
		t.Fatalf("expected success after two collisions, got %v", err)
	}
}

func TestCreateGivesUpAfterMaxAttempts(t *testing.T) {
	repo := newFakeRepo()
	repo.insertErrs = []error{ErrCodeTaken, ErrCodeTaken, ErrCodeTaken}
	svc := newTestService(repo)

	_, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com"})
	if err == nil || errors.Is(err, ErrCodeTaken) {
		t.Fatalf("want a non-ErrCodeTaken failure after %d attempts, got %v", maxCodeAttempts, err)
	}
}

func TestCreateWithAliasDoesNotRetry(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	if _, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com", Alias: "promo"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(t.Context(), 2, CreateParams{URL: "https://example.org", Alias: "promo"})
	if !errors.Is(err, ErrCodeTaken) {
		t.Fatalf("second alias insert = %v, want ErrCodeTaken", err)
	}
}

func TestCreateValidation(t *testing.T) {
	svc := newTestService(newFakeRepo())
	past := svc.now().Add(-time.Minute)
	future := svc.now().Add(time.Minute)

	cases := map[string]struct {
		p    CreateParams
		want error
	}{
		"bad url":        {CreateParams{URL: "notaurl"}, ErrInvalidURL},
		"reserved alias": {CreateParams{URL: "https://example.com", Alias: "api"}, ErrReservedAlias},
		"short alias":    {CreateParams{URL: "https://example.com", Alias: "ab"}, ErrInvalidAlias},
		"past expiry":    {CreateParams{URL: "https://example.com", ExpiresAt: &past}, ErrInvalidExpiry},
		"future expiry":  {CreateParams{URL: "https://example.com", ExpiresAt: &future}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Create(t.Context(), 1, tc.p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Create = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	repo := newFakeRepo()
	sink := &recordingSink{}
	svc := newTestService(repo, WithClickSink(sink))
	ctx := t.Context()

	live, _ := svc.Create(ctx, 1, CreateParams{URL: "https://example.com/live"})
	exp := svc.now().Add(time.Hour)
	expiring, _ := svc.Create(ctx, 1, CreateParams{URL: "https://example.com/exp", ExpiresAt: &exp})
	deleted, _ := svc.Create(ctx, 1, CreateParams{URL: "https://example.com/del"})
	if err := svc.Delete(ctx, 1, deleted.Code); err != nil {
		t.Fatal(err)
	}

	visit := &Visit{Referrer: "https://ref.example/", UserAgent: "test/1.0"}
	target, err := svc.Resolve(ctx, live.Code, visit)
	if err != nil || target != "https://example.com/live" {
		t.Fatalf("Resolve live = %q, %v", target, err)
	}
	want := Click{LinkID: live.ID, OccurredAt: svc.now(), Referrer: visit.Referrer, UserAgent: visit.UserAgent}
	if len(sink.clicks) != 1 || sink.clicks[0] != want {
		t.Errorf("recorded clicks = %+v, want [%+v]", sink.clicks, want)
	}

	if _, err := svc.Resolve(ctx, "nope123", visit); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown code = %v, want ErrNotFound", err)
	}
	if _, err := svc.Resolve(ctx, deleted.Code, visit); !errors.Is(err, ErrGone) {
		t.Errorf("deleted = %v, want ErrGone", err)
	}

	// Move the clock past the expiry.
	svc.now = func() time.Time { return exp.Add(time.Second) }
	if _, err := svc.Resolve(ctx, expiring.Code, visit); !errors.Is(err, ErrGone) {
		t.Errorf("expired = %v, want ErrGone", err)
	}
	if len(sink.clicks) != 1 {
		t.Errorf("failed resolves must not record clicks, got %d", len(sink.clicks))
	}
}

func TestResolveWithoutVisitDoesNotRecord(t *testing.T) {
	repo := newFakeRepo()
	sink := &recordingSink{}
	svc := newTestService(repo, WithClickSink(sink))
	l, _ := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com"})

	if _, err := svc.Resolve(t.Context(), l.Code, nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.clicks) != 0 {
		t.Errorf("clicks = %d, want 0 for HEAD-style resolve", len(sink.clicks))
	}
}

func TestResolveWithoutSinkStillRedirects(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	l, _ := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com"})

	target, err := svc.Resolve(t.Context(), l.Code, &Visit{})
	if err != nil || target != "https://example.com" {
		t.Fatalf("Resolve = %q, %v; redirect must not depend on a sink", target, err)
	}
}

func TestGetAndDeleteAreScopedToKey(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	ctx := t.Context()
	l, _ := svc.Create(ctx, 1, CreateParams{URL: "https://example.com"})

	if _, err := svc.Get(ctx, 2, l.Code); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get by other key = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, 2, l.Code); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete by other key = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, 1, l.Code); err != nil {
		t.Errorf("Delete by owner = %v", err)
	}
	if err := svc.Delete(ctx, 1, l.Code); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestListPaginates(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	ctx := t.Context()
	for range 5 {
		if _, err := svc.Create(ctx, 1, CreateParams{URL: "https://example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = svc.Create(ctx, 2, CreateParams{URL: "https://example.com/other"})

	page1, next, err := svc.List(ctx, 1, "", 2)
	if err != nil || len(page1) != 2 || next == "" {
		t.Fatalf("page1: n=%d next=%q err=%v", len(page1), next, err)
	}
	page2, next2, err := svc.List(ctx, 1, next, 2)
	if err != nil || len(page2) != 2 || next2 == "" {
		t.Fatalf("page2: n=%d next=%q err=%v", len(page2), next2, err)
	}
	page3, next3, err := svc.List(ctx, 1, next2, 2)
	if err != nil || len(page3) != 1 || next3 != "" {
		t.Fatalf("page3: n=%d next=%q err=%v", len(page3), next3, err)
	}
	if page1[0].ID <= page1[1].ID || page1[1].ID <= page2[0].ID {
		t.Error("list is not newest-first across pages")
	}

	if _, _, err := svc.List(ctx, 1, "!!not-base64!!", 2); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("bad cursor = %v, want ErrInvalidCursor", err)
	}
	if _, _, err := svc.List(ctx, 1, "bm90LWEtY3Vyc29y", 2); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("malformed cursor = %v, want ErrInvalidCursor", err)
	}
}

func TestListClampsLimit(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo)
	for range 3 {
		_, _ = svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com"})
	}
	rows, _, err := svc.List(t.Context(), 1, "", 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("limit 0 should use default: n=%d err=%v", len(rows), err)
	}
	rows, _, err = svc.List(t.Context(), 1, "", 10_000)
	if err != nil || len(rows) != 3 {
		t.Fatalf("huge limit should clamp: n=%d err=%v", len(rows), err)
	}
}
