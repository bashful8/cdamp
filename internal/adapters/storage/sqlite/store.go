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
	msqlite "modernc.org/sqlite"

	"cdamp/internal/domain"
)

// migrationsFS embeds the golang-migrate migration files so they ship
// inside the daemon binary and run automatically on startup — no external
// migrations directory needs to be deployed alongside it.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// messageColumnNames is the column list, in a fixed order, shared by every
// SELECT against messages so scanMessage's positional Scan stays in sync
// with the query text. messageColumns (comma-joined, for plain
// SELECT ... FROM messages queries) and qualifiedMessageColumns (each name
// prefixed with a table alias, for queries that JOIN messages against
// messages_fts — whose own subject/body columns would otherwise be
// ambiguous unqualified) both derive from this single slice so a future
// schema change only needs updating here.
var messageColumnNames = []string{
	"id", "thread_id", "agent_id", "direction", "from_addr", "to_addr", "sender_domain",
	"subject", "body", "priority", "in_reply_to", "idempotency_key", "sent_at",
	"expires_at", "trust", "status", "read", "next_attempt", "attempts", "fail_reason",
}

var messageColumns = strings.Join(messageColumnNames, ", ")

// qualifiedMessageColumns returns messageColumnNames each prefixed with
// "<alias>." — see messageColumnNames's doc comment.
func qualifiedMessageColumns(alias string) string {
	qualified := make([]string, len(messageColumnNames))
	for i, c := range messageColumnNames {
		qualified[i] = alias + "." + c
	}
	return strings.Join(qualified, ", ")
}

// sqliteConstraintUniqueCode is SQLite's extended result code for a
// UNIQUE-constraint violation (SQLITE_CONSTRAINT_UNIQUE in sqlite3.h;
// modernc.org/sqlite enables extended result codes on every connection by
// default). messages has exactly one UNIQUE index — idx_messages_idem, a
// partial unique index on idempotency_key; messages.id's own uniqueness is
// enforced as a PRIMARY KEY, which SQLite reports under the distinct
// SQLITE_CONSTRAINT_PRIMARYKEY code — so this code arriving from an
// INSERT INTO messages can only mean idx_messages_idem was violated.
const sqliteConstraintUniqueCode = 2067

// Store implements domain.InboxStore on top of a single SQLite file opened
// via modernc.org/sqlite (pure Go, no cgo).
type Store struct {
	db *sql.DB
}

