package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"cdamp/internal/domain"
)

// archiveTestMessage builds a minimal, valid domain.Message for archive.go's
// tests -- every field ClaimPending/SaveMessage's own constraints require,
// with SentAt/Read set by the caller (the two fields archiveOnce's cutoff
// query actually cares about).
func archiveTestMessage(id, threadID string, agentID int64, sentAt time.Time, read bool) *domain.Message {
	return &domain.Message{
		ID:           id,
		ThreadID:     threadID,
		AgentID:      agentID,
		Direction:    "in",
		From:         "a@x",
		To:           "me@local",
		SenderDomain: "x",
		Subject:      "s-" + id,
		Body:         "b-" + id,
		Priority:     "normal",
		SentAt:       sentAt,
		Trust:        "verified",
		Status:       "received",
		Read:         read,
	}
}

// rowExists reports whether a row with the given id exists in db's messages
// table.
func rowExists(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var got string
	err := db.QueryRow("SELECT id FROM messages WHERE id = ?", id).Scan(&got)
	if err == nil {
		return true
	}
	if err == sql.ErrNoRows {
		return false
	}
	t.Fatalf("querying messages for %s: %v", id, err)
	return false
}

func TestArchiverArchiveOnceMovesReadOldMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	old := archiveTestMessage("m1", "t1", 1, now.Add(-48*time.Hour), true)
	if err := s.SaveMessage(ctx, old); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 24*time.Hour)
	a.now = func() time.Time { return now }

	moved, err := a.archiveOnce(ctx)
	if err != nil {
		t.Fatalf("archiveOnce: %v", err)
	}
	if moved != 1 {
		t.Errorf("moved = %d, want 1", moved)
	}
	if rowExists(t, s.db, "m1") {
		t.Errorf("message m1 still present in main.messages after archiving")
	}
	if !rowExists(t, s.archiveDB, "m1") {
		t.Errorf("message m1 not present in archive.messages after archiving")
	}
}

func TestArchiverArchiveOnceLeavesUnreadMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	unread := archiveTestMessage("m1", "t1", 1, now.Add(-48*time.Hour), false)
	if err := s.SaveMessage(ctx, unread); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 24*time.Hour)
	a.now = func() time.Time { return now }

	moved, err := a.archiveOnce(ctx)
	if err != nil {
		t.Fatalf("archiveOnce: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0 (unread messages must never move)", moved)
	}
	if !rowExists(t, s.db, "m1") {
		t.Errorf("unread message m1 was removed from main.messages")
	}
	if rowExists(t, s.archiveDB, "m1") {
		t.Errorf("unread message m1 leaked into archive.messages")
	}
}

func TestArchiverArchiveOnceLeavesRecentReadMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	recent := archiveTestMessage("m1", "t1", 1, now.Add(-1*time.Hour), true)
	if err := s.SaveMessage(ctx, recent); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 24*time.Hour)
	a.now = func() time.Time { return now }

	moved, err := a.archiveOnce(ctx)
	if err != nil {
		t.Fatalf("archiveOnce: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0 (recent read message must not move yet)", moved)
	}
	if !rowExists(t, s.db, "m1") {
		t.Errorf("recent read message m1 was removed from main.messages")
	}
	if rowExists(t, s.archiveDB, "m1") {
		t.Errorf("recent read message m1 leaked into archive.messages")
	}
}

func TestArchiverArchiveOnceMirrorsThreadRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	sentAt := now.Add(-48 * time.Hour)
	old := archiveTestMessage("m1", "t1", 1, sentAt, true)
	if err := s.SaveMessage(ctx, old); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 24*time.Hour)
	a.now = func() time.Time { return now }

	if _, err := a.archiveOnce(ctx); err != nil {
		t.Fatalf("archiveOnce: %v", err)
	}

	var subject string
	var createdAt int64
	err := s.archiveDB.QueryRow("SELECT subject, created_at FROM threads WHERE id = ?", "t1").
		Scan(&subject, &createdAt)
	if err != nil {
		t.Fatalf("querying archive.threads for t1: %v", err)
	}
	if subject != old.Subject {
		t.Errorf("archive thread subject = %q, want %q", subject, old.Subject)
	}
	if createdAt != sentAt.Unix() {
		t.Errorf("archive thread created_at = %d, want %d", createdAt, sentAt.Unix())
	}
}

