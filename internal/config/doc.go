// Package config loads cdampd's single YAML config file plus environment
// variable overrides (e.g. the signing-key passphrase, never stored in
// the file itself) into a typed struct, and sets up the process-wide
// log/slog logger. No config service, no hot reload — per
// 02-ARCHITECTURE.md, a config change requires a restart.
package config
