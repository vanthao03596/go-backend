package link

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/server/middleware"
)

// failingRepo wraps fakeRepo and fails GetByCodeForKey with an opaque error.
type failingRepo struct {
	*fakeRepo
}

func (f failingRepo) GetByCodeForKey(context.Context, string, int64) (Link, error) {
	return Link{}, errors.New("get link for key: SQLSTATE 57P01 terminating connection")
}

// asKey stands in for the bearer middleware and acts as api_keys.id 1.
func asKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(apikey.WithID(r.Context(), 1)))
	})
}

func newTestRouter(t *testing.T, repo Repository, opts ...HandlerOption) chi.Router {
	t.Helper()
	svc := newTestService(repo)
	h := NewHandler(svc, slog.New(slog.DiscardHandler), opts...)

	root := chi.NewRouter()
	root.Use(chimw.ClientIPFromRemoteAddr)
	root.Route("/api/v1", func(api chi.Router) {
		api.Use(asKey)
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

func TestRedirectRateLimit(t *testing.T) {
	repo := newFakeRepo()
	l := seedLink(t, repo)
	r := newTestRouter(t, repo, WithRedirectRateLimit(middleware.RateLimitByIP(3)))

	codes := make([]int, 0, 4)
	for range 4 {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+l.Code, nil))
		codes = append(codes, rec.Code)
	}
	want := []int{302, 302, 302, 429}
	for i, c := range codes {
		if c != want[i] {
			t.Fatalf("request %d = %d, want %d (all: %v)", i+1, c, want[i], codes)
		}
	}

	// Health endpoints share the router but not the limiter.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/links/"+l.Code, nil))
	if rec.Code == http.StatusTooManyRequests {
		t.Error("management API was throttled by the redirect limiter")
	}
}

func TestCreateRateLimitOnlyMatchesCreate(t *testing.T) {
	var hits int
	h := chimw.ClientIPFromRemoteAddr(CreateRateLimit(middleware.RateLimitByIP(1))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.WriteHeader(http.StatusNoContent)
		})))
	serve := func(method, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code
	}

	if got := serve(http.MethodPost, "/api/v1/links"); got != http.StatusNoContent {
		t.Fatalf("first POST = %d, want 204", got)
	}
	if got := serve(http.MethodPost, "/api/v1/links"); got != http.StatusTooManyRequests {
		t.Fatalf("second POST = %d, want 429", got)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/links"},
		{http.MethodGet, "/api/v1/links/abc1234"},
		{http.MethodDelete, "/api/v1/links/abc1234"},
	} {
		if got := serve(c.method, c.path); got != http.StatusNoContent {
			t.Errorf("%s %s = %d, want 204 (should not be rate limited)", c.method, c.path, got)
		}
	}
	if hits != 4 {
		t.Errorf("handler hits = %d, want 4", hits)
	}
}

func TestRateLimitRejectsUnresolvedClientIP(t *testing.T) {
	// No ClientIP resolver in the chain: the limiter must not pool these
	// requests into one shared bucket.
	h := middleware.RateLimitByIP(100)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestRedirectExpiresUnderFakeClock drives the real HTTP stack over
// httptest's in-memory network inside a synctest bubble. The service keeps
// its default time.Now, which the bubble fakes, so the hour passes instantly.
func TestRedirectExpiresUnderFakeClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := newFakeRepo()
		svc := NewService(repo, "https://sho.rt", slog.New(slog.DiscardHandler))
		h := NewHandler(svc, slog.New(slog.DiscardHandler))

		root := chi.NewRouter()
		root.Route("/api/v1", func(api chi.Router) {
			api.Use(asKey)
			h.Register(root, humachi.New(api, huma.DefaultConfig("test", "0")))
		})
		ts := httptest.NewTestServer(t, root)
		client := ts.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

		exp := time.Now().Add(time.Hour)
		l, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/t", ExpiresAt: &exp})
		if err != nil {
			t.Fatal(err)
		}

		status := func() int {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/"+l.Code, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			return resp.StatusCode
		}

		if got := status(); got != http.StatusFound {
			t.Fatalf("before expiry = %d, want 302", got)
		}
		synctest.Sleep(time.Hour + time.Second)
		if got := status(); got != http.StatusGone {
			t.Fatalf("after expiry = %d, want 410", got)
		}
	})
}
