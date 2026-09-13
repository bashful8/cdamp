package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is cdampd's fully-resolved configuration: values loaded from the
// single YAML file (02-ARCHITECTURE.md's "Config" section), overridden by
// CDAMPD_* environment variables where set, with the signing-key
// passphrase resolved from the environment variable it names. There is no
// config service and no hot reload — a config change requires a restart.
type Config struct {
	Domain        string `yaml:"domain"`
	ListenAddr    string `yaml:"listen_addr"`
	AdminBindAddr string `yaml:"admin_bind_addr"`
	SQLitePath    string `yaml:"sqlite_path"`

	// SigningKeyPassphraseEnv names the environment variable that holds
	// the signing-key passphrase. The passphrase itself is never stored
	// in the config file — see SigningKeyPassphrase, resolved by Load.
	SigningKeyPassphraseEnv string `yaml:"signing_key_passphrase_env"`

	RateLimit struct {
		PerDomainRPS int `yaml:"per_domain_rps"`
		Burst        int `yaml:"burst"`
	} `yaml:"rate_limit"`

	RetrySchedule []time.Duration `yaml:"retry_schedule"`

	Message struct {
		MaxBodyBytes int64         `yaml:"max_body_bytes"`
		DefaultTTL   time.Duration `yaml:"default_ttl"`
	} `yaml:"message"`

	ArchiveAfter      time.Duration `yaml:"archive_after"`
	KeyRotationGrace  time.Duration `yaml:"key_rotation_grace"`
	DirectoryCacheTTL time.Duration `yaml:"directory_cache_ttl"`

	// SigningKeyPassphrase is resolved at Load time from the environment
	// variable named by SigningKeyPassphraseEnv (after env overrides are
	// applied to that name). It is never populated from the YAML file
	// directly and must never be logged.
	SigningKeyPassphrase string `yaml:"-"`
}

// rawConfig mirrors Config's YAML shape but keeps duration fields as
// plain strings, since yaml.v3 cannot unmarshal Go duration strings
// (e.g. "1m", "168h") directly into time.Duration.
type rawConfig struct {
	Domain                  string `yaml:"domain"`
	ListenAddr              string `yaml:"listen_addr"`
	AdminBindAddr           string `yaml:"admin_bind_addr"`
	SQLitePath              string `yaml:"sqlite_path"`
	SigningKeyPassphraseEnv string `yaml:"signing_key_passphrase_env"`

	RateLimit struct {
		PerDomainRPS int `yaml:"per_domain_rps"`
		Burst        int `yaml:"burst"`
	} `yaml:"rate_limit"`

	RetrySchedule []string `yaml:"retry_schedule"`

	Message struct {
		MaxBodyBytes int64  `yaml:"max_body_bytes"`
		DefaultTTL   string `yaml:"default_ttl"`
	} `yaml:"message"`

	ArchiveAfter      string `yaml:"archive_after"`
	KeyRotationGrace  string `yaml:"key_rotation_grace"`
	DirectoryCacheTTL string `yaml:"directory_cache_ttl"`
}

// UnmarshalYAML implements yaml.Unmarshaler so duration fields given as Go
// duration strings parse into time.Duration values.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	var raw rawConfig
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("decoding config yaml: %w", err)
	}

	c.Domain = raw.Domain
	c.ListenAddr = raw.ListenAddr
	c.AdminBindAddr = raw.AdminBindAddr
	c.SQLitePath = raw.SQLitePath
	c.SigningKeyPassphraseEnv = raw.SigningKeyPassphraseEnv
	c.RateLimit.PerDomainRPS = raw.RateLimit.PerDomainRPS
	c.RateLimit.Burst = raw.RateLimit.Burst
	c.Message.MaxBodyBytes = raw.Message.MaxBodyBytes

	var err error
	if c.RetrySchedule, err = parseDurations("retry_schedule", raw.RetrySchedule); err != nil {
		return err
	}
	if c.Message.DefaultTTL, err = parseDuration("message.default_ttl", raw.Message.DefaultTTL); err != nil {
		return err
	}
	if c.ArchiveAfter, err = parseDuration("archive_after", raw.ArchiveAfter); err != nil {
		return err
	}
	if c.KeyRotationGrace, err = parseDuration("key_rotation_grace", raw.KeyRotationGrace); err != nil {
		return err
	}
	if c.DirectoryCacheTTL, err = parseDuration("directory_cache_ttl", raw.DirectoryCacheTTL); err != nil {
		return err
	}
	return nil
}

