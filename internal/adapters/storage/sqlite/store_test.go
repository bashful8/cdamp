package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})
	return s
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing time %q: %v", s, err)
	}
	return tm.UTC()
}

func TestOpenRunsMigrationsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer s.Close()

	// Every table from 03-API.md's schema must exist after migration.
	wantTables := []string{"agents", "signing_keys", "threads", "messages", "domain_blocklist", "messages_fts"}
	for _, table := range wantTables {
		var name string
		err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE type IN ('table') AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after migration: %v", table, err)
		}
	}

	wantTriggers := []string{"messages_ai", "messages_ad", "messages_au"}
	for _, trig := range wantTriggers {
		var name string
		err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?", trig).Scan(&name)
		if err != nil {
			t.Errorf("trigger %q missing after migration: %v", trig, err)
		}
	}

	// Re-opening an already-migrated database must be a clean no-op, not
	// an error (idempotent startup migration).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-opening already-migrated database: %v", err)
	}
	defer s2.Close()
}

func TestOpenEnablesWALMode(t *testing.T) {
	s := newTestStore(t)

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestOpenEnablesForeignKeys(t *testing.T) {
	s := newTestStore(t)

	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	// Enforcement, not just the pragma value: inserting a message that
	// references a non-existent thread/agent must fail.
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO messages (
			id, thread_id, agent_id, direction, from_addr, to_addr, sender_domain,
			subject, body, priority, sent_at, trust, status
		) VALUES ('m1', 'no-such-thread', 999, 'out', 'a@x', 'b@y', 'x',
			'subj', 'body', 'normal', 0, 'verified', 'pending')
	`)
	if err == nil {
		t.Fatal("expected foreign key violation inserting a message with no matching thread/agent, got nil error")
	}
}

func TestSaveAndGetMessageRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedAgent(t, s, 1)

	sentAt := mustTime(t, "2026-01-01T12:00:00Z")
	expiresAt := mustTime(t, "2026-01-02T12:00:00Z")
	m := &domain.Message{
		ID:             "msg_1",
		ThreadID:       "thread_1",
		AgentID:        1,
		Direction:      "out",
		From:           "me@local.dev",
		To:             "you@remote.dev",
		SenderDomain:   "local.dev",
		Subject:        "hello",
		Body:           "hi there",
		Priority:       "normal",
		IdempotencyKey: "idem-1",
		SentAt:         sentAt,
		ExpiresAt:      &expiresAt,
		Trust:          "verified",
		Status:         "pending",
		Read:           false,
		Attempts:       0,
	}

	if err := s.SaveMessage(ctx, m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	got, err := s.GetMessage(ctx, "msg_1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}

	if got.ID != m.ID || got.ThreadID != m.ThreadID || got.AgentID != m.AgentID ||
		got.Direction != m.Direction || got.From != m.From || got.To != m.To ||
		got.SenderDomain != m.SenderDomain || got.Subject != m.Subject || got.Body != m.Body ||
		got.Priority != m.Priority || got.IdempotencyKey != m.IdempotencyKey ||
		got.Trust != m.Trust || got.Status != m.Status || got.Read != m.Read ||
		got.Attempts != m.Attempts {
		t.Errorf("round-tripped message mismatch:\n got  %+v\n want %+v", got, m)
	}
	if !got.SentAt.Equal(m.SentAt) {
		t.Errorf("SentAt = %v, want %v", got.SentAt, m.SentAt)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(*m.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, m.ExpiresAt)
	}
	if got.InReplyTo != "" {
		t.Errorf("InReplyTo = %q, want empty", got.InReplyTo)
	}
	if got.NextAttempt != nil {
		t.Errorf("NextAttempt = %v, want nil", got.NextAttempt)
	}
}

func TestSaveMessageCreatesThreadAndPreservesSubjectOnReply(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedAgent(t, s, 1)

	original := &domain.Message{
		ID:           "msg_1",
		ThreadID:     "thread_1",
		AgentID:      1,
		Direction:    "out",
		From:         "me@local.dev",
		To:           "you@remote.dev",
		SenderDomain: "local.dev",
		Subject:      "original subject",
		Body:         "hi",
		Priority:     "normal",
		SentAt:       mustTime(t, "2026-01-01T12:00:00Z"),
		Trust:        "verified",
		Status:       "pending",
	}
	if err := s.SaveMessage(ctx, original); err != nil {
		t.Fatalf("SaveMessage(original): %v", err)
	}

	reply := &domain.Message{
		ID:           "msg_2",
		ThreadID:     "thread_1",
		AgentID:      1,
		Direction:    "in",
		From:         "you@remote.dev",
		To:           "me@local.dev",
		SenderDomain: "remote.dev",
		Subject:      "Re: original subject (should not overwrite thread)",
		Body:         "reply body",
		Priority:     "normal",
		InReplyTo:    "msg_1",
		SentAt:       mustTime(t, "2026-01-01T13:00:00Z"),
		Trust:        "external",
		Status:       "received",
	}
	if err := s.SaveMessage(ctx, reply); err != nil {
		t.Fatalf("SaveMessage(reply): %v", err)
	}

	thread, messages, err := s.GetThread(ctx, "thread_1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Subject != "original subject" {
		t.Errorf("thread.Subject = %q, want %q (first message's subject must not be overwritten)", thread.Subject, "original subject")
	}
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(messages))
	}
	if messages[0].ID != "msg_1" || messages[1].ID != "msg_2" {
		t.Errorf("messages not ordered by sent_at: got [%s, %s]", messages[0].ID, messages[1].ID)
	}
	if messages[1].InReplyTo != "msg_1" {
		t.Errorf("reply.InReplyTo = %q, want %q", messages[1].InReplyTo, "msg_1")
	}
}

func TestGetMessageNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetMessage(context.Background(), "no-such-id")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetMessage(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestGetThreadNotFound(t *testing.T) {
	s := newTestStore(t)

	_, _, err := s.GetThread(context.Background(), "no-such-thread")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetThread(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestListMessagesFiltersAndPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedAgent(t, s, 1)
	seedAgent(t, s, 2)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	msgs := []*domain.Message{
		{ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x", Subject: "s1", Body: "b1", Priority: "normal", SentAt: base, Trust: "verified", Status: "received", Read: false},
		{ID: "m2", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x", Subject: "s1", Body: "b2", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received", Read: true},
		{ID: "m3", ThreadID: "t2", AgentID: 1, Direction: "out", From: "me@local", To: "b@y", SenderDomain: "local", Subject: "s2", Body: "b3", Priority: "normal", SentAt: base.Add(2 * time.Minute), Trust: "verified", Status: "pending", Read: false},
		{ID: "m4", ThreadID: "t3", AgentID: 2, Direction: "in", From: "c@z", To: "other@local", SenderDomain: "z", Subject: "s3", Body: "b4", Priority: "normal", SentAt: base.Add(3 * time.Minute), Trust: "verified", Status: "received", Read: false},
	}
	for _, m := range msgs {
		if err := s.SaveMessage(ctx, m); err != nil {
			t.Fatalf("SaveMessage(%s): %v", m.ID, err)
		}
	}

	t.Run("agent filter always applied", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 2})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m4" {
			t.Errorf("got %v, want only m4", ids(got))
		}
	})

	t.Run("status filter", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Status: "pending"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m3" {
			t.Errorf("got %v, want only m3", ids(got))
		}
	})

	t.Run("unread filter", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Unread: true})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %v, want m1 and m3 (unread)", ids(got))
		}
	})

	t.Run("limit", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Limit: 1})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m1" {
			t.Errorf("got %v, want only m1 (first by sent_at)", ids(got))
		}
	})

	t.Run("after cursor resumes past the given id", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, After: "m1"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 2 || got[0].ID != "m2" || got[1].ID != "m3" {
			t.Errorf("got %v, want [m2 m3]", ids(got))
		}
	})

	t.Run("query filtering not implemented", func(t *testing.T) {
		_, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Query: "hello"})
		if !errors.Is(err, ErrQueryNotSupported) {
			t.Errorf("ListMessages with Query set: err = %v, want ErrQueryNotSupported", err)
		}
	})
}

func ids(msgs []*domain.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

// seedAgent inserts a minimal agents row directly (no CreateAgent use case
// exists yet — that's Phase 6) so messages.agent_id's foreign key is
// satisfiable in these tests.
func seedAgent(t *testing.T, s *Store, id int64) {
	t.Helper()
	_, err := s.db.Exec(
		"INSERT INTO agents (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)",
		id, fmt.Sprintf("agent-%d", id), "hash", time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("seeding agent %d: %v", id, err)
	}
}
