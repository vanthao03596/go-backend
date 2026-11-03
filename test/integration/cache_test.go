//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vanthao03596/go-backend/internal/app"
	"github.com/vanthao03596/go-backend/internal/link"
	"github.com/vanthao03596/go-backend/internal/platform/redisx"
)

const cacheKeyPrefix = "shortener:link:v1:"

// newRedis returns a client for url built the way production builds it.
func newRedis(t *testing.T, url string) *redis.Client {
	t.Helper()
	rdb, err := redisx.NewClient(url, redisx.Options{})
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// newCachedEnv is newEnv with the redirect cache on rdb.
func newCachedEnv(t *testing.T, rdb *redis.Client) *env {
	t.Helper()
	return newEnvWith(t, pool, nil, app.WithRedis(rdb))
}

// retarget changes a link's target behind the application's back. Links
// have no update API, so a redirect that still answers with the old target
// can only have come from the cache.
func retarget(t *testing.T, code, url string) {
	t.Helper()
	tag, err := pool.Exec(t.Context(), "UPDATE links SET target_url = $1 WHERE code = $2", url, code)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("retarget %s: rows=%d err=%v", code, tag.RowsAffected(), err)
	}
}

func TestRedirectCache(t *testing.T) {
	t.Parallel()
	rdb := newRedis(t, redisURL)
	e := newCachedEnv(t, rdb)
	key := e.newKey("cache")

	l := e.create(key, createRequest{URL: target})
	ttl, err := rdb.TTL(t.Context(), cacheKeyPrefix+l.Code).Result()
	if err != nil || ttl <= 9*time.Minute || ttl > 10*time.Minute {
		t.Fatalf("entry TTL after create = %s, %v; want about LINK_CACHE_TTL (10m)", ttl, err)
	}

	retarget(t, l.Code, "https://changed.example/")
	r := e.do(http.MethodGet, l.ShortURL, "", nil)
	e.expectStatus(r, http.StatusFound, "GET short url")
	if loc := r.Header.Get("Location"); loc != target {
		t.Errorf("Location = %q, want the cached %q", loc, target)
	}
	// The click still counts: the cached entry carries the link id.
	e.waitFor("click from a cached redirect", func() bool {
		var got linkBody
		e.decode(e.do(http.MethodGet, "/api/v1/links/"+l.Code, key, nil), &got)
		return got.ClickCount == 1
	})

	// Deleting writes a tombstone: 410 at once, not after the TTL.
	e.expectStatus(e.do(http.MethodDelete, "/api/v1/links/"+l.Code, key, nil), http.StatusNoContent, "DELETE link")
	e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusGone, "GET deleted short url")
	if raw, _ := rdb.Get(t.Context(), cacheKeyPrefix+l.Code).Result(); raw != `{"deleted":true}` {
		t.Errorf("entry after delete = %q", raw)
	}
}

func TestUnknownCodesAreCachedBriefly(t *testing.T) {
	t.Parallel()
	rdb := newRedis(t, redisURL)
	e := newCachedEnv(t, rdb)
	key := e.newKey("negative")
	alias := "neg-" + randomSuffix(t)

	e.expectStatus(e.do(http.MethodGet, "/"+alias, "", nil), http.StatusNotFound, "GET unknown alias")
	ttl, err := rdb.TTL(t.Context(), cacheKeyPrefix+alias).Result()
	if err != nil || ttl <= 0 || ttl > time.Minute {
		t.Fatalf("negative entry TTL = %s, %v; want at most 60s", ttl, err)
	}

	// Creating the alias must not wait out the negative TTL.
	e.create(key, createRequest{URL: target, Alias: alias})
	r := e.do(http.MethodGet, "/"+alias, "", nil)
	e.expectStatus(r, http.StatusFound, "GET new alias")
	if loc := r.Header.Get("Location"); loc != target {
		t.Errorf("Location = %q, want %q", loc, target)
	}
}

// TestRedisCacheFillNeverOverwrites checks the coherence rule against a real
// server: a reader's Fill cannot replace a writer's Put.
func TestRedisCacheFillNeverOverwrites(t *testing.T) {
	t.Parallel()
	rdb := newRedis(t, redisURL)
	c := link.NewRedisCache(rdb, time.Hour, time.Minute)
	ctx := t.Context()
	code := "fill-" + randomSuffix(t)

	// The delete's tombstone lands between a reader's database read and its
	// fill of the now stale live destination.
	if err := c.Put(ctx, code, link.Destination{Deleted: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Fill(ctx, code, link.Destination{LinkID: 1, URL: target}); err != nil {
		t.Fatal(err)
	}
	d, ok, err := c.Get(ctx, code)
	if err != nil || !ok || !d.Deleted {
		t.Fatalf("Get = %+v, %v, %v; the tombstone must win", d, ok, err)
	}

	// And a Put replaces whatever a Fill stored.
	if err := c.Put(ctx, code, link.Destination{LinkID: 2, URL: target}); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := c.Get(ctx, code); d.LinkID != 2 {
		t.Errorf("Get after Put = %+v", d)
	}
}

// TestRedirectsSurviveACacheOutage runs the link lifecycle against a Redis
// that accepts connections and never answers, the failure a timeout alone
// handles worst. Every answer must still be right.
func TestRedirectsSurviveACacheOutage(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	e := newCachedEnv(t, newRedis(t, "redis://"+lis.Addr().String()+"/0?read_timeout=50ms"))
	key := e.newKey("outage")

	start := time.Now()
	l := e.create(key, createRequest{URL: target})
	for range 20 {
		e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusFound, "GET short url with Redis hung")
	}
	e.expectStatus(e.do(http.MethodDelete, "/api/v1/links/"+l.Code, key, nil), http.StatusNoContent, "DELETE with Redis hung")
	e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusGone, "GET deleted short url with Redis hung")
	e.expectStatus(e.do(http.MethodGet, "/unknown-"+randomSuffix(t), "", nil), http.StatusNotFound, "GET unknown with Redis hung")
	// 23 cache round trips at 50ms each would be well over a second; the
	// breaker opens after five.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("lifecycle with a hung Redis took %s", elapsed)
	}
}

