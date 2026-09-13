package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	msqlite "modernc.org/sqlite"

	"cdamp/internal/domain"
)

// sqliteConstraintPrimaryKeyCode is SQLite's extended result code for a
// PRIMARY KEY constraint violation (SQLITE_CONSTRAINT_PRIMARYKEY in
// sqlite3.h). Distinct from sqliteConstraintUniqueCode (store.go, 2067,
// used for agents.name's plain UNIQUE index): domain_blocklist.domain
// is declared TEXT PRIMARY KEY, and for a non-INTEGER (non-rowid-alias)
// PRIMARY KEY column, a duplicate INSERT reports this distinct code,
// not the plain UNIQUE code — confirmed empirically against this exact
// modernc.org/sqlite version during planning (STATUS.md, Phase 6 task 4
// spec): a duplicate INSERT against a `TEXT PRIMARY KEY` column returned
// code 1555, not 2067.
const sqliteConstraintPrimaryKeyCode = 1555

// SaveBlocklistEntry persists e as a new domain_blocklist row. Returns a
// wrapped domain.ErrConflict if e.Domain is already blocklisted.
func (s *Store) SaveBlocklistEntry(ctx context.Context, e *domain.BlocklistEntry) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO domain_blocklist (domain, reason, added_at) VALUES (?, ?, ?)",
		e.Domain, toNullString(e.Reason), e.AddedAt.Unix(),
	)
	if err != nil {
		var sqliteErr *msqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintPrimaryKeyCode {
			return fmt.Errorf("saving blocklist entry %q: %w", e.Domain, domain.ErrConflict)
		}
		return fmt.Errorf("saving blocklist entry %q: %w", e.Domain, err)
	}
	return nil
}

// ListBlocklist returns every blocklisted domain, ordered by domain
// ascending.
func (s *Store) ListBlocklist(ctx context.Context) ([]*domain.BlocklistEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT domain, reason, added_at FROM domain_blocklist ORDER BY domain")
	if err != nil {
		return nil, fmt.Errorf("listing blocklist: %w", err)
	}
	defer rows.Close()

	var entries []*domain.BlocklistEntry
	for rows.Next() {
		var (
			d       string
			reason  sql.NullString
			addedAt int64
		)
		if err := rows.Scan(&d, &reason, &addedAt); err != nil {
			return nil, fmt.Errorf("listing blocklist: %w", err)
		}
		entries = append(entries, &domain.BlocklistEntry{
			Domain:  d,
			Reason:  fromNullString(reason),
			AddedAt: time.Unix(addedAt, 0).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing blocklist: %w", err)
	}
	return entries, nil
}
