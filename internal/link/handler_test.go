package link

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/thaopv/go-backend/internal/apikey"
)

// failingRepo wraps fakeRepo and fails GetByCodeForKey with an opaque error.
type failingRepo struct {
	*fakeRepo
}

func (f failingRepo) GetByCodeForKey(context.Context, string, int64) (Link, error) {
	return Link{}, errors.New("get link for key: SQLSTATE 57P01 terminating connection")
}

func newTestRouter(t *testing.T, repo Repository) chi.Router {
	t.Helper()
	svc := newTestService(repo)
	h := NewHandler(svc, slog.New(slog.DiscardHandler))

	root := chi.NewRouter()
	root.Route("/api/v1", func(api chi.Router) {
		api.Use(apikey.Static(1))
		h.Register(root, humachi.New(api, huma.DefaultConfig("test", "0")))
	})
	return root
}

func seedLink(t *testing.T, repo *fakeRepo) Link {
	t.Helper()
	svc := newTestService(repo)
	l, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/target"})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRedirectGetCountsClick(t *testing.T) {
	repo := newFakeRepo()
	l := seedLink(t, repo)
	r := newTestRouter(t, repo)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+l.Code, nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://example.com/target" {
		t.Errorf("Location = %q", loc)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if repo.links[l.Code].ClickCount != 1 {
		t.Errorf("click_count = %d, want 1", repo.links[l.Code].ClickCount)
	}
}

func TestRedirectHeadDoesNotCount(t *testing.T) {
	repo := newFakeRepo()
	l := seedLink(t, repo)
	r := newTestRouter(t, repo)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/"+l.Code, nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("HEAD status = %d, want 302", rec.Code)
	}
	if repo.links[l.Code].ClickCount != 0 {
		t.Errorf("click_count = %d after HEAD, want 0", repo.links[l.Code].ClickCount)
	}
}

func TestRedirectStatuses(t *testing.T) {
	repo := newFakeRepo()
	l := seedLink(t, repo)
	if err := repo.SoftDelete(t.Context(), l.Code, 1); err != nil {
		t.Fatal(err)
	}
	r := newTestRouter(t, repo)

	for path, want := range map[string]int{"/" + l.Code: http.StatusGone, "/nope404": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestInternalErrorsAreMasked(t *testing.T) {
	r := newTestRouter(t, failingRepo{newFakeRepo()})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/links/abc1234", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "SQLSTATE") || strings.Contains(body, "terminating connection") {
		t.Errorf("internal error text leaked to client: %s", body)
	}
	if !strings.Contains(body, `"status":500`) || !strings.Contains(body, "internal error") {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestCreateReturns201WithLocation(t *testing.T) {
	r := newTestRouter(t, newFakeRepo())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/x","alias":"promo"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://sho.rt/promo" {
		t.Errorf("Location = %q", loc)
	}
	if !strings.Contains(rec.Body.String(), `"created_at":"`) || strings.Contains(rec.Body.String(), "+07:00") {
		t.Errorf("timestamps should be UTC: %s", rec.Body.String())
	}
}
