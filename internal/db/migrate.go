package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"github.com/vanthao03596/go-backend/migrations"
)

// Migrate applies every pending embedded migration using the given pool.
// It borrows connections from the pool through database/sql, so no second
// connection string or driver registration is needed.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (err error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { err = errors.Join(err, sqlDB.Close()) }()

	provider, err := goose.NewProvider(database.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	defer func() { err = errors.Join(err, provider.Close()) }()

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		log.InfoContext(ctx, "migration applied",
			"version", r.Source.Version,
			"file", r.Source.Path,
			"duration", r.Duration,
		)
	}
	if len(results) == 0 {
		log.InfoContext(ctx, "migrations up to date")
	}
	return nil
}
