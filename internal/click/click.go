// Package click is the bounded context for redirect analytics: it takes
// clicks off the redirect path, persists them in batches, answers per-link
// statistics and purges old rows.
//
// The redirect handler never waits on the database for a click. link.Service
// hands each counted redirect to a Buffer, whose single flusher goroutine
// writes the clicks rows and bumps links.click_count in one transaction.
package click

import (
	"context"
	"time"

	"github.com/vanthao03596/go-backend/internal/link"
)

// Storage limits. Longer values are truncated before they reach Postgres;
// the table has matching CHECK constraints.
const (
	MaxReferrerLen  = 2048
	MaxUserAgentLen = 512
)

// DayCount is the number of clicks on one UTC calendar day.
type DayCount struct {
	Day    time.Time // midnight UTC
	Clicks int64
}

// Stats is what the stats endpoint returns for one link.
type Stats struct {
	// Total is the link's lifetime counter (links.click_count), which also
	// covers clicks that have since been purged.
	Total int64
	// Days holds one entry per day of the window, oldest first, zero-filled.
	Days []DayCount
}

// Repository is the persistence boundary the buffer and service depend on.
type Repository interface {
	// Store persists clicks and adds them to their links' counters
	// atomically. An empty slice is a no-op.
	Store(ctx context.Context, clicks []link.Click) error
	// CountByDay returns clicks per UTC day for linkID in [since, until),
	// ascending. Days without clicks are absent.
	CountByDay(ctx context.Context, linkID int64, since, until time.Time) ([]DayCount, error)
	// DeleteBefore removes up to limit clicks that occurred before cutoff and
	// reports how many it removed.
	DeleteBefore(ctx context.Context, cutoff time.Time, limit int32) (int64, error)
}