// parseDuration parses s as a Go duration string, returning a wrapped
// error naming the field on failure. An empty string yields a zero
// duration rather than an error, so an unset field is simply zero-valued.
func parseDuration(field, s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing %s %q: %w", field, s, err)
	}
	return d, nil
}

// parseDurations parses each element of ss as a Go duration string.
func parseDurations(field string, ss []string) ([]time.Duration, error) {
	if ss == nil {
		return nil, nil
	}
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		d, err := parseDuration(fmt.Sprintf("%s[%d]", field, i), s)
		if err != nil {
			return nil, err
		}
		out[i] = d
	}
	return out, nil
}

// defaultAdminBindAddr is used when admin_bind_addr is left unset in the
// YAML file and no CDAMPD_ADMIN_BIND_ADDR override is given — implements
// the Gap 2 decision's "defaulting to 127.0.0.1:<port>" literally. Not
// derived from ListenAddr's own port: two http.Servers cannot share one
// bind address, so this must be a distinct, fixed port, not a formula.
// Not pinned by any doc — a builder judgment call in the same low-risk
// category as tokenBytes/shutdownTimeout/adminCookieName.
const defaultAdminBindAddr = "127.0.0.1:8444"

// Load reads the YAML config file at path, applies CDAMPD_*-style
// environment variable overrides on top of it, then resolves the
// signing-key passphrase from the environment variable named by
// SigningKeyPassphraseEnv. A missing or malformed file yields a wrapped
// error, never a panic.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	if err := applyEnvOverrides(&cfg); err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	if cfg.AdminBindAddr == "" {
		cfg.AdminBindAddr = defaultAdminBindAddr
	}

	// Never stored in the YAML file itself: resolved from whichever env
	// var SigningKeyPassphraseEnv names, after overrides above have been
	// applied to that name.
	cfg.SigningKeyPassphrase = os.Getenv(cfg.SigningKeyPassphraseEnv)

	return &cfg, nil
}

// applyEnvOverrides overlays CDAMPD_<FIELD> environment variables onto
// cfg's top-level scalar fields, wherever such a variable is set to a
// non-empty value. 02-ARCHITECTURE.md's Config section only pins down the
// passphrase-env indirection by name; this per-field naming convention is
// a builder judgment call applied consistently across the remaining
// top-level scalars (nested rate_limit/message fields and the
// retry_schedule slice are left to the YAML file, since the docs don't
// call for overriding those).
func applyEnvOverrides(cfg *Config) error {
	if v, ok := lookupEnv("CDAMPD_DOMAIN"); ok {
		cfg.Domain = v
	}
	if v, ok := lookupEnv("CDAMPD_LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
	}
	if v, ok := lookupEnv("CDAMPD_ADMIN_BIND_ADDR"); ok {
		cfg.AdminBindAddr = v
	}
	if v, ok := lookupEnv("CDAMPD_SQLITE_PATH"); ok {
		cfg.SQLitePath = v
	}
	if v, ok := lookupEnv("CDAMPD_SIGNING_KEY_PASSPHRASE_ENV"); ok {
		cfg.SigningKeyPassphraseEnv = v
	}

	durationOverrides := []struct {
		env   string
		field string
		dst   *time.Duration
	}{
		{"CDAMPD_ARCHIVE_AFTER", "archive_after", &cfg.ArchiveAfter},
		{"CDAMPD_KEY_ROTATION_GRACE", "key_rotation_grace", &cfg.KeyRotationGrace},
		{"CDAMPD_DIRECTORY_CACHE_TTL", "directory_cache_ttl", &cfg.DirectoryCacheTTL},
	}
	for _, o := range durationOverrides {
		v, ok := lookupEnv(o.env)
		if !ok {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parsing %s %q: %w", o.env, v, err)
		}
		*o.dst = d
	}

	return nil
}

// lookupEnv returns the named environment variable and whether it is set
// to a non-empty value, treating unset and empty the same way so an
// override only takes effect when explicitly given a value.
func lookupEnv(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// NewLogger constructs cdampd's process-wide structured logger, writing
// JSON-formatted records to w at the given level. JSON is chosen over the
// text handler as the more machine-parseable default for a daemon's logs;
// 04-BUILD-PLAN.md requires structured log/slog output but does not pin
// down a handler format. Wiring this into cmd/cdampd/main.go is a
// follow-on task.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
