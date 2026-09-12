package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const validYAML = `
domain: agents.example.dev
listen_addr: ":8443"
sqlite_path: "./cdampd.db"
signing_key_passphrase_env: "CDAMPD_KEY_PASSPHRASE"
rate_limit: { per_domain_rps: 5, burst: 20 }
retry_schedule: [1m, 5m, 30m, 2h, 12h]
message: { max_body_bytes: 262144, default_ttl: 168h }
archive_after: 2160h
key_rotation_grace: 720h
directory_cache_ttl: 1h
`

// writeTempConfig writes contents to a temp file and returns its path.
func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cdampd.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoad_ValidFile(t *testing.T) {
	path := writeTempConfig(t, validYAML)
	t.Setenv("CDAMPD_KEY_PASSPHRASE", "hunter2")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"Domain", cfg.Domain, "agents.example.dev"},
		{"ListenAddr", cfg.ListenAddr, ":8443"},
		{"SQLitePath", cfg.SQLitePath, "./cdampd.db"},
		{"SigningKeyPassphraseEnv", cfg.SigningKeyPassphraseEnv, "CDAMPD_KEY_PASSPHRASE"},
		{"SigningKeyPassphrase", cfg.SigningKeyPassphrase, "hunter2"},
		{"RateLimit.PerDomainRPS", cfg.RateLimit.PerDomainRPS, 5},
		{"RateLimit.Burst", cfg.RateLimit.Burst, 20},
		{"Message.MaxBodyBytes", cfg.Message.MaxBodyBytes, int64(262144)},
		{"Message.DefaultTTL", cfg.Message.DefaultTTL, 168 * time.Hour},
		{"ArchiveAfter", cfg.ArchiveAfter, 2160 * time.Hour},
		{"KeyRotationGrace", cfg.KeyRotationGrace, 720 * time.Hour},
		{"DirectoryCacheTTL", cfg.DirectoryCacheTTL, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}

	wantSchedule := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}
	if len(cfg.RetrySchedule) != len(wantSchedule) {
		t.Fatalf("RetrySchedule = %v, want %v", cfg.RetrySchedule, wantSchedule)
	}
	for i, want := range wantSchedule {
		if cfg.RetrySchedule[i] != want {
			t.Errorf("RetrySchedule[%d] = %v, want %v", i, cfg.RetrySchedule[i], want)
		}
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load with a missing file returned nil error, want a wrapped error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestLoad_EnvOverridePrecedence(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
		envVal string
		get    func(*Config) any
		want   any
	}{
		{
			name:   "CDAMPD_DOMAIN overrides domain",
			envVar: "CDAMPD_DOMAIN",
			envVal: "override.example.dev",
			get:    func(c *Config) any { return c.Domain },
			want:   "override.example.dev",
		},
		{
			name:   "CDAMPD_LISTEN_ADDR overrides listen_addr",
			envVar: "CDAMPD_LISTEN_ADDR",
			envVal: ":9999",
			get:    func(c *Config) any { return c.ListenAddr },
			want:   ":9999",
		},
		{
			name:   "CDAMPD_SQLITE_PATH overrides sqlite_path",
			envVar: "CDAMPD_SQLITE_PATH",
			envVal: "/var/lib/cdampd/other.db",
			get:    func(c *Config) any { return c.SQLitePath },
			want:   "/var/lib/cdampd/other.db",
		},
		{
			name:   "CDAMPD_ARCHIVE_AFTER overrides archive_after",
			envVar: "CDAMPD_ARCHIVE_AFTER",
			envVal: "48h",
			get:    func(c *Config) any { return c.ArchiveAfter },
			want:   48 * time.Hour,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, validYAML)
			t.Setenv("CDAMPD_KEY_PASSPHRASE", "hunter2")
			t.Setenv(tc.envVar, tc.envVal)

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load returned unexpected error: %v", err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Errorf("%s = %v, want %v (env should win over YAML)", tc.envVar, got, tc.want)
			}
		})
	}
}

func TestLoad_PassphraseIndirection(t *testing.T) {
	path := writeTempConfig(t, validYAML)
	// The YAML file names CDAMPD_KEY_PASSPHRASE as the passphrase env var;
	// set that named var and confirm Load resolves it into the passphrase
	// field, never reading a passphrase from the YAML file itself.
	t.Setenv("CDAMPD_KEY_PASSPHRASE", "indirect-secret")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg.SigningKeyPassphrase != "indirect-secret" {
		t.Errorf("SigningKeyPassphrase = %q, want %q", cfg.SigningKeyPassphrase, "indirect-secret")
	}
}

func TestLoad_PassphraseIndirection_EnvOverridesWhichVarNameIsUsed(t *testing.T) {
	path := writeTempConfig(t, validYAML)
	// Override which env var names the passphrase, then confirm the
	// resolved passphrase comes from the overridden name, not the
	// YAML-specified one.
	t.Setenv("CDAMPD_SIGNING_KEY_PASSPHRASE_ENV", "OTHER_PASSPHRASE_VAR")
	t.Setenv("OTHER_PASSPHRASE_VAR", "other-secret")
	t.Setenv("CDAMPD_KEY_PASSPHRASE", "should-not-be-used")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg.SigningKeyPassphraseEnv != "OTHER_PASSPHRASE_VAR" {
		t.Errorf("SigningKeyPassphraseEnv = %q, want %q", cfg.SigningKeyPassphraseEnv, "OTHER_PASSPHRASE_VAR")
	}
	if cfg.SigningKeyPassphrase != "other-secret" {
		t.Errorf("SigningKeyPassphrase = %q, want %q", cfg.SigningKeyPassphrase, "other-secret")
	}
}
