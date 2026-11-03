package link

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/vanthao03596/go-backend/internal/platform/otelx/otelxtest"
)

func TestCreateCountsLinksAndCollisions(t *testing.T) {
	rec := otelxtest.New(t)
	repo := newFakeRepo()
	svc := newTestService(repo, WithMeterProvider(rec.MeterProvider))
	ctx := t.Context()

	// Two collisions, then a generated code goes in.
	repo.insertErrs = []error{ErrCodeTaken, ErrCodeTaken}
	if _, err := svc.Create(ctx, 1, CreateParams{URL: "https://example.com/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, 1, CreateParams{URL: "https://example.com/b", Alias: "my-alias"}); err != nil {
		t.Fatal(err)
	}
	// A taken alias is a failed create and must not count.
	if _, err := svc.Create(ctx, 1, CreateParams{URL: "https://example.com/c", Alias: "my-alias"}); err == nil {
		t.Fatal("reusing an alias succeeded")
	}

	if got := rec.Sum(t, "shortener.links.code_collisions"); got != 2 {
		t.Errorf("code_collisions = %d, want 2", got)
	}
	if got := rec.Sum(t, "shortener.links.created", attrCustomAlias.Bool(false)); got != 1 {
		t.Errorf("created{custom_alias=false} = %d, want 1", got)
	}
	if got := rec.Sum(t, "shortener.links.created", attrCustomAlias.Bool(true)); got != 1 {
		t.Errorf("created{custom_alias=true} = %d, want 1", got)
	}
}

func TestRedirectsAreCountedByStatus(t *testing.T) {
	rec := otelxtest.New(t)
	repo := newFakeRepo()
	svc := newTestService(repo, WithMeterProvider(rec.MeterProvider))
	h := NewHandler(svc, slog.New(slog.DiscardHandler))
	root := chi.NewRouter()
	root.Route("/api/v1", func(api chi.Router) {
		h.Register(root, humachi.New(api, huma.DefaultConfig("test", "0")))
	})

	live, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/live"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com/gone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(t.Context(), 1, gone.Code); err != nil {
		t.Fatal(err)
	}

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/"+live.Code, nil),
		httptest.NewRequest(http.MethodHead, "/"+live.Code, nil),
		httptest.NewRequest(http.MethodGet, "/"+gone.Code, nil),
		httptest.NewRequest(http.MethodGet, "/missing", nil),
	} {
		root.ServeHTTP(httptest.NewRecorder(), req)
	}

	for status, want := range map[int]int64{
		http.StatusFound:               2, // HEAD answers 302 too, without counting a click
		http.StatusGone:                1,
		http.StatusNotFound:            1,
		http.StatusInternalServerError: 0,
	} {
		if got := rec.Sum(t, "shortener.redirects", semconv.HTTPResponseStatusCode(status)); got != want {
			t.Errorf("redirects{status=%d} = %d, want %d", status, got, want)
		}
	}
}

// A status without a prebuilt attribute set must still be counted, not panic
// on a nil option.
func TestCountRedirectHandlesUnlistedStatus(t *testing.T) {
	rec := otelxtest.New(t)
	svc := newTestService(newFakeRepo(), WithMeterProvider(rec.MeterProvider))
	h := NewHandler(svc, slog.New(slog.DiscardHandler))

	h.countRedirect(t.Context(), http.StatusBadRequest)

	if got := rec.Sum(t, "shortener.redirects", semconv.HTTPResponseStatusCode(http.StatusBadRequest)); got != 1 {
		t.Errorf("redirects{status=400} = %d, want 1", got)
	}
}