// switchProxy forwards TCP connections to upstream while up and holds them
// silently while down, like a Redis that has stopped answering. Connections
// accepted while down stay dead after it comes back, as they would after a
// real outage.
type switchProxy struct {
	addr string
	up   atomic.Bool
}

func newSwitchProxy(t *testing.T, upstream string) *switchProxy {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &switchProxy{addr: lis.Addr().String()}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	track := func(c net.Conn) {
		mu.Lock()
		defer mu.Unlock()
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		_ = lis.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			track(c)
			if !p.up.Load() {
				continue // held: never read, never answered
			}
			u, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = c.Close()
				continue
			}
			track(u)
			wg.Go(func() { _, _ = io.Copy(u, c); _ = u.Close() })
			wg.Go(func() { _, _ = io.Copy(c, u); _ = c.Close() })
		}
	})
	return p
}

// TestCacheRecoversAfterAnOutage starts with Redis unreachable, so the
// breaker opens before the pool has a single working connection, then
// brings it back. Recovery needs the breaker's probe to open a fresh
// connection, handshake included.
func TestCacheRecoversAfterAnOutage(t *testing.T) {
	t.Parallel()
	u, err := url.Parse(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newSwitchProxy(t, u.Host)
	e := newCachedEnv(t, newRedis(t, "redis://"+proxy.addr+"/0?read_timeout=50ms"))
	direct := newRedis(t, redisURL)
	key := e.newKey("recovery")

	l := e.create(key, createRequest{URL: target})
	for range 10 {
		e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusFound, "GET short url during outage")
	}
	if n, _ := direct.Exists(t.Context(), cacheKeyPrefix+l.Code).Result(); n != 0 {
		t.Fatal("entry written while Redis was unreachable")
	}

	proxy.up.Store(true)
	e.waitFor("the cache to be filled again", func() bool {
		e.expectStatus(e.do(http.MethodGet, l.ShortURL, "", nil), http.StatusFound, "GET short url after recovery")
		n, _ := direct.Exists(t.Context(), cacheKeyPrefix+l.Code).Result()
		return n == 1
	})
}

// TestBreakerOpensOnAuthFailure checks against a real server that an error
// reply (here a password the server rejects) counts against Redis: it answers,
// but every call to it is a wasted round trip. The test server's default user
// accepts any password, so the test adds a user of its own.
func TestBreakerOpensOnAuthFailure(t *testing.T) {
	t.Parallel()
	u, err := url.Parse(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	user := "breaker-" + randomSuffix(t)
	admin := newRedis(t, redisURL)
	if err := admin.Do(t.Context(), "ACL", "SETUSER", user, "on", ">right-password", "~*", "+@all").Err(); err != nil {
		t.Fatalf("create redis user: %v", err)
	}
	t.Cleanup(func() { _ = admin.Do(context.Background(), "ACL", "DELUSER", user).Err() })
	rdb := newRedis(t, "redis://"+user+":wrong-password@"+u.Host+"/0")
	b := redisx.NewBreaker(redisx.BreakerConfig{}, logger)

	refused := 0
	for i := range 20 {
		err := b.Allow()
		if err == nil {
			err = rdb.Get(t.Context(), "k").Err()
			b.ReportResult(err)
		}
		switch {
		case errors.Is(err, redisx.ErrCircuitOpen):
			refused++
		case err == nil || errors.Is(err, redis.Nil):
			t.Fatalf("GET %d with a wrong password = %v", i, err)
		}
	}
	if want := 20 - redisx.DefaultBreakerThreshold; refused != want {
		t.Errorf("%d of 20 calls short-circuited, want %d", refused, want)
	}
}

func TestListCursorsAreSigned(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	key := e.newKey("cursor-owner")
	other := e.newKey("cursor-other")
	for range 2 {
		e.create(key, createRequest{URL: target})
		e.create(other, createRequest{URL: target})
	}

	var page struct {
		NextCursor string `json:"next_cursor"`
	}
	e.decode(e.do(http.MethodGet, "/api/v1/links?limit=1", key, nil), &page)
	if page.NextCursor == "" {
		t.Fatal("no next cursor")
	}
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links?limit=1&cursor="+page.NextCursor, key, nil), http.StatusOK, "own cursor")
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links?limit=1&cursor="+page.NextCursor, other, nil),
		http.StatusUnprocessableEntity, "another key's cursor")

	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil {
		t.Fatalf("cursor is not base64url: %v", err)
	}
	raw[len(raw)/2] ^= 0x01
	forged := base64.RawURLEncoding.EncodeToString(raw)
	e.expectStatus(e.do(http.MethodGet, "/api/v1/links?limit=1&cursor="+forged, key, nil),
		http.StatusUnprocessableEntity, "edited cursor")
}

// randomSuffix returns a short code-safe string unique to this run.
func randomSuffix(t *testing.T) string {
	t.Helper()
	code, err := link.GenerateCode()
	if err != nil {
		t.Fatal(errors.Join(errors.New("random suffix"), err))
	}
	return code
}