func TestArchiverArchiveOnceIsIdempotentAcrossTicks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	old := archiveTestMessage("m1", "t1", 1, now.Add(-48*time.Hour), true)
	if err := s.SaveMessage(ctx, old); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 24*time.Hour)
	a.now = func() time.Time { return now }

	moved1, err := a.archiveOnce(ctx)
	if err != nil {
		t.Fatalf("archiveOnce (1): %v", err)
	}
	if moved1 != 1 {
		t.Fatalf("moved1 = %d, want 1", moved1)
	}

	moved2, err := a.archiveOnce(ctx)
	if err != nil {
		t.Fatalf("archiveOnce (2): %v", err)
	}
	if moved2 != 0 {
		t.Errorf("moved2 = %d, want 0 (second tick must be a no-op)", moved2)
	}

	var count int
	if err := s.archiveDB.QueryRow("SELECT COUNT(*) FROM messages WHERE id = ?", "m1").Scan(&count); err != nil {
		t.Fatalf("counting archive.messages rows for m1: %v", err)
	}
	if count != 1 {
		t.Errorf("archive.messages has %d rows for m1, want exactly 1 (must not duplicate)", count)
	}
}

func TestArchiverArchiveOnceZeroArchiveAfterIsNoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	old := archiveTestMessage("m1", "t1", 1, now.Add(-48*time.Hour), true)
	if err := s.SaveMessage(ctx, old); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 0)
	a.now = func() time.Time { return now }

	moved, err := a.archiveOnce(ctx)
	if err != nil {
		t.Fatalf("archiveOnce: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0 (archiveAfter=0 guard)", moved)
	}
	if !rowExists(t, s.db, "m1") {
		t.Errorf("message m1 was archived despite archiveAfter=0")
	}
}

func TestArchiverArchiveOnceMovedMessageStillFullTextSearchable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	old := archiveTestMessage("m1", "t1", 1, now.Add(-48*time.Hour), true)
	old.Subject = "quokka roadmap"
	if err := s.SaveMessage(ctx, old); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 24*time.Hour)
	a.now = func() time.Time { return now }
	if _, err := a.archiveOnce(ctx); err != nil {
		t.Fatalf("archiveOnce: %v", err)
	}

	var id string
	err := s.archiveDB.QueryRow(
		"SELECT m.id FROM messages m JOIN messages_fts ON messages_fts.rowid = m.rowid WHERE messages_fts MATCH ?",
		"quokka",
	).Scan(&id)
	if err != nil {
		t.Fatalf("archive.db FTS match for %q: %v", "quokka", err)
	}
	if id != "m1" {
		t.Errorf("archive FTS match returned id %q, want %q", id, "m1")
	}
}

func TestArchiverArchiveOnceRemovesFromMainFTSIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	now := mustTime(t, "2026-06-01T00:00:00Z")
	old := archiveTestMessage("m1", "t1", 1, now.Add(-48*time.Hour), true)
	old.Subject = "capybara summary"
	if err := s.SaveMessage(ctx, old); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	a := NewArchiver(s, 24*time.Hour)
	a.now = func() time.Time { return now }
	if _, err := a.archiveOnce(ctx); err != nil {
		t.Fatalf("archiveOnce: %v", err)
	}

	var id string
	err := s.db.QueryRow(
		"SELECT m.id FROM messages m JOIN messages_fts ON messages_fts.rowid = m.rowid WHERE messages_fts MATCH ?",
		"capybara",
	).Scan(&id)
	if err != sql.ErrNoRows {
		t.Errorf("main.db FTS match for %q: got id=%q, err=%v, want sql.ErrNoRows", "capybara", id, err)
	}
}

// TestArchiverRunStopsOnContextCancel mirrors delivery.Worker's own
// Run-cancellation test pattern: start Run in a goroutine against an
// already-canceled context, and assert it returns within a bounded time.
// Unlike delivery.Worker's own version of this test (which runs against an
// in-memory fake store whose ClaimPending ignores ctx cancellation entirely,
// so its immediate first pass can complete and be asserted on), Archiver's
// immediate first pass runs real SQL against ctx -- an already-canceled
// context makes that first archiveOnce call fail immediately with "context
// canceled" (logged, not fatal, per Run's own doc comment), so there is
// nothing further to assert about its outcome here; only that Run itself
// still returns promptly.
func TestArchiverRunStopsOnContextCancel(t *testing.T) {
	s := newTestStore(t)
	a := NewArchiver(s, 24*time.Hour)

	runCtx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Run is even started

	done := make(chan struct{})
	go func() {
		a.Run(runCtx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return within 200ms of an already-canceled context")
	}
}
