// Package sqlite's archive.go implements Phase 8 task 4: the periodic
// archival job that moves read=1 messages older than cfg.ArchiveAfter
// (90 days by default -- internal/config.defaultArchiveAfter) out of the
// live database into a separate archive.db file, per 03-API.md's schema
// section and 02-ARCHITECTURE.md's Deployment section ("Old read
// messages move to a separate archive.db after 90 days" --
// 00-OVERVIEW.md calls this "the entire scaling story", the only scaling
// mechanism this project has; no sharding). See STATUS.md's Phase 8 task
// 4 spec for the full design reasoning, especially design decisions 3-6
// and 8 (schema-creation strategy, ATTACH DATABASE usage, why no
// domain.Archiver port exists).
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

//go:embed archive_schema.sql
var archiveSchemaSQL string

// archiveFilename is the fixed name of the separate archive database
// file, always created alongside the main SQLite file in the same
// directory. See STATUS.md's Phase 8 task 4 spec, design decision 1, for
// why this is derived from the main db's own path rather than a new
// config field.
const archiveFilename = "archive.db"

// archiveDBPath returns the archive.db path that sits alongside mainPath
// (the main SQLite file's own path).
func archiveDBPath(mainPath string) string {
	return filepath.Join(filepath.Dir(mainPath), archiveFilename)
}

// ensureArchiveSchema creates archive.db's tables/indexes/FTS5 table/
// triggers if they don't already exist, against db (a plain, dedicated
// *sql.DB opened on the archive file -- not an ATTACHed alias). See
// STATUS.md's Phase 8 task 4 spec, design decision 3.
func ensureArchiveSchema(db *sql.DB) error {
	if _, err := db.Exec(archiveSchemaSQL); err != nil {
		return fmt.Errorf("creating archive schema: %w", err)
	}
	return nil
}

// archiveTickInterval is how often Archiver.Run attempts a move pass.
// Not pinned by any doc -- only the 90-day archive_after *threshold* is
// pinned; the tick *frequency* isn't. See STATUS.md's Phase 8 task 4
// spec, design decision 7.
const archiveTickInterval = time.Hour

// Archiver is the background job that moves read=1 messages older than
// archiveAfter out of store's live "messages" table into store's
// archive.db. Implements no domain port -- see STATUS.md's Phase 8 task 4
// spec, design decision 6: this is delivery.Worker's/
// httpadapter.DomainLimiters' own shape (a concrete adapter type
// constructed directly in main.go, started via "go x.Run(ctx)"), not
// signing.Rotator's (which needed a port because internal/adapters/http
// had to invoke it).
type Archiver struct {
	store        *Store
	archiveAfter time.Duration

	// now is an injectable time source, overridable only from this
	// package's own tests, mirroring delivery.Worker's own "now" field.
	// Defaults to time.Now in NewArchiver.
	now func() time.Time
}

// NewArchiver returns an Archiver that moves store's read=1 messages
// older than archiveAfter (cfg.ArchiveAfter) into store's archive
// database.
func NewArchiver(store *Store, archiveAfter time.Duration) *Archiver {
	return &Archiver{store: store, archiveAfter: archiveAfter, now: time.Now}
}

// Run blocks, performing one archive pass immediately, then again every
// archiveTickInterval, until ctx is canceled -- the same lifecycle shape
// as delivery.Worker.Run/httpadapter.DomainLimiters.Run: an immediate
// first pass, a ticker loop, a select on ctx.Done()/ticker.C, and a
// logged-not-fatal error on failure so one bad pass never crashes the
// daemon.
func (a *Archiver) Run(ctx context.Context) {
	if _, err := a.archiveOnce(ctx); err != nil {
		slog.Default().Error("archiver: archive pass failed", "error", err)
	}

	ticker := time.NewTicker(archiveTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := a.archiveOnce(ctx); err != nil {
				slog.Default().Error("archiver: archive pass failed", "error", err)
			}
		}
	}
}

// archiveOnce performs exactly one move pass: every message with read=1
// and sent_at at or before the cutoff (now - archiveAfter) is copied into
// archive.messages (its owning thread mirrored into archive.threads) and
// deleted from the live messages table, all inside one transaction on a
// single connection with archive.db ATTACHed -- see STATUS.md's Phase 8
// task 4 spec, design decision 8. Returns the number of messages moved.
// Unexported but directly callable from this package's own tests, the
// same "test the method, not Run's ticker" pattern as delivery.Worker's
// runOnce.
func (a *Archiver) archiveOnce(ctx context.Context) (moved int, err error) {
	if a.archiveAfter <= 0 {
		// Defensive only: internal/config.Load always defaults
		// ArchiveAfter to a positive value (design decision 2), so this
		// branch is unreachable through normal wiring -- it guards a
		// hand-built Archiver (e.g. in a test) against archiving every
		// read message unconditionally.
		return 0, nil
	}
	cutoff := a.now().Add(-a.archiveAfter).Unix()

	conn, err := a.store.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("archiving messages: getting connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returns the connection to the pool

	attachSQL := "ATTACH DATABASE " + sqliteQuoteLiteral(a.store.archivePath) + " AS archive"
	if _, err := conn.ExecContext(ctx, attachSQL); err != nil {
		return 0, fmt.Errorf("archiving messages: attaching archive database: %w", err)
	}
	// Detach before the connection is returned to the pool by the
	// deferred conn.Close() above -- defers run LIFO, so this runs
	// first. See design decision 8 for why this matters (a pooled
	// connection reused by a later, unrelated caller must not still have
	// "archive" attached).
	defer func() {
		if _, derr := conn.ExecContext(context.Background(), "DETACH DATABASE archive"); derr != nil {
			slog.Default().Error("archiver: detaching archive database failed", "error", derr)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("archiving messages: beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Mirror the owning thread row into archive.threads -- structural
	// parity only (design decision 4); no read path relies on it, since
	// thread metadata is always read from main (design decision 9).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO archive.threads (id, subject, created_at)
		SELECT id, subject, created_at FROM main.threads
		WHERE id IN (
			SELECT DISTINCT thread_id FROM main.messages
			WHERE read = 1 AND sent_at <= ?
		)
		ON CONFLICT(id) DO NOTHING
	`, cutoff); err != nil {
		return 0, fmt.Errorf("archiving messages: mirroring threads: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO archive.messages (`+messageColumns+`)
		SELECT `+messageColumns+` FROM main.messages
		WHERE read = 1 AND sent_at <= ?
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("archiving messages: copying messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("archiving messages: counting copied messages: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM main.messages WHERE read = 1 AND sent_at <= ?
	`, cutoff); err != nil {
		return 0, fmt.Errorf("archiving messages: deleting moved messages from main: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("archiving messages: committing: %w", err)
	}

	return int(n), nil
}

// sqliteQuoteLiteral quotes s as a single-quoted SQLite string literal,
// doubling any embedded single quotes. Used only for ATTACH DATABASE's
// filename -- store.go's own Open already embeds its DSN's path via
// plain Go string formatting for the same reason (see design decision
// 8). archivePath is never attacker-controlled: it is derived once,
// internally, from cfg.SQLitePath (archiveDBPath), never from request
// input.
func sqliteQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
