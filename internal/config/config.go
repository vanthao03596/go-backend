// Package config loads the typed application configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds every runtime setting. Fields are populated from the environment
// by Load; nothing else in the program reads os.Getenv.
type Config struct {
	Env             string        `env:"APP_ENV"          envDefault:"dev"`
	Port            int           `env:"PORT"             envDefault:"8080"`
	BaseURL         string        `env:"BASE_URL"         envDefault:"http://localhost:8080"`
	LogLevel        string        `env:"LOG_LEVEL"        envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`
	RequestTimeout  time.Duration `env:"REQUEST_TIMEOUT"  envDefault:"15s"`

	DatabaseURL    string `env:"DATABASE_URL,required,notEmpty"`
	MigrateOnStart bool   `env:"MIGRATE_ON_START"  envDefault:"false"`
	DB             DBConfig

	// DevAPIKeyID, when non-zero, makes every management API request act as
	// this api_keys.id without authentication. Only honoured when Env is dev.
	// Removed in roadmap phase 3 once bearer keys exist.
	DevAPIKeyID int64 `env:"DEV_API_KEY_ID" envDefault:"0"`
}

// DBConfig tunes the pgx connection pool. Zero values keep pgx defaults.
type DBConfig struct {
	MaxConns        int32         `env:"DB_MAX_CONNS"          envDefault:"10"`
	MinConns        int32         `env:"DB_MIN_CONNS"          envDefault:"2"`
	MaxConnLifetime time.Duration `env:"DB_MAX_CONN_LIFETIME"  envDefault:"1h"`
	MaxConnIdleTime time.Duration `env:"DB_MAX_CONN_IDLE_TIME" envDefault:"30m"`
	ConnectTimeout  time.Duration `env:"DB_CONNECT_TIMEOUT"    envDefault:"5s"`
}

// Load parses the environment into a Config and validates it.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535, got %d", c.Port)
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	switch c.Env {
	case "dev", "test", "prod":
	default:
		return fmt.Errorf("APP_ENV must be dev, test or prod, got %q", c.Env)
	}
	if c.DB.MinConns > c.DB.MaxConns {
		return fmt.Errorf("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", c.DB.MinConns, c.DB.MaxConns)
	}
	if c.DevAPIKeyID != 0 && !c.IsDev() {
		return fmt.Errorf("DEV_API_KEY_ID is only allowed when APP_ENV=dev, got APP_ENV=%s", c.Env)
	}
	return nil
}

// IsDev reports whether the service runs in local development mode.
func (c Config) IsDev() bool { return c.Env == "dev" }

// Addr returns the listen address for the HTTP server.
func (c Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }

// SlogLevel converts LogLevel into a slog.Level.
func (c Config) SlogLevel() (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(c.LogLevel))); err != nil {
		return 0, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel)
	}
	return lvl, nil
}