// Compile-time assertion that *Store satisfies domain.InboxStore in full.
var _ domain.InboxStore = (*Store)(nil)

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
	// _txlock=immediate makes every transaction this Store opens via
	// (*sql.DB).BeginTx issue "BEGIN IMMEDIATE" instead of SQLite's default
	// "BEGIN DEFERRED" — modernc.org/sqlite reads this DSN parameter and
	// applies it to every non-read-only Begin on the connection (see
	// modernc.org/sqlite's tx.go: newTx picks "begin "+beginMode when set).
	// This is what ClaimPending's concurrency contract (cdamp-sqlite-fts
	// skill, "ClaimPending concurrency") requires: BEGIN IMMEDIATE takes
	// the write lock up front, so two concurrent ClaimPending
	// transactions serialize on it (the loser blocks for up to the
	// busy_timeout below, then proceeds against the post-commit state)
	// instead of both reading the same due rows before either commits.
	// Applying it to every transaction (not just ClaimPending's) rather
	// than a one-off "BEGIN IMMEDIATE" statement is also strictly safer
	// for SaveMessage's existing transaction: BEGIN DEFERRED's classic
	// footgun is a mid-transaction upgrade from a read lock to a write
	// lock failing with SQLITE_BUSY after other work has already run.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate", path)

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
		var sqliteErr *msqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUniqueCode {
			return domain.ErrDuplicateIdempotencyKey
		}
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
// f.Query, when set, narrows to messages matching it via the messages_fts
// FTS5 index (never a LIKE scan on subject/body — see the cdamp-sqlite-fts
// skill's "FTS5 sync triggers" section). This adds a JOIN against
// messages_fts and one more WHERE term to the same filter-building logic
// used for From/Thread/Status/Unread/After below; it doesn't change how
// any of those combine. An empty f.Query skips the join entirely and
// falls back to the plain indexed scan already used for every other
// filter combination.
func (s *Store) ListMessages(ctx context.Context, f domain.MessageFilter) ([]*domain.Message, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	from := "messages"
	cols := messageColumns
	where := []string{"agent_id = ?"}
	args := []any{f.AgentID}

	if f.Query != "" {
		// agent_id/from_addr/thread_id/status/read/sent_at/id all exist
		// only on messages, never on messages_fts (whose columns are just
		// subject, body, plus the implicit rowid/rank) — so referencing
		// them unqualified stays unambiguous even under this join, and
		// only the SELECT list (messageColumns, which does include
		// subject/body) needs table-qualifying.
		from = "messages m JOIN messages_fts ON messages_fts.rowid = m.rowid"
		cols = qualifiedMessageColumns("m")
		where = append(where, "messages_fts MATCH ?")
		args = append(args, f.Query)
	}

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

	query := "SELECT " + cols + " FROM " + from + " WHERE " + strings.Join(where, " AND ") +
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

// ClaimPending atomically selects up to limit outbound messages due for a
// delivery attempt (direction='out', status='pending', next_attempt NULL
// or already past), marks them 'claimed', and returns their full rows —
// all inside one transaction, per the cdamp-sqlite-fts skill's
// "ClaimPending concurrency" section and 02-ARCHITECTURE.md's Delivery
// worker flow. Because Open's DSN sets _txlock=immediate (see Open's doc
// comment), the BeginTx below issues "BEGIN IMMEDIATE" rather than
// SQLite's default "BEGIN DEFERRED": it takes the write lock up front, so
// a second concurrent ClaimPending call blocks (for up to the busy_timeout
// pragma) until this one commits, instead of both selecting the same due
// rows before either writes 'claimed'.
//
// 'claimed' is a transient in-process status with no CHECK constraint
// guarding messages.status (03-API.md schema), resolved by a later
// MarkDelivered or MarkFailed call. Results are ordered by sent_at, the
// same "oldest due first" order the skill's reference query uses.
func (s *Store) ClaimPending(ctx context.Context, limit int) ([]*domain.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claiming pending messages: beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	now := time.Now().Unix()
	idRows, err := tx.QueryContext(ctx, `
		SELECT id FROM messages
		WHERE direction = 'out' AND status = 'pending' AND (next_attempt IS NULL OR next_attempt <= ?)
		ORDER BY sent_at
		LIMIT ?
	`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claiming pending messages: selecting due ids: %w", err)
	}
	var ids []string
	for idRows.Next() {
		var id string
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			return nil, fmt.Errorf("claiming pending messages: scanning id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := idRows.Err(); err != nil {
		idRows.Close()
		return nil, fmt.Errorf("claiming pending messages: iterating ids: %w", err)
	}
	idRows.Close()

	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("claiming pending messages: committing empty claim: %w", err)
		}
		return nil, nil
	}

	placeholders, args := idInArgs(ids)
	if _, err := tx.ExecContext(ctx,
		"UPDATE messages SET status = 'claimed' WHERE id IN ("+placeholders+")", args...,
	); err != nil {
		return nil, fmt.Errorf("claiming pending messages: marking claimed: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE id IN ("+placeholders+") ORDER BY sent_at, id", args...)
	if err != nil {
		return nil, fmt.Errorf("claiming pending messages: reselecting claimed rows: %w", err)
	}
	var messages []*domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("claiming pending messages: scanning claimed message: %w", err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("claiming pending messages: iterating claimed messages: %w", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claiming pending messages: committing: %w", err)
	}

	return messages, nil
}

// idInArgs builds a "?,?,?"-style placeholder list and the corresponding
// []any argument slice for an `id IN (...)` clause over ids.
func idInArgs(ids []string) (placeholders string, args []any) {
	ph := make([]string, len(ids))
	args = make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return strings.Join(ph, ","), args
}

// MarkDelivered sets a message's status to 'delivered'. Returns
// domain.ErrNotFound if no message with the given id exists.
func (s *Store) MarkDelivered(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE messages SET status = 'delivered' WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("marking message %s delivered: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("marking message %s delivered: %w", id, err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// MarkFailed records a failed delivery attempt: fail_reason is set and
// attempts incremented unconditionally. What happens to status/next_attempt
// depends on whether a retry remains, per 02-ARCHITECTURE.md's Delivery
// worker flow read together with ClaimPending's own status='pending'
// filter — status going back to 'pending' is the *only* way a retryable
// failure is ever reclaimed by a later ClaimPending:
//
//   - nextAttempt != nil (more retries scheduled): status goes back to
//     'pending' with next_attempt set, so ClaimPending picks it up again
//     once due.
//   - nextAttempt == nil (backoff schedule exhausted): status becomes the
//     terminal 'failed', next_attempt cleared to NULL.
//
// Returns domain.ErrNotFound if no message with the given id exists.
func (s *Store) MarkFailed(ctx context.Context, id string, nextAttempt *time.Time, reason string) error {
	var (
		res sql.Result
		err error
	)
	if nextAttempt != nil {
		res, err = s.db.ExecContext(ctx, `
			UPDATE messages
			SET status = 'pending', next_attempt = ?, attempts = attempts + 1, fail_reason = ?
			WHERE id = ?
		`, nextAttempt.Unix(), reason, id)
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE messages
			SET status = 'failed', next_attempt = NULL, attempts = attempts + 1, fail_reason = ?
			WHERE id = ?
		`, reason, id)
	}
	if err != nil {
		return fmt.Errorf("marking message %s failed: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("marking message %s failed: %w", id, err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// FindByIdempotencyKey looks up a message by its idempotency key. An empty
// key means "no idempotency key was provided" (idx_messages_idem is a
// partial unique index over non-NULL values only — see toNullString's doc
// comment), never a lookup key, so it short-circuits to domain.ErrNotFound
// without touching the database. Otherwise returns domain.ErrNotFound on
// no match.
func (s *Store) FindByIdempotencyKey(ctx context.Context, key string) (*domain.Message, error) {
	if key == "" {
		return nil, domain.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, "SELECT "+messageColumns+" FROM messages WHERE idempotency_key = ?", key)
	m, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("finding message by idempotency key: %w", err)
	}
	return m, nil
}

// SearchThreads returns threads containing at least one message belonging
// to agentID, ordered by (created_at, id) and paginated via f.After/
// f.Limit with the same strictly-resume-past-cursor, default-50/cap-200
// discipline ListMessages uses for messages.
//
// When query is non-empty, results are narrowed to threads where at least
// one message matches it via the messages_fts index — "a thread matches q
// if any message in it matches" (03-API.md, GET /threads) — which is why
// this joins threads -> messages -> messages_fts rather than searching a
// thread's own subject: the matching message need not be the one that
// created the thread. DISTINCT collapses threads with more than one
// matching message down to one row. An empty query skips the FTS join
// entirely and returns a plain scan of every thread with at least one
// message for agentID, mirroring ListMessages' handling of an empty
// f.Query.
func (s *Store) SearchThreads(ctx context.Context, agentID int64, query string, f domain.ThreadFilter) ([]*domain.Thread, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	from := "threads t JOIN messages m ON m.thread_id = t.id"
	where := []string{"m.agent_id = ?"}
	args := []any{agentID}

	if query != "" {
		from += " JOIN messages_fts ON messages_fts.rowid = m.rowid"
		where = append(where, "messages_fts MATCH ?")
		args = append(args, query)
	}

	if f.After != "" {
		where = append(where, "(t.created_at, t.id) > (SELECT created_at, id FROM threads WHERE id = ?)")
		args = append(args, f.After)
	}

	q := "SELECT DISTINCT t.id, t.subject, t.created_at FROM " + from +
		" WHERE " + strings.Join(where, " AND ") +
		" ORDER BY t.created_at, t.id LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("searching threads: %w", err)
	}
	defer rows.Close()

	var threads []*domain.Thread
	for rows.Next() {
		var (
			id, subject string
			createdAt   int64
		)
		if err := rows.Scan(&id, &subject, &createdAt); err != nil {
			return nil, fmt.Errorf("searching threads: scanning thread: %w", err)
		}
		threads = append(threads, &domain.Thread{
			ID:        id,
			Subject:   subject,
			CreatedAt: time.Unix(createdAt, 0).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("searching threads: iterating threads: %w", err)
	}

	return threads, nil
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
