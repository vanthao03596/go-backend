//go:build integration

// Package integration exercises the assembled HTTP server against a real
// PostgreSQL started with testcontainers. Run with:
//
//	go test -race -count=1 -tags=integration ./test/integration/...
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/vanthao03596/go-backend/internal/db"
)

// postgresImage matches deploy/docker-compose.yml so tests and local
// development run the same server version.
const postgresImage = "postgres:17-alpine"

// pool is shared by every test. Tests isolate themselves by issuing their own
// API key; links are scoped to a key and codes are random, so nothing collides.
var pool *pgxpool.Pool

// dsn reaches the test database, for tests that need a pool of their own
// (for example one instrumented with a test tracer provider).
var dsn string

// logger discards application logs so test output stays readable. Switch to a
// text handler on stderr when debugging a failing test.
var logger = slog.New(slog.DiscardHandler)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("shortener"),
		postgres.WithUsername("shortener"),
		postgres.WithPassword("shortener"),
		postgres.BasicWaitStrategies(),
	)
	// Terminate even when Run failed part way: the helper is a no-op on nil.
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintln(os.Stderr, "terminate postgres container:", err)
		}
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres container:", err)
		return 1
	}

	dsn, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres connection string:", err)
		return 1
	}

	pool, err = db.NewPool(ctx, dsn, db.PoolOptions{MaxConns: 10, MinConns: 1})
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, logger); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}

	return m.Run()
}
