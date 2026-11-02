package config

import "testing"

const testDSN = "postgres://u:p@localhost:5432/db?sslmode=disable"

// clearEnv unsets every variable Load reads so a developer's .env (loaded by
// the Taskfile) cannot leak into the test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"APP_ENV", "PORT", "BASE_URL", "LOG_LEVEL", "SHUTDOWN_TIMEOUT", "REQUEST_TIMEOUT",
		"DATABASE_URL", "MIGRATE_ON_START", "DB_MAX_CONNS", "DB_MIN_CONNS",
		"DB_MAX_CONN_LIFETIME", "DB_MAX_CONN_IDLE_TIME", "DB_CONNECT_TIMEOUT",
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
