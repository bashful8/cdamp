CREATE TABLE agents (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  token_hash    TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE signing_keys (
  kid                  TEXT PRIMARY KEY,
  public_key            BLOB NOT NULL,
  private_key_encrypted BLOB NOT NULL,   -- encrypted at rest under the
                                          -- passphrase from config/env;
                                          -- never stored/logged plaintext
  active                INTEGER NOT NULL DEFAULT 0,  -- 1 = current signing key
  created_at            INTEGER NOT NULL,
  retire_at             INTEGER          -- set at rotation; row is dropped
                                          -- from /.well-known responses and
                                          -- may be purged after this time
);

CREATE TABLE threads (
  id            TEXT PRIMARY KEY,
  subject       TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE messages (
  id             TEXT PRIMARY KEY,
  thread_id      TEXT NOT NULL REFERENCES threads(id),
  agent_id       INTEGER NOT NULL REFERENCES agents(id),
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
  status         TEXT NOT NULL,   -- pending|delivered|failed|received
  read           INTEGER NOT NULL DEFAULT 0,
  next_attempt   INTEGER,
  attempts       INTEGER NOT NULL DEFAULT 0,
  fail_reason    TEXT             -- e.g. "expired", "max_retries_exhausted"
);
CREATE UNIQUE INDEX idx_messages_idem  ON messages(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_messages_inbox   ON messages(agent_id, status, sent_at);
CREATE INDEX idx_messages_thread  ON messages(thread_id, sent_at);
CREATE INDEX idx_messages_domain  ON messages(sender_domain);
CREATE INDEX idx_messages_pending ON messages(status, next_attempt) WHERE direction='out';

CREATE TABLE domain_blocklist (
  domain     TEXT PRIMARY KEY,
  reason     TEXT,
  added_at   INTEGER NOT NULL
);

-- Full-text search (FTS5, built into modernc.org/sqlite)
CREATE VIRTUAL TABLE messages_fts USING fts5(
  subject, body, content='messages', content_rowid='rowid'
);
CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, subject, body) VALUES (new.rowid, new.subject, new.body);
END;
CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body) VALUES('delete', old.rowid, old.subject, old.body);
END;
CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body) VALUES('delete', old.rowid, old.subject, old.body);
  INSERT INTO messages_fts(rowid, subject, body) VALUES (new.rowid, new.subject, new.body);
END;
