package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/vanthao03596/go-backend/internal/config"
	"github.com/vanthao03596/go-backend/internal/server/middleware"
)

func newTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	cfg := config.Config{Env: "test", Port: 0, RequestTimeout: time.Second, ShutdownTimeout: time.Second}
	log := slog.New(slog.DiscardHandler)
	return New(cfg, log, opts...)
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(middleware.HeaderRequestID); got == "" {
		t.Error("missing X-Request-Id header")
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestReadyzReportsFailingDependency(t *testing.T) {
	srv := newTestServer(t, WithReadiness("db", func(context.Context) error {
		return errors.New("down")
	}))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "db unavailable") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestRequestIDIsPropagated(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(middleware.HeaderRequestID, "trace-42")
	srv.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get(middleware.HeaderRequestID); got != "trace-42" {
		t.Errorf("X-Request-Id = %q, want trace-42", got)
	}
}

func TestAPIMiddlewareSkipsDocsRoutes(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
	srv := newTestServer(t, WithAPIMiddleware(deny))

	cases := map[string]int{
		"/api/v1/openapi.json":     http.StatusOK,
		"/api/v1/openapi.yaml":     http.StatusOK,
		"/api/v1/openapi-3.0.json": http.StatusOK,
		"/api/v1/openapi-3.0.yaml": http.StatusOK,
		"/api/v1/docs":             http.StatusOK,
		"/api/v1/schemas/X":        http.StatusOK,
		"/api/v1/links":            http.StatusUnauthorized,
		"/api/v1/openapi":          http.StatusUnauthorized,
	}
	for path, want := range cases {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestOpenAPIDeclaresBearerSecurity(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `"securitySchemes":{"bearerAuth":{`) || !strings.Contains(body, `"scheme":"bearer","type":"http"`) {
		t.Errorf("security scheme missing: %s", body)
	}
	if !strings.Contains(body, `"security":[{"bearerAuth":[]}]`) {
		t.Errorf("top-level security missing: %s", body)
	}
}

type schemaLinkOutput struct {
	Body struct {
		Name string `json:"name"`
	}
}

// TestSchemaLinksUseBaseURL pins the `$schema` URL to BASE_URL whatever the
// request says. Huma v2.40.0 built it from the request: a localhost Host
// doubled the /api/v1 prefix, a plain-HTTP LAN host got https, and any client
// could pick the host with X-Forwarded-Host.
func TestSchemaLinksUseBaseURL(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
	feature := WithFeature(func(_ chi.Router, api huma.API) {
		huma.Get(api, "/thing", func(context.Context, *struct{}) (*schemaLinkOutput, error) {
			return &schemaLinkOutput{}, nil
		})
		huma.Get(api, "/broken", func(context.Context, *struct{}) (*schemaLinkOutput, error) {
			return nil, huma.Error404NotFound("gone")
		})
	})
	// deny stands in for auth on every route except the two above, so
	// following the link proves the schema route is public.
	authOnOthers := WithAPIMiddleware(chimw.Maybe(deny, func(r *http.Request) bool {
		return r.URL.Path != APIBasePath+"/thing" && r.URL.Path != APIBasePath+"/broken"
	}))

	cases := []struct {
		name   string
		base   string
		host   string
		header http.Header
	}{
		{name: "localhost host, localhost base", base: "http://localhost:8080", host: "localhost:8080"},
		{name: "localhost host, public base", base: "https://sho.rt", host: "localhost:8080"},
		{name: "plain-HTTP LAN host", base: "http://192.168.1.5:8080", host: "192.168.1.5:8080"},
		{name: "loopback host", base: "https://sho.rt", host: "127.0.0.1:8080"},
		{name: "spoofed X-Forwarded-Host", base: "https://sho.rt", host: "sho.rt", header: http.Header{"X-Forwarded-Host": {"evil.example"}}},
		{name: "spoofed Forwarded", base: "https://sho.rt", host: "sho.rt", header: http.Header{"Forwarded": {"host=evil.example"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Env: "test", BaseURL: tc.base, RequestTimeout: time.Second, ShutdownTimeout: time.Second}
			srv := New(cfg, slog.New(slog.DiscardHandler), feature, authOnOthers)

			for path, schema := range map[string]string{"/thing": "SchemaLinkOutputBody", "/broken": "ErrorModel"} {
				req := httptest.NewRequest(http.MethodGet, APIBasePath+path, nil)
				req.Host = tc.host
				maps.Copy(req.Header, tc.header)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)

				var body struct {
					Schema string `json:"$schema"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("GET %s: decode %q: %v", path, rec.Body.String(), err)
				}
				want := tc.base + APIBasePath + "/schemas/" + schema + ".json"
				if body.Schema != want {
					t.Errorf("GET %s: $schema = %q, want %q", path, body.Schema, want)
				}
				// The Link header was already right: relative, resolved
				// against the request URL. It must stay that way.
				if got, wantLink := rec.Header().Get("Link"), `<`+APIBasePath+`/schemas/`+schema+`.json>; rel="describedBy"`; got != wantLink {
					t.Errorf("GET %s: Link = %q, want %q", path, got, wantLink)
				}

				// The link resolves, without credentials, on this server.
				u, err := url.Parse(body.Schema)
				if err != nil {
					t.Fatalf("parse %q: %v", body.Schema, err)
				}
				follow := httptest.NewRecorder()
				srv.Handler().ServeHTTP(follow, httptest.NewRequest(http.MethodGet, u.Path, nil))
				if follow.Code != http.StatusOK {
					t.Errorf("GET %s = %d, want 200", u.Path, follow.Code)
				}
			}
		})
	}
}

func TestRunStartsWorkersAndRunsHooksOnShutdown(t *testing.T) {
	workerStarted := make(chan struct{})
	workerStopped := make(chan struct{})
	hookRan := make(chan struct{})
	srv := newTestServer(t,
		WithWorker("w", func(ctx context.Context) error {
			close(workerStarted)
			<-ctx.Done()
			close(workerStopped)
			return ctx.Err()
		}),
		WithShutdownHook("h", func(context.Context) error {
			// Workers must already be stopped when hooks run.
			select {
			case <-workerStopped:
			default:
				t.Error("hook ran before the worker stopped")
			}
			close(hookRan)
			return nil
		}),
	)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	select {
	case <-workerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	select {
	case <-hookRan:
	default:
		t.Error("shutdown hook did not run")
	}
}

func TestShutdownWithoutRunStillRunsHooks(t *testing.T) {
	ran := false
	want := errors.New("flush failed")
	srv := newTestServer(t,
		WithWorker("never started", func(ctx context.Context) error { <-ctx.Done(); return nil }),
		WithShutdownHook("h", func(context.Context) error { ran = true; return want }),
	)
	err := srv.Shutdown(t.Context())
	if !ran {
		t.Fatal("hook did not run")
	}
	if !errors.Is(err, want) {
		t.Fatalf("Shutdown = %v, want wrapped %v", err, want)
	}
}
