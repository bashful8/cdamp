package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	migratelib "github.com/golang-migrate/migrate/v4"
	migsqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite"

	"cdamp/internal/domain"
)

// migrationsFS embeds the golang-migrate migration files so they ship
// inside the daemon binary and run automatically on startup — no external
// migrations directory needs to be deployed alongside it.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrQueryNotSupported is returned by ListMessages when f.Query is set.
// FTS5-backed search (MessageFilter.Query and SearchThreads) is Phase 2
// task 2's job, not this one's — callers must not silently get an
// unfiltered result set when they asked for a text match.
var ErrQueryNotSupported = errors.New("sqlite: query (FTS) filtering is not implemented yet")

// messageColumns is the column list, in a fixed order, shared by every
// SELECT against messages so scanMessage's positional Scan stays in sync
// with the query text.
const messageColumns = `id, thread_id, agent_id, direction, from_addr, to_addr, sender_domain,
	subject, body, priority, in_reply_to, idempotency_key, sent_at,
	expires_at, trust, status, read, next_attempt, attempts, fail_reason`

// Store implements the non-FTS, non-retry-queue half of domain.InboxStore
// (SaveMessage, GetMessage, GetThread, ListMessages) on top of a single
// SQLite file opened via modernc.org/sqlite (pure Go, no cgo).
//
// Store intentionally does not implement domain.InboxStore in full yet:
// ClaimPending, MarkDelivered, MarkFailed, FindByIdempotencyKey, and
// SearchThreads (plus MessageFilter.Query support in ListMessages) are
// Phase 2 task 2's job. Adding stub methods that panic would let *Store
// satisfy the interface today only to panic at runtime tomorrow, which is
// worse than just not claiming the interface yet — so *Store is never
// assigned to a domain.InboxStore-typed variable anywhere in this package.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database file at path,
// enables WAL mode + a busy timeout + foreign-key enforcement via DSN
// pragmas, runs every pending migration embedded under migrations/, and
// returns a ready-to-use Store.
//
// Migration library note: golang-migrate ships two SQLite database
// drivers. "database/sqlite3" (import path
// .../migrate/v4/database/sqlite3) is the one most examples online use,
// and it is built against the cgo mattn/go-sqlite3 driver — not usable
// here, since the project's driver is the pure-Go modernc.org/sqlite.
// golang-migrate also ships a second, separate driver, confusingly named
// "database/sqlite" (no "3"), which is built specifically for
// modernc.org/sqlite and exposes WithInstance so it can be bound directly
// to the *sql.DB opened below instead of being given a DSN/driver name of
// its own. That's the one used here (aliased migsqlite below) — no cgo
// anywhere in the path, migrations still run through golang-migrate as
// 04-BUILD-PLAN.md requires.
//
// Filename note: 03-API.md/the cdamp-sqlite-fts skill describe the
// migration file as "0001_init.sql". golang-migrate's source drivers
// (file and iofs alike) parse filenames with a fixed
// "{version}_{title}.{up|down}.{ext}" pattern and simply won't discover a
// file without an up/down direction in its name, so the file on disk here
// is migrations/0001_init.up.sql (plus a 0001_init.down.sql for
// completeness) rather than a literal 0001_init.sql. The content is still
// the complete schema from 03-API.md, copied verbatim, in one migration,
// exactly as specified — only the filename suffix was adjusted to make
// the required library able to run it.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database %s: %w", path, err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to sqlite database %s: %w", path, err)
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating sqlite database %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// runMigrations applies every embedded migration under migrations/ that
// hasn't already been applied to db, using golang-migrate bound directly
// to the already-open connection (see Open's doc comment for why).
//
// Deliberately does not call migrate.Migrate.Close(): that closes both the
// source driver and the database driver, and the database driver here
// wraps the same *sql.DB the caller keeps using afterward via Store — so
// closing it here would sever the connection Open is about to return.
func runMigrations(db *sql.DB) error {
	sourceDriver, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("loading embedded migrations: %w", err)
	}

	dbDriver, err := migsqlite.WithInstance(db, &migsqlite.Config{})
	if err != nil {
		return fmt.Errorf("binding migration driver: %w", err)
	}

	m, err := migratelib.NewWithInstance("iofs", sourceDriver, "sqlite", dbDriver)
	if err != nil {
		return fmt.Errorf("initializing migrator: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migratelib.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// SaveMessage stores m, first upserting m's thread row.
//
// ports.go has no separate CreateThread method and messages.thread_id
// references threads(id) with foreign_keys=ON, so SaveMessage is the only
// place a thread row can come into existence. ON CONFLICT(id) DO NOTHING
// means only the first message of a thread sets its subject — a later
// reply's own (possibly "Re: ..."-prefixed) Subject never overwrites the
// thread's original subject.
func (s *Store) SaveMessage(ctx context.Context, m *domain.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("saving message %s: beginning transaction: %w", m.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO threads (id, subject, created_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, m.ThreadID, m.Subject, m.SentAt.Unix()); err != nil {
		return fmt.Errorf("saving message %s: upserting thread %s: %w", m.ID, m.ThreadID, err)
	}

	// fail_reason is always NULL here: Message has no FailReason field —
	// it's only ever set later by MarkFailed (Phase 2 task 2).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			id, thread_id, agent_id, direction, from_addr, to_addr, sender_domain,
			subject, body, priority, in_reply_to, idempotency_key, sent_at,
			expires_at, trust, status, read, next_attempt, attempts, fail_reason
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
	`,
		m.ID, m.ThreadID, m.AgentID, m.Direction, m.From, m.To, m.SenderDomain,
		m.Subject, m.Body, m.Priority, toNullString(m.InReplyTo), toNullString(m.IdempotencyKey),
		m.SentAt.Unix(), toNullUnix(m.ExpiresAt), m.Trust, m.Status, boolToInt(m.Read),
		toNullUnix(m.NextAttempt), m.Attempts,
	); err != nil {
		return fmt.Errorf("saving message %s: inserting message: %w", m.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("saving message %s: committing transaction: %w", m.ID, err)
	}
	return nil
}

// GetMessage returns the message with the given id, or domain.ErrNotFound
// if no such message exists.
func (s *Store) GetMessage(ctx context.Context, id string) (*domain.Message, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+messageColumns+" FROM messages WHERE id = ?", id)
	m, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("getting message %s: %w", id, err)
	}
	return m, nil
}

// GetThread returns the thread with the given id and every message in it,
// ordered by sent_at (then id, to break ties deterministically), per
// 03-API.md's `GET /threads/{id}` spec. Returns domain.ErrNotFound if no
// such thread exists.
func (s *Store) GetThread(ctx context.Context, id string) (*domain.Thread, []*domain.Message, error) {
	var (
		threadID  string
		subject   string
		createdAt int64
	)
	err := s.db.QueryRowContext(ctx, "SELECT id, subject, created_at FROM threads WHERE id = ?", id).
		Scan(&threadID, &subject, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, domain.ErrNotFound
		}
		return nil, nil, fmt.Errorf("getting thread %s: %w", id, err)
	}
	thread := &domain.Thread{
		ID:        threadID,
		Subject:   subject,
		CreatedAt: time.Unix(createdAt, 0).UTC(),
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE thread_id = ? ORDER BY sent_at, id", id)
	if err != nil {
		return nil, nil, fmt.Errorf("getting thread %s: listing messages: %w", id, err)
	}
	defer rows.Close()

	var messages []*domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("getting thread %s: scanning message: %w", id, err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("getting thread %s: iterating messages: %w", id, err)
	}

	return thread, messages, nil
}

// ListMessages returns messages matching f, always scoped to f.AgentID and
// further narrowed by whichever of f.From/f.Thread/f.Status/f.Unread/
// f.After is set. f.Limit defaults to 50 and is capped at 200, per
// 03-API.md.
//
// Results are ordered by (sent_at, id) — sent_at is the natural order
// 03-API.md's endpoints imply, and id is appended only to make that order
// total (sent_at is not guaranteed unique), which is what makes f.After
// (an opaque cursor = the last message id from the previous page)
// well-defined: pages resume strictly after that row's (sent_at, id)
// position.
//
// f.Query (FTS5 matching) is not implemented here — that lands in Phase 2
// task 2 alongside SearchThreads. Passing a non-empty f.Query returns
// ErrQueryNotSupported rather than silently ignoring it.
func (s *Store) ListMessages(ctx context.Context, f domain.MessageFilter) ([]*domain.Message, error) {
	if f.Query != "" {
		return nil, ErrQueryNotSupported
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	where := []string{"agent_id = ?"}
	args := []any{f.AgentID}

	if f.From != "" {
		where = append(where, "from_addr = ?")
		args = append(args, f.From)
	}
	if f.Thread != "" {
		where = append(where, "thread_id = ?")
		args = append(args, f.Thread)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Unread {
		where = append(where, "read = 0")
	}
	if f.After != "" {
		where = append(where, "(sent_at, id) > (SELECT sent_at, id FROM messages WHERE id = ?)")
		args = append(args, f.After)
	}

	query := "SELECT " + messageColumns + " FROM messages WHERE " + strings.Join(where, " AND ") +
		" ORDER BY sent_at, id LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	defer rows.Close()

	var messages []*domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("listing messages: scanning message: %w", err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing messages: iterating messages: %w", err)
	}

	return messages, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows, letting scanMessage
// serve GetMessage (single row) and GetThread/ListMessages (multi-row)
// alike.
type scanner interface {
	Scan(dest ...any) error
}

// scanMessage scans one row in messageColumns order into a domain.Message.
func scanMessage(row scanner) (*domain.Message, error) {
	var (
		m                         domain.Message
		agentID                   int64
		sentAt                    int64
		expiresAt, nextAttempt    sql.NullInt64
		inReplyTo, idempotencyKey sql.NullString
		readInt, attempts         int64
		failReason                sql.NullString // unused: Message has no FailReason field
	)

	if err := row.Scan(
		&m.ID, &m.ThreadID, &agentID, &m.Direction, &m.From, &m.To, &m.SenderDomain,
		&m.Subject, &m.Body, &m.Priority, &inReplyTo, &idempotencyKey, &sentAt,
		&expiresAt, &m.Trust, &m.Status, &readInt, &nextAttempt, &attempts, &failReason,
	); err != nil {
		return nil, err
	}

	m.AgentID = agentID
	m.InReplyTo = fromNullString(inReplyTo)
	m.IdempotencyKey = fromNullString(idempotencyKey)
	m.SentAt = time.Unix(sentAt, 0).UTC()
	m.ExpiresAt = fromNullUnix(expiresAt)
	m.Read = readInt != 0
	m.NextAttempt = fromNullUnix(nextAttempt)
	m.Attempts = int(attempts)

	return &m, nil
}

// toNullString maps "" (domain's zero value for "no value") to SQL NULL.
// This matters beyond style for idempotency_key specifically:
// idx_messages_idem is a unique index over non-NULL values, so storing ""
// instead of NULL for "no idempotency key" would make every second
// key-less message collide on that index.
func toNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func fromNullString(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}

func toNullUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func fromNullUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
