//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
)

func TestAuthRequired(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	revokedID, revoked := e.newKeyWithID("revoked")
	e.revokeKey(revokedID)

	// Each value is the verbatim Authorization header; empty sends none.
	cases := map[string]string{
		"no header":     "",
		"basic scheme":  "Basic " + strings.Repeat("a", 43),
		"bare token":    "sk_" + strings.Repeat("a", 43),
		"empty bearer":  "Bearer ",
		"random token":  "Bearer sk_definitely-not-issued",
		"wrong prefix":  "Bearer pk_" + strings.Repeat("a", 43),
		"oversized":     "Bearer sk_" + strings.Repeat("a", 200),
		"revoked key":   "Bearer " + revoked,
		"double scheme": "Bearer Bearer " + revoked,
	}
	for name, auth := range cases {
		r := e.doAuth(http.MethodPost, "/api/v1/links", auth, createRequest{URL: target})
		if r.Status != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401: %s", name, r.Status, r.Body)
			continue
		}
		if !strings.HasPrefix(r.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: WWW-Authenticate = %q", name, r.Header.Get("WWW-Authenticate"))
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
			t.Errorf("%s: Content-Type = %q", name, ct)
		}
	}

	// The scheme is case-insensitive per RFC 9110.
	key := e.newKey("lowercase-scheme")
	e.expectStatus(e.doAuth(http.MethodGet, "/api/v1/links", "bearer "+key, nil), http.StatusOK, "lowercase scheme")
}

func TestLinksAreScopedToKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	owner := e.newKey("owner")
	intruder := e.newKey("intruder")

	l := e.create(owner, createRequest{URL: target})

	// Another key sees 404, not 403, so it cannot probe which codes exist.
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links/"+l.Code, intruder, nil), http.StatusNotFound, "GET as intruder")
	e.expectStatus(e.do(http.MethodDelete, "/api/v1/links/"+l.Code, intruder, nil), http.StatusNotFound, "DELETE as intruder")

	r := e.do(http.MethodGet, "/api/v1/links", intruder, nil)
	e.expectStatus(r, http.StatusOK, "GET list as intruder")
	if strings.Contains(string(r.Body), l.Code) {
		t.Errorf("intruder list leaks owner's code: %s", r.Body)
	}

	// The owner is unaffected, and the public redirect never needs a key.
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links/"+l.Code, owner, nil), http.StatusOK, "GET as owner")
	e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusFound, "anonymous redirect")
}
