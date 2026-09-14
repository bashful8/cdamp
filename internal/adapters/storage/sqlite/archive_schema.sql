-- archive.db's schema. NOT managed by golang-migrate -- see STATUS.md's
-- Phase 8 task 4 spec, design decision 3: this file is executed directly
-- (ensureArchiveSchema, archive.go) against a plain, dedicated *sql.DB
-- opened on the archive file, once, from Store.Open, using IF NOT EXISTS
-- everywhere so re-running it on every daemon startup is a safe no-op.
-- Deliberately not under migrations/ -- that directory is golang-migrate's
-- own versioned-migration source and this file is neither versioned nor
-- discovered by it.
--
-- Deviates from 03-API.md's live schema in four ways -- see the Phase 8
-- task 4 spec's design decision 4 for the full reasoning behind each:
--   - agents, signing_keys, domain_blocklist are omitted entirely.
--   - messages.thread_id has no REFERENCES threads(id) -- SQLite cannot
--     enforce a foreign key across two separate database files.
--   - idx_messages_pending is omitted -- archive.db never participates in
--     ClaimPending.
--   - threads is kept (structural parity / an archival-job mirror
--     target), even though no read path relies on archive.db's own
--     threads table -- thread metadata is always read from main.
-- messages_fts + its three sync triggers ARE included, structurally
-- identical to 03-API.md's, so archived messages remain full-text
-- searchable via GET /messages?q=/GET /threads?q= -- see design
-- decision 5.

CREATE TABLE IF NOT EXISTS threads (
  id            TEXT PRIMARY KEY,
  subject       TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id             TEXT PRIMARY KEY,
  thread_id      TEXT NOT NULL,
  agent_id       INTEGER NOT NULL,
  direction      TEXT NOT NULL CHECK(direction IN ('in','out')),
  from_addr      TEXT NOT NULL,
  to_addr        TEXT NOT NULL,
  sender_domain  TEXT NOT NULL,
  subject        TEXT NOT NULL,
  body           TEXT NOT NULL,
  priority       TEXT NOT NULL DEFAULT 'normal',
  in_reply_to    TEXT,
  idempotency_key TEXT,
  sent_at        INTEGER NOT NULL,
  expires_at     INTEGER,
  trust          TEXT NOT NULL CHECK(trust IN ('verified','external','untrusted')),
  status         TEXT NOT NULL,
  read           INTEGER NOT NULL DEFAULT 0,
  next_attempt   INTEGER,
  attempts       INTEGER NOT NULL DEFAULT 0,
  fail_reason    TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_idem  ON messages(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_inbox   ON messages(agent_id, status, sent_at);
CREATE INDEX IF NOT EXISTS idx_messages_thread  ON messages(thread_id, sent_at);
CREATE INDEX IF NOT EXISTS idx_messages_domain  ON messages(sender_domain);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  subject, body, content='messages', content_rowid='rowid'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, subject, body) VALUES (new.rowid, new.subject, new.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body) VALUES('delete', old.rowid, old.subject, old.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body) VALUES('delete', old.rowid, old.subject, old.body);
  INSERT INTO messages_fts(rowid, subject, body) VALUES (new.rowid, new.subject, new.body);
END;
