package click

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vanthao03596/go-backend/internal/db/sqlc"
	"github.com/vanthao03596/go-backend/internal/link"
)

// Beginner starts transactions. *pgxpool.Pool satisfies it.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PgRepository implements Repository over the sqlc-generated queries.
type PgRepository struct {
	db Beginner
	q  *sqlc.Queries
}

// NewRepository wraps a pool and its query set.
func NewRepository(db Beginner, q *sqlc.Queries) *PgRepository {
	return &PgRepository{db: db, q: q}
}

var _ Repository = (*PgRepository)(nil)

// Store implements Repository. The clicks rows go in with COPY and the
// per-link deltas are applied with one UPDATE, both in a single transaction
// so the counter and the rows never disagree.
func (r *PgRepository) Store(ctx context.Context, clicks []link.Click) error {
	if len(clicks) == 0 {
		return nil
	}
	rows := make([]sqlc.InsertClicksParams, 0, len(clicks))
	deltas := make(map[int64]int64, len(clicks))
	for _, c := range clicks {
		rows = append(rows, sqlc.InsertClicksParams{
			LinkID:     c.LinkID,
			OccurredAt: c.OccurredAt,
			Referrer:   nullable(c.Referrer),
			UserAgent:  nullable(c.UserAgent),
		})
		deltas[c.LinkID]++
	}
	ids := make([]int64, 0, len(deltas))
	counts := make([]int64, 0, len(deltas))
	for id, n := range deltas {
		ids = append(ids, id)
		counts = append(counts, n)
	}

	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if _, err := q.InsertClicks(ctx, rows); err != nil {
			return fmt.Errorf("copy clicks: %w", err)
		}
		if err := q.AddClickCounts(ctx, ids, counts); err != nil {
			return fmt.Errorf("add click counts: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store clicks: %w", err)
	}
	return nil
}

// CountByDay implements Repository.
func (r *PgRepository) CountByDay(ctx context.Context, linkID int64, since, until time.Time) ([]DayCount, error) {
	rows, err := r.q.CountClicksByDay(ctx, sqlc.CountClicksByDayParams{LinkID: linkID, Since: since, Until: until})
	if err != nil {
		return nil, fmt.Errorf("count clicks by day: %w", err)
	}
	out := make([]DayCount, 0, len(rows))
	for _, row := range rows {
		// A date column scans as midnight in the connection's zone; pin it to UTC.
		y, m, d := row.Day.Date()
		out = append(out, DayCount{Day: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Clicks: row.Clicks})
	}
	return out, nil
}

// DeleteBefore implements Repository.
func (r *PgRepository) DeleteBefore(ctx context.Context, cutoff time.Time, limit int32) (int64, error) {
	n, err := r.q.PurgeClicksBefore(ctx, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("purge clicks: %w", err)
	}
	return n, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
