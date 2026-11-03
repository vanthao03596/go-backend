package config

import (
	"strings"
	"testing"
	"time"
)

const testDSN = "postgres://u:p@localhost:5432/db?sslmode=disable"

// clearEnv unsets every variable Load reads so a developer's .env (loaded by
// the Taskfile) cannot leak into the test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"APP_ENV", "PORT", "BASE_URL", "LOG_LEVEL", "SHUTDOWN_TIMEOUT", "REQUEST_TIMEOUT",
		"DATABASE_URL", "MIGRATE_ON_START", "DB_MAX_CONNS", "DB_MIN_CONNS",
		"DB_MAX_CONN_LIFETIME", "DB_MAX_CONN_IDLE_TIME", "DB_CONNECT_TIMEOUT",
		"TRUSTED_PROXIES", "RATE_LIMIT_CREATE_PER_SEC", "RATE_LIMIT_REDIRECT_PER_SEC",
		"TELEMETRY_ENABLED", "CURSOR_SECRET", "REDIS_URL", "LINK_CACHE_TTL", "LINK_CACHE_NEGATIVE_TTL",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", testDSN)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if !cfg.IsDev() {
		t.Errorf("Env = %q, want dev", cfg.Env)
	}
	if cfg.Addr() != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr())
	}
	if cfg.MigrateOnStart {
		t.Error("MigrateOnStart should default to false")
	}
	if cfg.DB.MaxConns != 10 || cfg.DB.MinConns != 2 {
		t.Errorf("DB pool defaults = %d/%d, want 10/2", cfg.DB.MaxConns, cfg.DB.MinConns)
	}
	if cfg.Telemetry.Enabled {
		t.Error("Telemetry.Enabled should default to false")
	}
	if cfg.Cache.Enabled() || cfg.Cache.TTL != 10*time.Minute || cfg.Cache.NegativeTTL != time.Minute {
		t.Errorf("cache defaults = %+v, want disabled with 10m/60s TTLs", cfg.Cache)
	}
}

func TestDefaultsIgnoreTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("BASE_URL", "https://sho.rt")
	t.Setenv("REDIS_URL", "redis://cache:6379")

	cfg := Defaults()
	if cfg.BaseURL != "http://localhost:8080" || cfg.Cache.Enabled() {
		t.Errorf("Defaults read the environment: BaseURL=%q RedisURL=%q", cfg.BaseURL, cfg.Cache.RedisURL)
	}
	if cfg.RateLimit.RedirectPerSecond != 100 || cfg.Clicks.BufferSize != 10000 {
		t.Errorf("Defaults lost envDefault values: %+v %+v", cfg.RateLimit, cfg.Clicks)
	}
}

func TestCursorSecret(t *testing.T) {
	cases := map[string]struct {
		env, secret string
		ok          bool
	}{
		"dev without secret":   {"dev", "", true},
		"prod without secret":  {"prod", "", false},
		"prod with short":      {"prod", strings.Repeat("s", MinCursorSecretLen-1), false},
		"prod with min length": {"prod", strings.Repeat("s", MinCursorSecretLen), true},
		"dev with short":       {"dev", "short", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", testDSN)
			t.Setenv("APP_ENV", tc.env)
			t.Setenv("CURSOR_SECRET", tc.secret)
			_, err := Load()
			if (err == nil) != tc.ok {
				t.Fatalf("Load = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestTrustedProxiesParseAsList(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", testDSN)
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8,::1/128")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[1] != "::1/128" {
		t.Errorf("TrustedProxies = %v", cfg.TrustedProxies)
	}
	if cfg.RateLimit.CreatePerSecond != 10 || cfg.RateLimit.RedirectPerSecond != 100 {
		t.Errorf("rate limit defaults = %+v", cfg.RateLimit)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	clearEnv(t)
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted empty DATABASE_URL")
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := map[string]struct{ key, val string }{
		"port out of range": {"PORT", "70000"},
		"unknown env":       {"APP_ENV", "staging"},
		"unknown log level": {"LOG_LEVEL", "verbose"},
		"min conns > max":   {"DB_MIN_CONNS", "50"},
		"proxy not a cidr":  {"TRUSTED_PROXIES", "10.0.0.1"},
		"zero rate limit":   {"RATE_LIMIT_CREATE_PER_SEC", "0"},
		"zero cache ttl":    {"LINK_CACHE_TTL", "0s"},
		"negative 404 ttl":  {"LINK_CACHE_NEGATIVE_TTL", "-1s"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", testDSN)
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %s=%s", tc.key, tc.val)
			}
		})
	}
}
