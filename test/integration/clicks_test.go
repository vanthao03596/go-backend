//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vanthao03596/go-backend/internal/click"
	"github.com/vanthao03596/go-backend/internal/db/sqlc"
)

type statsBody struct {
	Code        string `json:"code"`
	TotalClicks int64  `json:"total_clicks"`
	Days        []struct {
		Date   string `json:"date"`
		Clicks int64  `json:"clicks"`
	} `json:"days"`
}

func (e *env) stats(key, code string) (response, statsBody) {
	e.t.Helper()
	r := e.do(http.MethodGet, "/api/v1/links/"+code+"/stats", key, nil)
	var s statsBody
	if r.Status == http.StatusOK {
		e.decode(r, &s)
	}
	return r, s
}

func TestLinkStats(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	key := e.newKey("stats")
	other := e.newKey("stats-other")
	l := e.create(key, createRequest{URL: target})

	// Before any visit: seven zero days and the lifetime counter at zero.
	r, s := e.stats(key, l.Code)
	e.expectStatus(r, http.StatusOK, "GET stats (fresh)")
	today := e.clock.Now().UTC().Format(time.DateOnly)
	if s.Code != l.Code || s.TotalClicks != 0 || len(s.Days) != click.StatsDays || s.Days[click.StatsDays-1].Date != today {
		t.Fatalf("fresh stats = %+v (today %s)", s, today)
	}
	for _, d := range s.Days {
		if d.Clicks != 0 {
			t.Fatalf("fresh stats has clicks: %+v", s.Days)
		}
	}

	// Three visits with a referrer, one HEAD that must not count.
	for range 3 {
		req := e.request(http.MethodGet, l.ShortURL, "")
		req.Header.Set("Referer", "https://ref.example/page?token=secret")
		req.Header.Set("User-Agent", "integration-test/1.0")
		e.expectStatus(e.send(req), http.StatusFound, "redirect")
	}
	e.expectStatus(e.do(http.MethodHead, l.ShortURL, "", nil), http.StatusFound, "HEAD")

	// Clicks are flushed asynchronously; wait for them to land.
	e.waitFor("clicks flushed", func() bool {
		_, s = e.stats(key, l.Code)
		return s.TotalClicks == 3
	})
	if got := s.Days[click.StatsDays-1]; got.Date != today || got.Clicks != 3 {
		t.Errorf("today's bucket = %+v, want %s with 3 clicks", got, today)
	}
	var sum int64
	for _, d := range s.Days {
		sum += d.Clicks
	}
	if sum != 3 {
		t.Errorf("sum of days = %d, want 3", sum)
	}

	// The link resource reports the same counter.
	r = e.do(http.MethodGet, "/api/v1/links/"+l.Code, key, nil)
	e.expectStatus(r, http.StatusOK, "GET link")
	var got linkBody
	e.decode(r, &got)
	if got.ClickCount != 3 {
		t.Errorf("link click_count = %d, want 3", got.ClickCount)
	}

	// Rows carry the referrer without its query string, and the user agent.
	rows, err := pool.Query(e.t.Context(),
		`SELECT c.referrer, c.user_agent FROM clicks c JOIN links l ON l.id = c.link_id WHERE l.code = $1`, l.Code)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ref, ua *string
		if err := rows.Scan(&ref, &ua); err != nil {
			t.Fatal(err)
		}
		if ref == nil || *ref != "https://ref.example/page" || ua == nil || *ua != "integration-test/1.0" {
			t.Errorf("row referrer=%v ua=%v", ref, ua)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("clicks rows = %d, want 3", n)
	}

	// A day later the visits sit in yesterday's bucket.
	e.clock.Advance(24 * time.Hour)
	_, s = e.stats(key, l.Code)
	if s.Days[click.StatsDays-2].Clicks != 3 || s.Days[click.StatsDays-1].Clicks != 0 || s.TotalClicks != 3 {
		t.Errorf("stats after a day = %+v", s)
	}

	// Scoping and auth.
	r, _ = e.stats(other, l.Code)
	e.expectStatus(r, http.StatusNotFound, "stats for another key's link")
	r, _ = e.stats(key, "nope404x")
	e.expectStatus(r, http.StatusNotFound, "stats for unknown code")
	r, _ = e.stats("", l.Code)
	e.expectStatus(r, http.StatusUnauthorized, "stats without key")

	// The operation is in the spec.
	r = e.do(http.MethodGet, "/api/v1/openapi.json", "", nil)
	if !strings.Contains(string(r.Body), `"/links/{code}/stats"`) {
		t.Error("openapi.json does not list the stats operation")
	}
}

func TestClickPurgeKeepsRecentRows(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	key := e.newKey("purge")
	l := e.create(key, createRequest{URL: target})

	var linkID int64
	if err := pool.QueryRow(e.t.Context(), `SELECT id FROM links WHERE code = $1`, l.Code).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	q := sqlc.New(pool)
	now := time.Now()
	old := now.Add(-91 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	ua := "purge-test"
	if _, err := q.InsertClicks(e.t.Context(), []sqlc.InsertClicksParams{
		{LinkID: linkID, OccurredAt: old, UserAgent: &ua},
		{LinkID: linkID, OccurredAt: old.Add(-time.Hour), UserAgent: &ua},
		{LinkID: linkID, OccurredAt: recent, UserAgent: &ua},
	}); err != nil {
		t.Fatal(err)
	}

	svc := click.NewService(click.NewRepository(pool, q), logger)
	n, err := svc.Purge(e.t.Context(), 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("purged %d rows, want at least the 2 old ones", n)
	}

	var remaining int
	if err := pool.QueryRow(e.t.Context(), `SELECT count(*) FROM clicks WHERE link_id = $1`, linkID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Errorf("remaining rows = %d, want 1", remaining)
	}
}
