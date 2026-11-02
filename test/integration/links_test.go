//go:build integration

package integration

import (
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vanthao03596/go-backend/internal/config"
)

const target = "https://example.com/some/long/path?utm=1"

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	e.expectStatus(e.do(http.MethodGet, "/healthz", "", nil), http.StatusOK, "GET /healthz")
	e.expectStatus(e.do(http.MethodGet, "/readyz", "", nil), http.StatusOK, "GET /readyz")
}

func TestDocsArePublic(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	r := e.do(http.MethodGet, "/api/v1/openapi.json", "", nil)
	e.expectStatus(r, http.StatusOK, "GET /api/v1/openapi.json")
	if !strings.Contains(string(r.Body), `"bearerAuth"`) {
		t.Errorf("spec does not declare bearerAuth: %s", r.Body)
	}
	e.expectStatus(e.do(http.MethodGet, "/api/v1/docs", "", nil), http.StatusOK, "GET /api/v1/docs")
}

func TestLinkLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	key := e.newKey("lifecycle")

	// Create.
	r := e.do(http.MethodPost, "/api/v1/links", key, createRequest{URL: target})
	e.expectStatus(r, http.StatusCreated, "POST /api/v1/links")
	var created linkBody
	e.decode(r, &created)
	if len(created.Code) != 7 {
		t.Errorf("code = %q, want 7 chars", created.Code)
	}
	if want := e.base + "/" + created.Code; created.ShortURL != want || r.Header.Get("Location") != want {
		t.Errorf("short_url = %q, Location = %q, want %q", created.ShortURL, r.Header.Get("Location"), want)
	}
	if created.URL != target || created.ClickCount != 0 || created.ExpiresAt != nil {
		t.Errorf("unexpected body: %+v", created)
	}
	if created.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at zone = %v, want UTC", created.CreatedAt.Location())
	}

	// Get.
	r = e.do(http.MethodGet, "/api/v1/links/"+created.Code, key, nil)
	e.expectStatus(r, http.StatusOK, "GET link")
	var got linkBody
	e.decode(r, &got)
	if got.Code != created.Code || got.URL != created.URL || got.ShortURL != created.ShortURL ||
		got.ClickCount != created.ClickCount || !got.CreatedAt.Equal(created.CreatedAt) || got.ExpiresAt != nil {
		t.Errorf("GET = %+v, want %+v", got, created)
	}

	// Redirect counts a click; HEAD does not. HEAD goes first so that, by
	// the time the GET's click has been flushed, a wrongly counted HEAD
	// would have been flushed too.
	e.expectStatus(e.do(http.MethodHead, created.ShortURL, "", nil), http.StatusFound, "HEAD short url")
	r = e.do(http.MethodGet, created.ShortURL, "", nil)
	e.expectStatus(r, http.StatusFound, "GET short url")
	if loc := r.Header.Get("Location"); loc != target {
		t.Errorf("redirect Location = %q, want %q", loc, target)
	}
	if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rp := r.Header.Get("Referrer-Policy"); rp != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q", rp)
	}

	// The click is written by the background flusher, so the counter is
	// eventually consistent.
	e.waitFor("click_count to reach 1", func() bool {
		r = e.do(http.MethodGet, "/api/v1/links/"+created.Code, key, nil)
		e.expectStatus(r, http.StatusOK, "GET link after redirect")
		e.decode(r, &got)
		return got.ClickCount >= 1
	})
	if got.ClickCount != 1 {
		t.Errorf("click_count = %d, want 1 (GET counts, HEAD does not)", got.ClickCount)
	}

	// Delete.
	e.expectStatus(e.do(http.MethodDelete, "/api/v1/links/"+created.Code, key, nil), http.StatusNoContent, "DELETE link")
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links/"+created.Code, key, nil), http.StatusNotFound, "GET deleted link")
	e.expectStatus(e.do(http.MethodDelete, "/api/v1/links/"+created.Code, key, nil), http.StatusNotFound, "DELETE twice")
	e.expectStatus(e.do(http.MethodGet, created.ShortURL, "", nil), http.StatusGone, "GET deleted short url")

	// Unknown codes.
	e.expectStatus(e.do(http.MethodGet, "/nope404x", "", nil), http.StatusNotFound, "GET unknown code")
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links/nope404x", key, nil), http.StatusNotFound, "GET unknown link")
}

func TestAliasRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	key := e.newKey("alias")
	other := e.newKey("alias-other")

	// Deleted aliases stay reserved, so make it unique per run (-count=2
	// reuses the container).
	alias := "promo-" + strings.ToLower(rand.Text()[:8])
	l := e.create(key, createRequest{URL: target, Alias: alias})
	if l.Code != alias {
		t.Fatalf("code = %q, want alias %q", l.Code, alias)
	}
	e.expectStatus(e.do(http.MethodGet, "/"+alias, "", nil), http.StatusFound, "redirect alias")

	// The namespace is global: another key cannot take the same alias.
	e.expectStatus(e.do(http.MethodPost, "/api/v1/links", other, createRequest{URL: target, Alias: alias}),
		http.StatusConflict, "duplicate alias from another key")

	// Deleted aliases stay reserved.
	e.expectStatus(e.do(http.MethodDelete, "/api/v1/links/"+alias, key, nil), http.StatusNoContent, "DELETE alias")
	e.expectStatus(e.do(http.MethodPost, "/api/v1/links", key, createRequest{URL: target, Alias: alias}),
		http.StatusConflict, "reuse deleted alias")

	cases := map[string]createRequest{
		"reserved alias":   {URL: target, Alias: "docs"},
		"alias too short":  {URL: target, Alias: "ab"},
		"alias bad chars":  {URL: target, Alias: "has space"},
		"private host":     {URL: "http://localhost/admin"},
		"ftp scheme":       {URL: "ftp://example.com/file"},
		"relative url":     {URL: "/just/a/path"},
		"missing url":      {},
		"url too long":     {URL: "https://example.com/" + strings.Repeat("a", 2048)},
		"expiry in past":   {URL: target, ExpiresAt: new(e.clock.Now().Add(-time.Minute))},
		"expiry right now": {URL: target, ExpiresAt: new(e.clock.Now())},
	}
	for name, req := range cases {
		r := e.do(http.MethodPost, "/api/v1/links", key, req)
		if r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422: %s", name, r.Status, r.Body)
		}
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	key := e.newKey("expiry")

	exp := e.clock.Now().Add(time.Hour).Truncate(time.Microsecond) // timestamptz resolution
	l := e.create(key, createRequest{URL: target, ExpiresAt: &exp})
	if l.ExpiresAt == nil || !l.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at = %v, want %v", l.ExpiresAt, exp)
	}

	e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusFound, "redirect before expiry")

	// Move the application clock past the expiry instead of sleeping.
	e.clock.Advance(time.Hour + time.Second)
	e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusGone, "redirect after expiry")
	e.expectStatus(e.do(http.MethodHead, l.ShortURL, "", nil), http.StatusGone, "HEAD after expiry")

	// The owner can still inspect an expired link.
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links/"+l.Code, key, nil), http.StatusOK, "GET expired link")
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	key := e.newKey("list")
	other := e.newKey("list-other")

	var codes []string
	for range 3 {
		codes = append(codes, e.create(key, createRequest{URL: target}).Code)
	}
	e.create(other, createRequest{URL: target})

	type page struct {
		Items      []linkBody `json:"items"`
		NextCursor string     `json:"next_cursor"`
	}

	var first page
	r := e.do(http.MethodGet, "/api/v1/links?limit=2", key, nil)
	e.expectStatus(r, http.StatusOK, "GET page 1")
	e.decode(r, &first)
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("page 1 = %d items, cursor %q", len(first.Items), first.NextCursor)
	}
	// Newest first.
	if first.Items[0].Code != codes[2] || first.Items[1].Code != codes[1] {
		t.Errorf("page 1 order = %s,%s want %s,%s", first.Items[0].Code, first.Items[1].Code, codes[2], codes[1])
	}

	var second page
	r = e.do(http.MethodGet, "/api/v1/links?limit=2&cursor="+first.NextCursor, key, nil)
	e.expectStatus(r, http.StatusOK, "GET page 2")
	e.decode(r, &second)
	if len(second.Items) != 1 || second.Items[0].Code != codes[0] || second.NextCursor != "" {
		t.Errorf("page 2 = %+v", second)
	}

	e.expectStatus(e.do(http.MethodGet, "/api/v1/links?cursor=%2A%2A", key, nil), http.StatusUnprocessableEntity, "bad cursor")
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links?limit=0", key, nil), http.StatusUnprocessableEntity, "limit 0")
}

func TestCreateRateLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *config.Config) { c.RateLimit.CreatePerSecond = 2 })

	// The limiter sits before authentication, so anonymous bursts are cheap
	// to reject: two 401s, then a 429.
	for i, want := range []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusTooManyRequests} {
		r := e.do(http.MethodPost, "/api/v1/links", "", createRequest{URL: target})
		if r.Status != want {
			t.Fatalf("POST %d = %d, want %d: %s", i+1, r.Status, want, r.Body)
		}
		if want == http.StatusTooManyRequests && r.Header.Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}

	// Other management routes keep their own budget.
	key := e.newKey("ratelimit")
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links", key, nil), http.StatusOK, "GET list during create burst")
}
