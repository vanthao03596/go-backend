// Command api is the HTTP entry point. It wires config, logging and the server
// and contains no business logic.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/thaopv/go-backend/internal/apikey"
	"github.com/thaopv/go-backend/internal/config"
	"github.com/thaopv/go-backend/internal/db"
	"github.com/thaopv/go-backend/internal/db/sqlc"
	"github.com/thaopv/go-backend/internal/link"
	"github.com/thaopv/go-backend/internal/platform/slogx"
	"github.com/thaopv/go-backend/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := cfg.SlogLevel()
	log := slogx.New(os.Stdout, level, cfg.IsDev())
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.PoolOptions{
		MaxConns:        cfg.DB.MaxConns,
		MinConns:        cfg.DB.MinConns,
		MaxConnLifetime: cfg.DB.MaxConnLifetime,
		MaxConnIdleTime: cfg.DB.MaxConnIdleTime,
		ConnectTimeout:  cfg.DB.ConnectTimeout,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("database connected", "max_conns", cfg.DB.MaxConns)

	if cfg.MigrateOnStart {
		if err := db.Migrate(ctx, pool, log); err != nil {
			return err
		}
	}

	queries := sqlc.New(pool)
	linkSvc := link.NewService(link.NewRepository(queries), cfg.BaseURL, log)
	linkHandler := link.NewHandler(linkSvc, log)

	opts := []server.Option{
		server.WithReadiness("postgres", pool.Ping),
		server.WithFeature(linkHandler.Register),
	}
	if cfg.DevAPIKeyID != 0 {
		if _, err := queries.GetAPIKeyByID(ctx, cfg.DevAPIKeyID); err != nil {
			return fmt.Errorf("DEV_API_KEY_ID=%d does not match an active api_keys row (run `task dev:seed`): %w", cfg.DevAPIKeyID, err)
		}
		log.Warn("DEV_API_KEY_ID set: management API is unauthenticated", "api_key_id", cfg.DevAPIKeyID)
		opts = append(opts, server.WithAPIMiddleware(apikey.Static(cfg.DevAPIKeyID)))
	}

	srv := server.New(cfg, log, opts...)
	return srv.Run(ctx)
}
