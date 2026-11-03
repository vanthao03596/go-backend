// Command api is the HTTP entry point. It wires config, logging and the server
// and contains no business logic. It also hosts the operator subcommands
// "apikey create <name>" and "clicks purge" so they reuse the same config
// and pool.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/app"
	"github.com/vanthao03596/go-backend/internal/click"
	"github.com/vanthao03596/go-backend/internal/config"
	"github.com/vanthao03596/go-backend/internal/db"
	"github.com/vanthao03596/go-backend/internal/db/sqlc"
	"github.com/vanthao03596/go-backend/internal/platform/otelx"
	"github.com/vanthao03596/go-backend/internal/platform/slogx"
)

// serviceName is the default OpenTelemetry service.name. OTEL_SERVICE_NAME
// overrides it.
const serviceName = "shortener"

// maxTelemetryShutdown caps the final span and metric export at exit. With
// the default SHUTDOWN_TIMEOUT of 10s the worst case after SIGTERM is then
// 10s (HTTP drain) + 10s (click flush) + 5s, inside a 30s grace period.
const maxTelemetryShutdown = 5 * time.Second

// version is reported as service.version. Release builds set it with
// -ldflags "-X main.version=<tag or sha>"; when empty, otelx.Version falls
// back to the build info stamped by go build.
var version string

const usage = `usage:
  api                       start the HTTP server
  api apikey create <name>  issue an API key and print it once
  api clicks purge          delete clicks older than CLICK_RETENTION`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	switch {
	case len(args) == 0:
		return serve()
	case len(args) >= 3 && args[0] == "apikey" && args[1] == "create":
		// Task expands CLI_ARGS unquoted, so a multi-word name arrives as
		// several arguments. Join them back together.
		return createAPIKey(strings.Join(args[2:], " "), stdout)
	case len(args) == 2 && args[0] == "clicks" && args[1] == "purge":
		return purgeClicks()
	default:
		return errors.New(usage)
	}
}

// bootstrap loads config, builds the logger and opens the pool. Logs go to
// logOut so the apikey subcommand can keep stdout for the key alone. The
// operator subcommands that use it are not traced.
func bootstrap(ctx context.Context, logOut io.Writer) (config.Config, *slog.Logger, *pgxpool.Pool, error) {
	cfg, log, err := loadConfig(logOut)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	pool, err := openPool(ctx, cfg, otelx.Disabled())
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	return cfg, log, pool, nil
}

// loadConfig reads the environment and installs the process logger.
func loadConfig(logOut io.Writer) (config.Config, *slog.Logger, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, err
	}
	level, _ := cfg.SlogLevel()
	log := slogx.New(logOut, level, cfg.IsDev())
	slog.SetDefault(log)
	return cfg, log, nil
}

// openPool connects to Postgres, reporting query spans and pool metrics to tel.
func openPool(ctx context.Context, cfg config.Config, tel *otelx.Telemetry) (*pgxpool.Pool, error) {
	return db.NewPool(ctx, cfg.DatabaseURL, db.PoolOptions{
		MaxConns:        cfg.DB.MaxConns,
		MinConns:        cfg.DB.MinConns,
		MaxConnLifetime: cfg.DB.MaxConnLifetime,
		MaxConnIdleTime: cfg.DB.MaxConnIdleTime,
		ConnectTimeout:  cfg.DB.ConnectTimeout,
		TracerProvider:  tel.TracerProvider,
		MeterProvider:   tel.MeterProvider,
	})
}

func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, log, err := loadConfig(os.Stdout)
	if err != nil {
		return err
	}

	serviceVersion := otelx.Version(version)
	tel, err := otelx.Setup(ctx, otelx.Config{
		Enabled:        cfg.Telemetry.Enabled,
		ServiceName:    serviceName,
		ServiceVersion: serviceVersion,
		Environment:    cfg.Env,
		Log:            log,
	})
	if err != nil {
		return err
	}
	// Deferred first so it runs last: after the server has drained and the
	// click buffer has flushed, so their final spans are exported too. The
	// signal context is cancelled by then, hence a fresh budget, kept short
	// because Server.Shutdown may already have spent twice SHUTDOWN_TIMEOUT
	// and the orchestrator's grace period is counting. Telemetry is best
	// effort; a collector that is down at exit is logged, not fatal.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(),
			min(cfg.ShutdownTimeout, maxTelemetryShutdown))
		defer cancel()
		if err := tel.Shutdown(shutdownCtx); err != nil {
			log.Warn("telemetry shutdown", "err", err)
		}
	}()
	if cfg.Telemetry.Enabled {
		log.Info("telemetry enabled: exporting traces and metrics over OTLP gRPC",
			"service_version", serviceVersion)
	}

	pool, err := openPool(ctx, cfg, tel)
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

	srv := app.New(cfg, log, pool, app.WithTelemetry(tel.TracerProvider, tel.MeterProvider))
	if len(cfg.TrustedProxies) == 0 {
		log.Info("no TRUSTED_PROXIES set: client IP is the TCP peer")
	} else {
		log.Info("trusting X-Forwarded-For from proxies", "cidrs", cfg.TrustedProxies)
	}
	return srv.Run(ctx)
}

func createAPIKey(name string, stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, log, pool, err := bootstrap(ctx, os.Stderr)
	if err != nil {
		return err
	}
	defer pool.Close()

	svc := apikey.NewService(apikey.NewRepository(sqlc.New(pool)))
	k, raw, err := svc.Create(ctx, name)
	if err != nil {
		return err
	}
	log.Info("api key created", "id", k.ID, "name", k.Name)
	// The raw key is written once to stdout and nowhere else.
	if _, err := fmt.Fprintln(stdout, raw); err != nil {
		return fmt.Errorf("print api key: %w", err)
	}
	return nil
}

// purgeClicks is the cron-friendly form of the in-process purger, for
// deployments that set CLICK_PURGE_INTERVAL=0 and schedule retention
// externally.
func purgeClicks() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, log, pool, err := bootstrap(ctx, os.Stdout)
	if err != nil {
		return err
	}
	defer pool.Close()

	svc := click.NewService(click.NewRepository(pool, sqlc.New(pool)), log)
	n, err := svc.Purge(ctx, cfg.Clicks.Retention)
	if err != nil {
		return fmt.Errorf("purge clicks (deleted %d): %w", n, err)
	}
	log.Info("clicks purged", "deleted", n, "retention", cfg.Clicks.Retention)
	return nil
}
