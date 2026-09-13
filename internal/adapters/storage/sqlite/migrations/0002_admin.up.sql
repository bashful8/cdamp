CREATE TABLE admin_credential (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  token_hash    TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);
