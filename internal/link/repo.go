package link

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thaopv/go-backend/internal/db/sqlc"
)

const pgUniqueViolation = "23505"

// PgRepository implements Repository over the sqlc-generated queries.
type PgRepository struct {
	q *sqlc.Queries
}

// NewRepository wraps a sqlc query set. Pass sqlc.New(pool).
func NewRepository(q *sqlc.Queries) *PgRepository {
	return &PgRepository{q: q}
}

var _ Repository = (*PgRepository)(nil)

// Insert implements Repository.
func (r *PgRepository) Insert(ctx context.Context, code, targetURL string, apiKeyID int64, expiresAt *time.Time) (Link, error) {
	row, err := r.q.CreateLink(ctx, sqlc.CreateLinkParams{
		Code:      code,
		TargetURL: targetURL,
		APIKeyID:  apiKeyID,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Link{}, ErrCodeTaken
		}
		return Link{}, fmt.Errorf("insert link: %w", err)
	}
	return fromRow(row), nil
}

// GetByCode implements Repository.
func (r *PgRepository) GetByCode(ctx context.Context, code string) (Link, error) {
	row, err := r.q.GetLinkByCode(ctx, code)
	if err != nil {
		return Link{}, mapGetErr("get link", err)
	}
	return fromRow(row), nil
}

// GetByCodeForKey implements Repository.
func (r *PgRepository) GetByCodeForKey(ctx context.Context, code string, apiKeyID int64) (Link, error) {
	row, err := r.q.GetLinkByCodeForAPIKey(ctx, code, apiKeyID)
	if err != nil {
		return Link{}, mapGetErr("get link for key", err)
	}
	return fromRow(row), nil
}

// List implements Repository.
func (r *PgRepository) List(ctx context.Context, apiKeyID int64, cursor Cursor, limit int32) ([]Link, error) {
	rows, err := r.q.ListLinksByAPIKey(ctx, sqlc.ListLinksByAPIKeyParams{
		APIKeyID:        apiKeyID,
		CursorCreatedAt: cursor.CreatedAt,
		CursorID:        cursor.ID,
		PageSize:        limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	out := make([]Link, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row))
	}
	return out, nil
}

// SoftDelete implements Repository.
func (r *PgRepository) SoftDelete(ctx context.Context, code string, apiKeyID int64) error {
	n, err := r.q.SoftDeleteLink(ctx, code, apiKeyID)
	if err != nil {
		return fmt.Errorf("soft delete link: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IncrementClicks implements Repository.
func (r *PgRepository) IncrementClicks(ctx context.Context, id, delta int64) error {
	if err := r.q.IncrementClick(ctx, delta, id); err != nil {
		return fmt.Errorf("increment click: %w", err)
	}
	return nil
}

func fromRow(row sqlc.Link) Link {
	return Link{
		ID:         row.ID,
		Code:       row.Code,
		TargetURL:  row.TargetURL,
		APIKeyID:   row.APIKeyID,
		ClickCount: row.ClickCount,
		ExpiresAt:  row.ExpiresAt,
		CreatedAt:  row.CreatedAt,
		DeletedAt:  row.DeletedAt,
	}
}

func mapGetErr(op string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("%s: %w", op, err)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
