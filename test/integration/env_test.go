//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/app"
	"github.com/vanthao03596/go-backend/internal/config"
	"github.com/vanthao03596/go-backend/internal/db/sqlc"
)

// flushInterval is how quickly the click buffer writes in tests. waitFor
// polls for a few multiples of it.
const flushInterval = 20 * time.Millisecond

// clock is a controllable time source anchored to the real clock. Rows get
// created_at from Postgres' now(), so staying close to wall time keeps the
// two consistent; Advance moves only the application's view of time.
type clock struct {
	offset atomic.Int64 // nanoseconds
}

func (c *clock) Now() time.Time {
	return time.Now().Add(time.Duration(c.offset.Load()))
}

func (c *clock) Advance(d time.Duration) {
	c.offset.Add(int64(d))
}

// env is one running server with its own clock. Each test builds its own so
// rate limiters and clocks never leak between tests.
type env struct {
	t      *testing.T
	base   string
	client *http.Client
	clock  *clock
}

// newEnv starts the full application on a loopback httptest server. The
// listener is created before the handler so BASE_URL can point at the real
// address and short URLs returned by the API are directly requestable.
func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	return newEnvWith(t, pool, mutate)
}

// newEnvWith is newEnv against a specific pool and with extra app options.
func newEnvWith(t *testing.T, p *pgxpool.Pool, mutate func(*config.Config), opts ...app.Option) *env {
	t.Helper()

	ts := httptest.NewUnstartedServer(nil)
	base := "http://" + ts.Listener.Addr().String()

	cfg := config.Config{
		Env:             "test",
		BaseURL:         base,
		RequestTimeout:  15 * time.Second,
		ShutdownTimeout: time.Second,
		// Generous defaults so ordinary tests never trip a limiter. The rate
		// limit test lowers them explicitly.
		RateLimit: config.RateLimitConfig{CreatePerSecond: 1000, RedirectPerSecond: 1000},
		// Fast flushes keep the eventually-consistent assertions quick. The
		// in-process purger is off; TestClickPurgeKeepsRecentRows drives
		// retention directly.
		Clicks: config.ClickConfig{
			BufferSize:    1000,
			FlushInterval: flushInterval,
			FlushBatch:    100,
			Retention:     90 * 24 * time.Hour,
			PurgeInterval: 0,
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}

	clk := &clock{}
	srv := app.New(cfg, logger, p, append([]app.Option{app.WithClock(clk.Now)}, opts...)...)
	// Cleanups run last-in first-out: stop traffic, then drain the buffer.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)

	client := ts.Client()
	// Redirects are the thing under test: inspect them, do not follow them.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &env{t: t, base: base, client: client, clock: clk}
}

// newKey issues an API key directly through the service and returns the raw
// bearer token.
func (e *env) newKey(name string) string {
	e.t.Helper()
	_, raw := e.newKeyWithID(name)
	return raw
}

// newKeyWithID is newKey that also returns the row id, for revocation.
func (e *env) newKeyWithID(name string) (int64, string) {
	e.t.Helper()
	svc := apikey.NewService(apikey.NewRepository(sqlc.New(pool)))
	k, raw, err := svc.Create(e.t.Context(), name)
	if err != nil {
		e.t.Fatalf("create api key: %v", err)
	}
	return k.ID, raw
}

// revokeKey marks a key revoked so it must stop authenticating.
func (e *env) revokeKey(id int64) {
	e.t.Helper()
	n, err := sqlc.New(pool).RevokeAPIKey(e.t.Context(), id)
	if err != nil || n != 1 {
		e.t.Fatalf("revoke api key %d: rows=%d err=%v", id, n, err)
	}
}

// response is a fully read HTTP response.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// do sends one request. key is the bearer token or empty for anonymous.
// body is encoded as JSON when non-nil.
func (e *env) do(method, path, key string, body any) response {
	e.t.Helper()
	auth := ""
	if key != "" {
		auth = "Bearer " + key
	}
	return e.doAuth(method, path, auth, body)
}

// doAuth is do with a verbatim Authorization header value; empty sends none.
func (e *env) doAuth(method, path, authorization string, body any) response {
	e.t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		payload = bytes.NewReader(raw)
	}
	req := e.newRequest(method, path, payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return e.send(req)
}

// request builds a bodiless request with an optional bearer key so callers
// can add headers before send.
func (e *env) request(method, path, key string) *http.Request {
	e.t.Helper()
	req := e.newRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func (e *env) newRequest(method, path string, body io.Reader) *http.Request {
	e.t.Helper()
	url := path
	if !strings.HasPrefix(path, "http") {
		url = e.base + path
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, url, body)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	return req
}

// send performs req and reads the whole body.
func (e *env) send(req *http.Request) response {
	e.t.Helper()
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

// waitFor polls cond until it holds or a deadline passes. Click counts are
// written by a background flusher, so assertions on them are eventual.
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(flushInterval / 2)
	}
}

// decode unmarshals the response body into v, failing the test on bad JSON.
func (e *env) decode(r response, v any) {
	e.t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		e.t.Fatalf("decode %q: %v", r.Body, err)
	}
}

// linkBody mirrors the API's Link schema.
type linkBody struct {
	Code       string     `json:"code"`
	ShortURL   string     `json:"short_url"`
	URL        string     `json:"url"`
	ClickCount int64      `json:"click_count"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type createRequest struct {
	URL       string     `json:"url"`
	Alias     string     `json:"alias,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// create posts a new link and fails the test unless it is accepted.
func (e *env) create(key string, req createRequest) linkBody {
	e.t.Helper()
	r := e.do(http.MethodPost, "/api/v1/links", key, req)
	if r.Status != http.StatusCreated {
		e.t.Fatalf("create link = %d: %s", r.Status, r.Body)
	}
	var l linkBody
	e.decode(r, &l)
	return l
}

// expectStatus fails the test when r.Status differs from want.
func (e *env) expectStatus(r response, want int, what string) {
	e.t.Helper()
	if r.Status != want {
		e.t.Fatalf("%s = %d, want %d: %s", what, r.Status, want, r.Body)
	}
}
