// Package sqlite implements internal/domain's InboxStore port and
// signing-key persistence on top of a single SQLite file (via
// modernc.org/sqlite, WAL mode, no cgo), with schema migrations under
// migrations/ managed by golang-migrate.
package sqlite
