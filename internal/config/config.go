// Package config loads the typed application configuration from environment variables.
package config

import (
	"errors"
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

	// DatabaseURL has no default and is required; validate enforces it so
	// that Defaults can still describe an empty environment.
	DatabaseURL    string `env:"DATABASE_URL"`
	MigrateOnStart bool   `env:"MIGRATE_ON_START"  envDefault:"false"`
	DB             DBConfig

	// CursorSecret keys the HMAC that signs list pagination cursors. It must
	// be at least MinCursorSecretLen bytes and shared by every replica.
	// Required in prod; elsewhere an empty value means a random per-process
	// key, so cursors stop working across restarts.
	CursorSecret string `env:"CURSOR_SECRET"`

	// TrustedProxies lists the CIDR ranges of reverse proxies allowed to set
	// X-Forwarded-For. Empty means the TCP peer is the client. Deliberately a
	// CIDR list rather than a boolean: trusting the header from any peer lets
	// a direct caller forge its IP and escape or weaponise rate limits.
	TrustedProxies []string `env:"TRUSTED_PROXIES" envSeparator:","`
	RateLimit      RateLimitConfig
	Clicks         ClickConfig
	Cache          CacheConfig
	Telemetry      TelemetryConfig
}

// MinCursorSecretLen is the shortest CURSOR_SECRET accepted: 32 bytes, the
// output size of SHA-256, so the HMAC key is as strong as the hash.
const MinCursorSecretLen = 32

// CacheConfig enables the Redis cache in front of redirect lookups.
type CacheConfig struct {
	// RedisURL is a go-redis URL (redis://user:pass@host:6379/0?opt=v).
	// Empty disables the cache and every redirect reads Postgres. Timeouts
	// and pool settings may be given as query parameters; unset ones get
	// cache-friendly defaults, see platform/redisx.
	RedisURL string `env:"REDIS_URL"`
	// TTL bounds how long a cached destination lives. Entries are kept
	// coherent on create and delete, so the TTL only limits staleness when
	// such an update fails to reach Redis, and bounds memory.
	TTL time.Duration `env:"LINK_CACHE_TTL" envDefault:"10m"`
	// NegativeTTL is how long an unknown code is remembered as missing.
	NegativeTTL time.Duration `env:"LINK_CACHE_NEGATIVE_TTL" envDefault:"60s"`
}

// Enabled reports whether a Redis URL was configured.
func (c CacheConfig) Enabled() bool { return c.RedisURL != "" }

// TelemetryConfig switches OpenTelemetry export on. Everything else (OTLP
// endpoint, headers, sampler, resource attributes, export intervals) is read
// by the OpenTelemetry SDK itself from the standard OTEL_* variables, so it
// is deliberately not duplicated here.
type TelemetryConfig struct {
	// Enabled exports traces and metrics over OTLP gRPC. Off by default so a
	// process without a collector neither dials one nor logs export errors.
	// The Go SDK does not implement OTEL_SDK_DISABLED, hence an app flag.
	Enabled bool `env:"TELEMETRY_ENABLED" envDefault:"false"`
}

// ClickConfig tunes click analytics: the in-process buffer that takes click
// writes off the redirect path, and retention of the clicks table.
type ClickConfig struct {
	// BufferSize is how many clicks may wait for a flush. Beyond it clicks
	// are dropped rather than slowing redirects.
	BufferSize int `env:"CLICK_BUFFER_SIZE" envDefault:"10000"`
	// FlushInterval bounds how long a click waits before it is written.
	FlushInterval time.Duration `env:"CLICK_FLUSH_INTERVAL" envDefault:"1s"`
	// FlushBatch flushes early once this many clicks are pending.
	FlushBatch int `env:"CLICK_FLUSH_BATCH" envDefault:"500"`
	// Retention is how long clicks rows are kept. The lifetime counter on
	// the link is unaffected.
	Retention time.Duration `env:"CLICK_RETENTION" envDefault:"2160h"`
	// PurgeInterval is how often the in-process purger runs. Zero disables
	// it; run "api clicks purge" from an external scheduler instead.
	PurgeInterval time.Duration `env:"CLICK_PURGE_INTERVAL" envDefault:"24h"`
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

// Defaults returns the configuration an empty environment produces. It is
// not validated (DATABASE_URL is empty), so it only suits building the
// application without serving, as the OpenAPI export does: that output must
// not depend on whoever runs it.
func Defaults() Config {
	cfg, err := env.ParseAsWithOptions[Config](env.Options{Environment: map[string]string{}})
	if err != nil {
		// Only a malformed envDefault tag can fail here, a programming error
		// that TestDefaults catches.
		panic(fmt.Sprintf("config defaults: %v", err))
	}
	return cfg
}

func (c Config) validate() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
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
	if c.Clicks.BufferSize <= 0 || c.Clicks.FlushBatch <= 0 || c.Clicks.FlushInterval <= 0 {
		return fmt.Errorf("CLICK_BUFFER_SIZE, CLICK_FLUSH_BATCH and CLICK_FLUSH_INTERVAL must be positive, got %d, %d, %s",
			c.Clicks.BufferSize, c.Clicks.FlushBatch, c.Clicks.FlushInterval)
	}
	if c.Clicks.Retention <= 0 {
		return fmt.Errorf("CLICK_RETENTION must be positive, got %s", c.Clicks.Retention)
	}
	if c.Clicks.PurgeInterval < 0 {
		return fmt.Errorf("CLICK_PURGE_INTERVAL must be zero or positive, got %s", c.Clicks.PurgeInterval)
	}
	if c.Cache.TTL <= 0 || c.Cache.NegativeTTL <= 0 {
		return fmt.Errorf("LINK_CACHE_TTL and LINK_CACHE_NEGATIVE_TTL must be positive, got %s and %s",
			c.Cache.TTL, c.Cache.NegativeTTL)
	}
	switch {
	case c.CursorSecret == "" && c.Env == "prod":
		return errors.New("CURSOR_SECRET is required when APP_ENV=prod")
	case c.CursorSecret != "" && len(c.CursorSecret) < MinCursorSecretLen:
		return fmt.Errorf("CURSOR_SECRET must be at least %d bytes, got %d", MinCursorSecretLen, len(c.CursorSecret))
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
