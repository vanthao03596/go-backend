package apikey

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/thaopv/go-backend/internal/db/sqlc"
)

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
func (r *PgRepository) Insert(ctx context.Context, keyHash []byte, name string) (Key, error) {
	row, err := r.q.CreateAPIKey(ctx, keyHash, name)
	if err != nil {
		return Key{}, fmt.Errorf("insert api key: %w", err)
	}
	return fromRow(row), nil
}

// GetByHash implements Repository.
func (r *PgRepository) GetByHash(ctx context.Context, keyHash []byte) (Key, error) {
	row, err := r.q.GetAPIKeyByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Key{}, ErrNotFound
		}
		return Key{}, fmt.Errorf("get api key by hash: %w", err)
	}
	return fromRow(row), nil
}

func fromRow(row sqlc.APIKey) Key {
	return Key{ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt}
}
