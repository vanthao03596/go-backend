package click

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/link"
)

// fakeLinks is the smallest link.Repository that lets link.Service answer
// ownership checks: one link, owned by key 1, with a fixed counter.
type fakeLinks struct {
	l link.Link
}

func (f fakeLinks) Insert(context.Context, string, string, int64, *time.Time) (link.Link, error) {
	return link.Link{}, errors.New("not used")
}

func (f fakeLinks) GetByCode(_ context.Context, code string) (link.Link, error) {
	if code != f.l.Code {
		return link.Link{}, link.ErrNotFound
	}
	return f.l, nil
}

func (f fakeLinks) GetByCodeForKey(_ context.Context, code string, keyID int64) (link.Link, error) {
	if code != f.l.Code || keyID != f.l.APIKeyID {
		return link.Link{}, link.ErrNotFound
	}
	return f.l, nil
}

func (f fakeLinks) List(context.Context, int64, link.Cursor, int32) ([]link.Link, error) {
	return nil, nil
}

func (f fakeLinks) SoftDelete(context.Context, string, int64) error { return link.ErrNotFound }

// asKey stands in for the bearer middleware and acts as api_keys.id keyID.
func asKey(keyID int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(apikey.WithID(r.Context(), keyID)))
		})
	}
}

func newStatsRouter(t *testing.T, repo Repository, keyID int64) http.Handler {
	t.Helper()
	owned := link.Link{ID: 42, Code: "abc1234", TargetURL: "https://example.com", APIKeyID: 1, ClickCount: 99}
	links := link.NewService(fakeLinks{l: owned}, "https://sho.rt", discard)
	h := NewHandler(NewService(repo, discard, WithNow(fixedNow)), links, discard)

	root := chi.NewRouter()
	root.Route("/api/v1", func(api chi.Router) {
		if keyID != 0 {
			api.Use(asKey(keyID))
		}
		h.Register(root, humachi.New(api, huma.DefaultConfig("test", "0")))
	})
	return root
}

func TestStatsEndpoint(t *testing.T) {
	repo := &fakeRepo{days: []DayCount{{Day: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Clicks: 5}}}
	r := newStatsRouter(t, repo, 1)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/links/abc1234/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Code        string `json:"code"`
		TotalClicks int64  `json:"total_clicks"`
		Days        []struct {
			Date   string `json:"date"`
			Clicks int64  `json:"clicks"`
		} `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if body.Code != "abc1234" || body.TotalClicks != 99 {
		t.Errorf("code=%q total=%d", body.Code, body.TotalClicks)
	}
	if len(body.Days) != StatsDays {
		t.Fatalf("days = %d, want %d", len(body.Days), StatsDays)
	}
	if body.Days[0].Date != "2026-10-27" || body.Days[6].Date != "2026-11-02" || body.Days[6].Clicks != 5 {
		t.Errorf("days = %+v", body.Days)
	}
}

func TestStatsIsScopedToOwner(t *testing.T) {
	r := newStatsRouter(t, &fakeRepo{}, 2)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/links/abc1234/stats", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another key's link", rec.Code)
	}
}

func TestStatsRequiresKey(t *testing.T) {
	r := newStatsRouter(t, &fakeRepo{}, 0)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/links/abc1234/stats", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestStatsMasksInternalErrors(t *testing.T) {
	r := newStatsRouter(t, &fakeRepo{daysErr: errors.New("SQLSTATE 57P01")}, 1)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/links/abc1234/stats", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SQLSTATE") {
		t.Errorf("internal error leaked: %s", rec.Body.String())
	}
}
