// Package config loads the typed application configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/netip"
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

	// TrustedProxies lists the CIDR ranges of reverse proxies allowed to set
	// X-Forwarded-For. Empty means the TCP peer is the client. Deliberately a
	// CIDR list rather than a boolean: trusting the header from any peer lets
	// a direct caller forge its IP and escape or weaponise rate limits.
	TrustedProxies []string `env:"TRUSTED_PROXIES" envSeparator:","`
	RateLimit      RateLimitConfig
}

// RateLimitConfig sets per-client-IP request budgets, each over a sliding
// one second window.
type RateLimitConfig struct {
	// CreatePerSecond applies to POST /api/v1/links.
	CreatePerSecond int `env:"RATE_LIMIT_CREATE_PER_SEC"   envDefault:"10"`
	// RedirectPerSecond applies to GET and HEAD /{code}.
	RedirectPerSecond int `env:"RATE_LIMIT_REDIRECT_PER_SEC" envDefault:"100"`
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
	for _, cidr := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("TRUSTED_PROXIES entry %q is not a CIDR prefix: %w", cidr, err)
		}
	}
	if c.RateLimit.CreatePerSecond <= 0 || c.RateLimit.RedirectPerSecond <= 0 {
		return fmt.Errorf("rate limits must be positive, got create=%d redirect=%d",
			c.RateLimit.CreatePerSecond, c.RateLimit.RedirectPerSecond)
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
