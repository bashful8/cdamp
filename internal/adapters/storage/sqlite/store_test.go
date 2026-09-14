package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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

}

func TestListMessagesQueryFTSMatching(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedAgent(t, s, 1)
	seedAgent(t, s, 2)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	msgs := []*domain.Message{
		{ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x", Subject: "quarterly report", Body: "nothing special here", Priority: "normal", SentAt: base, Trust: "verified", Status: "received"},
		{ID: "m2", ThreadID: "t2", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x", Subject: "unrelated", Body: "please review the banana proposal", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received"},
		{ID: "m3", ThreadID: "t3", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x", Subject: "totally different", Body: "no match at all", Priority: "normal", SentAt: base.Add(2 * time.Minute), Trust: "verified", Status: "received"},
		{ID: "m4", ThreadID: "t4", AgentID: 2, Direction: "in", From: "c@z", To: "other@local", SenderDomain: "z", Subject: "banana", Body: "also mentions banana", Priority: "normal", SentAt: base.Add(3 * time.Minute), Trust: "verified", Status: "received"},
	}
	for _, m := range msgs {
		if err := s.SaveMessage(ctx, m); err != nil {
			t.Fatalf("SaveMessage(%s): %v", m.ID, err)
		}
	}

	t.Run("matches on subject", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Query: "quarterly"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m1" {
			t.Errorf("got %v, want only m1 (subject match)", ids(got))
		}
	})

	t.Run("matches on body", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Query: "banana"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m2" {
			t.Errorf("got %v, want only m2 (body match, scoped to agent 1)", ids(got))
		}
	})

	t.Run("scoped to agent even though another agent's message matches too", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 2, Query: "banana"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m4" {
			t.Errorf("got %v, want only m4", ids(got))
		}
	})

	t.Run("no match", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Query: "nonexistentterm"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want none", ids(got))
		}
	})

	t.Run("combined with another filter", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Query: "banana", Thread: "t2"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m2" {
			t.Errorf("got %v, want only m2", ids(got))
		}

		got, err = s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Query: "banana", Thread: "t3"})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want none (banana doesn't match thread t3)", ids(got))
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

func threadIDs(threads []*domain.Thread) []string {
	out := make([]string, len(threads))
	for i, th := range threads {
		out[i] = th.ID
	}
	return out
}

// saveOutboundPending seeds a minimal pending outbound message directly
// through SaveMessage, for ClaimPending/MarkFailed tests that don't care
// about most fields.
func saveOutboundPending(t *testing.T, s *Store, id string, agentID int64, sentAt time.Time) {
	t.Helper()
	m := &domain.Message{
		ID:           id,
		ThreadID:     "thread-" + id,
		AgentID:      agentID,
		Direction:    "out",
		From:         "me@local.dev",
		To:           "you@remote.dev",
		SenderDomain: "local.dev",
		Subject:      "s",
		Body:         "b",
		Priority:     "normal",
		SentAt:       sentAt,
		Trust:        "verified",
		Status:       "pending",
	}
	if err := s.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage(%s): %v", id, err)
	}
}

func TestClaimPendingConcurrentClaimsDoNotOverlap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedAgent(t, s, 1)

	const (
		numMessages = 50
		numWorkers  = 10
		perWorker   = numMessages / numWorkers
	)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	for i := 0; i < numMessages; i++ {
		saveOutboundPending(t, s, fmt.Sprintf("out-%02d", i), 1, base.Add(time.Duration(i)*time.Second))
	}

	var (
		mu      sync.Mutex
		claimed = make(map[string]int) // id -> number of batches it appeared in
		wg      sync.WaitGroup
	)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch, err := s.ClaimPending(ctx, perWorker)
			if err != nil {
				t.Errorf("ClaimPending: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range batch {
				claimed[m.ID]++
			}
		}()
	}
	wg.Wait()

	if len(claimed) != numMessages {
		t.Fatalf("total distinct claimed = %d, want %d (claimed: %v)", len(claimed), numMessages, claimed)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("message %s claimed in %d batches, want exactly 1", id, n)
		}
	}

	// Every message should now be status='claimed' in the DB.
	for i := 0; i < numMessages; i++ {
		id := fmt.Sprintf("out-%02d", i)
		var status string
		if err := s.db.QueryRow("SELECT status FROM messages WHERE id = ?", id).Scan(&status); err != nil {
			t.Fatalf("querying status of %s: %v", id, err)
		}
		if status != "claimed" {
			t.Errorf("message %s status = %q, want %q", id, status, "claimed")
		}
	}

	// Nothing should be left to claim.
	leftover, err := s.ClaimPending(ctx, 100)
	if err != nil {
		t.Fatalf("ClaimPending (final): %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("leftover claim = %v, want none", ids(leftover))
	}
}

func TestClaimPendingIgnoresInboundAndNotYetDue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	saveOutboundPending(t, s, "due", 1, base)

	future := time.Now().Add(time.Hour)
	if _, err := s.db.Exec("UPDATE messages SET next_attempt = ? WHERE id = ?", future.Unix(), "due"); err != nil {
		t.Fatalf("setting next_attempt: %v", err)
	}

	inbound := &domain.Message{
		ID: "inbound", ThreadID: "thread-inbound", AgentID: 1, Direction: "in",
		From: "a@x", To: "me@local", SenderDomain: "x", Subject: "s", Body: "b",
		Priority: "normal", SentAt: base, Trust: "verified", Status: "pending",
	}
	if err := s.SaveMessage(ctx, inbound); err != nil {
		t.Fatalf("SaveMessage(inbound): %v", err)
	}

	got, err := s.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none (one message not due yet, the other inbound)", ids(got))
	}
}

func TestMarkDeliveredNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkDelivered(context.Background(), "no-such-id"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("MarkDelivered(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestMarkDeliveredSetsStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)
	saveOutboundPending(t, s, "m1", 1, mustTime(t, "2026-01-01T00:00:00Z"))

	if err := s.MarkDelivered(ctx, "m1"); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	got, err := s.GetMessage(ctx, "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Status != "delivered" {
		t.Errorf("Status = %q, want %q", got.Status, "delivered")
	}
}

func TestMarkFailedNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkFailed(context.Background(), "no-such-id", nil, "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("MarkFailed(missing) error = %v, want domain.ErrNotFound", err)
	}
}

// rawMessageState reads the columns MarkFailed touches directly, bypassing
// scanMessage/domain.Message (which has no FailReason field), so these
// tests can assert on fail_reason precisely.
func rawMessageState(t *testing.T, s *Store, id string) (status string, nextAttempt sql.NullInt64, attempts int, failReason sql.NullString) {
	t.Helper()
	if err := s.db.QueryRow(
		"SELECT status, next_attempt, attempts, fail_reason FROM messages WHERE id = ?", id,
	).Scan(&status, &nextAttempt, &attempts, &failReason); err != nil {
		t.Fatalf("querying raw state of %s: %v", id, err)
	}
	return status, nextAttempt, attempts, failReason
}

func TestMarkFailedRetryPathReturnsToPendingForReclaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)
	saveOutboundPending(t, s, "m1", 1, mustTime(t, "2026-01-01T00:00:00Z"))

	// A real delivery worker would have claimed it first; simulate that.
	if _, err := s.db.Exec("UPDATE messages SET status = 'claimed' WHERE id = ?", "m1"); err != nil {
		t.Fatalf("seeding claimed status: %v", err)
	}

	future := time.Now().Add(time.Hour)
	if err := s.MarkFailed(ctx, "m1", &future, "connection refused"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	status, nextAttempt, attempts, failReason := rawMessageState(t, s, "m1")
	if status != "pending" {
		t.Errorf("status = %q, want %q (retryable failure must return to pending so ClaimPending can reclaim it)", status, "pending")
	}
	if !nextAttempt.Valid || nextAttempt.Int64 != future.Unix() {
		t.Errorf("next_attempt = %v, want %d", nextAttempt, future.Unix())
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !failReason.Valid || failReason.String != "connection refused" {
		t.Errorf("fail_reason = %v, want %q", failReason, "connection refused")
	}

	// Not yet due: ClaimPending must not reclaim it.
	claimed, err := s.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("ClaimPending before due: got %v, want none", ids(claimed))
	}

	// Simulate time passing until next_attempt is due: it must now be
	// reclaimable — this is the whole point of MarkFailed's retry path.
	if _, err := s.db.Exec("UPDATE messages SET next_attempt = ? WHERE id = ?", time.Now().Add(-time.Minute).Unix(), "m1"); err != nil {
		t.Fatalf("simulating elapsed time: %v", err)
	}
	claimed, err = s.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "m1" {
		t.Fatalf("ClaimPending after due: got %v, want [m1]", ids(claimed))
	}
}

func TestMarkFailedTerminalPathStaysFailed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)
	saveOutboundPending(t, s, "m1", 1, mustTime(t, "2026-01-01T00:00:00Z"))

	if _, err := s.db.Exec("UPDATE messages SET status = 'claimed' WHERE id = ?", "m1"); err != nil {
		t.Fatalf("seeding claimed status: %v", err)
	}

	if err := s.MarkFailed(ctx, "m1", nil, "max_retries_exhausted"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	status, nextAttempt, attempts, failReason := rawMessageState(t, s, "m1")
	if status != "failed" {
		t.Errorf("status = %q, want %q (schedule exhausted must be terminal)", status, "failed")
	}
	if nextAttempt.Valid {
		t.Errorf("next_attempt = %v, want NULL", nextAttempt)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !failReason.Valid || failReason.String != "max_retries_exhausted" {
		t.Errorf("fail_reason = %v, want %q", failReason, "max_retries_exhausted")
	}

	// A terminally failed message must never be reclaimed.
	claimed, err := s.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	for _, cm := range claimed {
		if cm.ID == "m1" {
			t.Errorf("ClaimPending reclaimed terminally failed message %s", "m1")
		}
	}
}

func TestFindByIdempotencyKeyEmptyKeyIsNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.FindByIdempotencyKey(context.Background(), ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("FindByIdempotencyKey(\"\") error = %v, want domain.ErrNotFound", err)
	}
}

func TestFindByIdempotencyKeyMiss(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.FindByIdempotencyKey(context.Background(), "no-such-key"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("FindByIdempotencyKey(miss) error = %v, want domain.ErrNotFound", err)
	}
}

func TestSaveMessageDuplicateIdempotencyKeyRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	original := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "out",
		From: "me@local", To: "you@remote", SenderDomain: "local",
		Subject: "s", Body: "b", Priority: "normal",
		IdempotencyKey: "idem-shared",
		SentAt:         mustTime(t, "2026-01-01T00:00:00Z"),
		Trust:          "verified", Status: "pending",
	}
	if err := s.SaveMessage(ctx, original); err != nil {
		t.Fatalf("SaveMessage(original): %v", err)
	}

	dup := &domain.Message{
		ID: "m2", ThreadID: "t2", AgentID: 1, Direction: "out",
		From: "me@local", To: "someone-else@remote", SenderDomain: "local",
		Subject: "s2", Body: "b2", Priority: "normal",
		IdempotencyKey: "idem-shared",
		SentAt:         mustTime(t, "2026-01-01T00:01:00Z"),
		Trust:          "verified", Status: "pending",
	}
	if err := s.SaveMessage(ctx, dup); !errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		t.Fatalf("SaveMessage(dup) error = %v, want domain.ErrDuplicateIdempotencyKey", err)
	}

	got, err := s.FindByIdempotencyKey(ctx, "idem-shared")
	if err != nil {
		t.Fatalf("FindByIdempotencyKey: %v", err)
	}
	if got.ID != "m1" {
		t.Errorf("FindByIdempotencyKey returned %s, want original m1", got.ID)
	}

	// The rejected duplicate must not have been partially persisted.
	if _, err := s.GetMessage(ctx, "m2"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetMessage(m2) error = %v, want domain.ErrNotFound (rejected duplicate must not persist)", err)
	}
}

func TestSearchThreadsEmptyQueryReturnsAllForAgent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)
	seedAgent(t, s, 2)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	msgs := []*domain.Message{
		{ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x", Subject: "s1", Body: "b1", Priority: "normal", SentAt: base, Trust: "verified", Status: "received"},
		{ID: "m2", ThreadID: "t2", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x", Subject: "s2", Body: "b2", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received"},
		{ID: "m3", ThreadID: "t3", AgentID: 2, Direction: "in", From: "c@z", To: "other@local", SenderDomain: "z", Subject: "s3", Body: "b3", Priority: "normal", SentAt: base.Add(2 * time.Minute), Trust: "verified", Status: "received"},
	}
	for _, m := range msgs {
		if err := s.SaveMessage(ctx, m); err != nil {
			t.Fatalf("SaveMessage(%s): %v", m.ID, err)
		}
	}

	got, err := s.SearchThreads(ctx, 1, "", domain.ThreadFilter{})
	if err != nil {
		t.Fatalf("SearchThreads: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 threads (t1, t2)", threadIDs(got))
	}
	gotIDs := map[string]bool{}
	for _, th := range got {
		gotIDs[th.ID] = true
	}
	if !gotIDs["t1"] || !gotIDs["t2"] {
		t.Errorf("got threads %v, want t1 and t2", threadIDs(got))
	}
}

func TestSearchThreadsFTSMatching(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	original := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "project kickoff", Body: "let's get started", Priority: "normal",
		SentAt: base, Trust: "verified", Status: "received",
	}
	if err := s.SaveMessage(ctx, original); err != nil {
		t.Fatalf("SaveMessage(original): %v", err)
	}
	// A different thread that must never match the searches below.
	other := &domain.Message{
		ID: "m2", ThreadID: "t2", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "unrelated topic", Body: "nothing to do with it", Priority: "normal",
		SentAt: base.Add(time.Minute), Trust: "verified", Status: "received",
	}
	if err := s.SaveMessage(ctx, other); err != nil {
		t.Fatalf("SaveMessage(other): %v", err)
	}
	// A reply in thread t1 carrying the search term neither of thread
	// t1's own creating message shares — proving a thread matches via any
	// message in it, not just the one that created it.
	reply := &domain.Message{
		ID: "m3", ThreadID: "t1", AgentID: 1, Direction: "out", From: "me@local", To: "a@x", SenderDomain: "local",
		Subject: "Re: project kickoff", Body: "attaching the mango roadmap", Priority: "normal",
		InReplyTo: "m1", SentAt: base.Add(2 * time.Minute), Trust: "verified", Status: "pending",
	}
	if err := s.SaveMessage(ctx, reply); err != nil {
		t.Fatalf("SaveMessage(reply): %v", err)
	}

	t.Run("matches via a reply's body, not the thread's original message", func(t *testing.T) {
		got, err := s.SearchThreads(ctx, 1, "mango", domain.ThreadFilter{})
		if err != nil {
			t.Fatalf("SearchThreads: %v", err)
		}
		if len(got) != 1 || got[0].ID != "t1" {
			t.Fatalf("got %v, want only t1", threadIDs(got))
		}
	})

	t.Run("matches on subject", func(t *testing.T) {
		got, err := s.SearchThreads(ctx, 1, "kickoff", domain.ThreadFilter{})
		if err != nil {
			t.Fatalf("SearchThreads: %v", err)
		}
		if len(got) != 1 || got[0].ID != "t1" {
			t.Fatalf("got %v, want only t1", threadIDs(got))
		}
	})

	t.Run("no match", func(t *testing.T) {
		got, err := s.SearchThreads(ctx, 1, "nonexistentterm", domain.ThreadFilter{})
		if err != nil {
			t.Fatalf("SearchThreads: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want none", threadIDs(got))
		}
	})
}

func TestGetAgentByID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	got, err := s.GetAgentByID(ctx, 1)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	if got.ID != 1 || got.Name != "agent-1" {
		t.Fatalf("GetAgentByID = %+v, want ID=1 Name=agent-1", got)
	}
}

func TestGetAgentByIDNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetAgentByID(context.Background(), 999); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetAgentByID(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestFindAgentByTokenHash(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(
		"INSERT INTO agents (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)",
		1, "alice", "deadbeef", time.Now().Unix(),
	); err != nil {
		t.Fatalf("seeding agent: %v", err)
	}

	got, err := s.FindAgentByTokenHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("FindAgentByTokenHash: %v", err)
	}
	if got.ID != 1 || got.Name != "alice" {
		t.Fatalf("FindAgentByTokenHash = %+v, want ID=1 Name=alice", got)
	}
}

func TestFindAgentByTokenHashNotFound(t *testing.T) {
	s := newTestStore(t)
	seedAgent(t, s, 1)
	if _, err := s.FindAgentByTokenHash(context.Background(), "no-such-hash"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAgentByTokenHash(wrong) error = %v, want domain.ErrNotFound", err)
	}
}

func TestFindAgentByName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	got, err := s.FindAgentByName(ctx, "agent-1")
	if err != nil {
		t.Fatalf("FindAgentByName: %v", err)
	}
	if got.ID != 1 || got.Name != "agent-1" {
		t.Fatalf("FindAgentByName = %+v, want ID=1 Name=agent-1", got)
	}
}

func TestFindAgentByNameNotFound(t *testing.T) {
	s := newTestStore(t)
	seedAgent(t, s, 1)
	if _, err := s.FindAgentByName(context.Background(), "no-such-agent"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAgentByName(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestListAgents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)
	seedAgent(t, s, 2)

	got, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAgents returned %d agents, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].Name != "agent-1" {
		t.Errorf("ListAgents[0] = %+v, want ID=1 Name=agent-1", got[0])
	}
	if got[1].ID != 2 || got[1].Name != "agent-2" {
		t.Errorf("ListAgents[1] = %+v, want ID=2 Name=agent-2", got[1])
	}
}

func TestListAgentsEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListAgents = %v, want empty", got)
	}
}

func TestOpenCreatesArchiveSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer s.Close()

	wantTables := []string{"threads", "messages", "messages_fts"}
	for _, table := range wantTables {
		var name string
		err := s.archiveDB.QueryRow("SELECT name FROM sqlite_master WHERE type IN ('table') AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("archive table %q missing after Open: %v", table, err)
		}
	}

	wantTriggers := []string{"messages_ai", "messages_ad", "messages_au"}
	for _, trig := range wantTriggers {
		var name string
		err := s.archiveDB.QueryRow("SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?", trig).Scan(&name)
		if err != nil {
			t.Errorf("archive trigger %q missing after Open: %v", trig, err)
		}
	}

	// Re-opening an already-schema'd archive database must be a clean
	// no-op, not an error (idempotent ensureArchiveSchema).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-opening already-schema'd database: %v", err)
	}
	defer s2.Close()
}

func TestCloseClosesBothDatabases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := s.db.Ping(); err == nil {
		t.Errorf("s.db still accepts queries after Close")
	}
	if err := s.archiveDB.Ping(); err == nil {
		t.Errorf("s.archiveDB still accepts queries after Close")
	}
}

// seedFullyArchivedMessage simulates what Archiver's archiveOnce actually
// does to a message that ages past the archive threshold: first saves m
// through the normal SaveMessage path (so its thread row exists in
// main.threads, exactly as design decision 9 requires -- a thread's row is
// created only by SaveMessage and is never deleted, so any archived
// message's owning thread is always still resolvable from main), then
// removes m from main.messages and inserts the equivalent row directly into
// archive.db (bypassing Archiver itself, to isolate the read methods' own
// merge logic from the mover job, per the Phase 8 task 4 spec's testing
// checklist).
func seedFullyArchivedMessage(t *testing.T, s *Store, m *domain.Message) {
	t.Helper()
	ctx := context.Background()

	if err := s.SaveMessage(ctx, m); err != nil {
		t.Fatalf("SaveMessage(%s): %v", m.ID, err)
	}
	if _, err := s.db.Exec("DELETE FROM messages WHERE id = ?", m.ID); err != nil {
		t.Fatalf("removing %s from main.messages: %v", m.ID, err)
	}
	insertArchiveMessage(t, s, m)
}

// insertArchiveMessage inserts a message row directly into s.archiveDB,
// bypassing Archiver entirely -- used to isolate the read methods' own
// merge logic from the mover job, per the Phase 8 task 4 spec's testing
// checklist. threadID's own row is inserted into archive.threads too (the
// same structural-parity mirror Archiver performs), unless the caller has
// already seeded it (INSERT OR IGNORE keeps this idempotent across
// multiple calls for the same thread).
func insertArchiveMessage(t *testing.T, s *Store, m *domain.Message) {
	t.Helper()

	if _, err := s.archiveDB.Exec(
		"INSERT OR IGNORE INTO threads (id, subject, created_at) VALUES (?, ?, ?)",
		m.ThreadID, m.Subject, m.SentAt.Unix(),
	); err != nil {
		t.Fatalf("seeding archive thread %s: %v", m.ThreadID, err)
	}

	_, err := s.archiveDB.Exec(`
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
	)
	if err != nil {
		t.Fatalf("seeding archive message %s: %v", m.ID, err)
	}
}

func TestGetMessageChecksArchiveWhenNotInMain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	m := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in",
		From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "archived subject", Body: "archived body", Priority: "normal",
		SentAt: mustTime(t, "2026-01-01T00:00:00Z"), Trust: "verified", Status: "received", Read: true,
	}
	insertArchiveMessage(t, s, m)

	got, err := s.GetMessage(ctx, "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.ID != "m1" || got.Subject != "archived subject" {
		t.Errorf("GetMessage = %+v, want id=m1 subject=%q", got, "archived subject")
	}
}

func TestGetMessageNotFoundInEitherDatabase(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetMessage(context.Background(), "no-such-id")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetMessage(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestGetThreadMergesMainAndArchiveMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	main1 := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "thread subject", Body: "b1", Priority: "normal", SentAt: base, Trust: "verified", Status: "received",
	}
	if err := s.SaveMessage(ctx, main1); err != nil {
		t.Fatalf("SaveMessage(main1): %v", err)
	}
	main2 := &domain.Message{
		ID: "m3", ThreadID: "t1", AgentID: 1, Direction: "out", From: "me@local", To: "a@x", SenderDomain: "local",
		Subject: "Re: thread subject", Body: "b3", Priority: "normal", SentAt: base.Add(2 * time.Minute), Trust: "verified", Status: "pending",
	}
	if err := s.SaveMessage(ctx, main2); err != nil {
		t.Fatalf("SaveMessage(main2): %v", err)
	}

	archived := &domain.Message{
		ID: "m2", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "thread subject", Body: "b2", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received", Read: true,
	}
	insertArchiveMessage(t, s, archived)

	thread, messages, err := s.GetThread(ctx, "t1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Subject != "thread subject" {
		t.Errorf("thread.Subject = %q, want %q", thread.Subject, "thread subject")
	}
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(messages))
	}
	if messages[0].ID != "m1" || messages[1].ID != "m2" || messages[2].ID != "m3" {
		t.Errorf("messages not correctly ordered across databases: got %v, want [m1 m2 m3]", ids(messages))
	}
}

func TestListMessagesMergesMainAndArchiveResults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	mainMsg := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "s1", Body: "b1", Priority: "normal", SentAt: base, Trust: "verified", Status: "received",
	}
	if err := s.SaveMessage(ctx, mainMsg); err != nil {
		t.Fatalf("SaveMessage(mainMsg): %v", err)
	}
	archiveMsg := &domain.Message{
		ID: "m2", ThreadID: "t2", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "s2", Body: "b2", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received", Read: true,
	}
	insertArchiveMessage(t, s, archiveMsg)

	got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 2 || got[0].ID != "m1" || got[1].ID != "m2" {
		t.Errorf("got %v, want [m1 m2]", ids(got))
	}

	t.Run("capped to limit", func(t *testing.T) {
		got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Limit: 1})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(got) != 1 || got[0].ID != "m1" {
			t.Errorf("got %v, want only m1", ids(got))
		}
	})
}

func TestListMessagesAfterCursorSpansBothDatabases(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	// Cursor row lives in archive.db; the next page's row lives in main.
	cursorMsg := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "s1", Body: "b1", Priority: "normal", SentAt: base, Trust: "verified", Status: "received", Read: true,
	}
	insertArchiveMessage(t, s, cursorMsg)

	nextMsg := &domain.Message{
		ID: "m2", ThreadID: "t2", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "s2", Body: "b2", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received",
	}
	if err := s.SaveMessage(ctx, nextMsg); err != nil {
		t.Fatalf("SaveMessage(nextMsg): %v", err)
	}

	got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, After: "m1"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m2" {
		t.Errorf("got %v, want only m2 (page after archive-only cursor)", ids(got))
	}
}

func TestListMessagesQueryMatchesArchivedMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	archiveMsg := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "archived only", Body: "distinctive gizmo content", Priority: "normal",
		SentAt: mustTime(t, "2026-01-01T00:00:00Z"), Trust: "verified", Status: "received", Read: true,
	}
	insertArchiveMessage(t, s, archiveMsg)

	got, err := s.ListMessages(ctx, domain.MessageFilter{AgentID: 1, Query: "gizmo"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Errorf("got %v, want only m1 (archive-only FTS match)", ids(got))
	}
}

func TestSearchThreadsFindsArchiveOnlyMatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	archiveMsg := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "archive-only thread", Body: "narwhal migration notes", Priority: "normal",
		SentAt: mustTime(t, "2026-01-01T00:00:00Z"), Trust: "verified", Status: "received", Read: true,
	}
	seedFullyArchivedMessage(t, s, archiveMsg)

	got, err := s.SearchThreads(ctx, 1, "narwhal", domain.ThreadFilter{})
	if err != nil {
		t.Fatalf("SearchThreads: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("got %v, want only t1", threadIDs(got))
	}
	if got[0].Subject != "archive-only thread" {
		t.Errorf("got[0].Subject = %q, want %q (thread metadata must come from main)", got[0].Subject, "archive-only thread")
	}
}

func TestSearchThreadsThreadWithMessagesInBothDatabasesAppearsOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	base := mustTime(t, "2026-01-01T00:00:00Z")
	mainMsg := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "shared thread", Body: "platypus report", Priority: "normal", SentAt: base, Trust: "verified", Status: "received",
	}
	if err := s.SaveMessage(ctx, mainMsg); err != nil {
		t.Fatalf("SaveMessage(mainMsg): %v", err)
	}
	archiveMsg := &domain.Message{
		ID: "m2", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "shared thread", Body: "platypus follow-up", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received", Read: true,
	}
	insertArchiveMessage(t, s, archiveMsg)

	got, err := s.SearchThreads(ctx, 1, "platypus", domain.ThreadFilter{})
	if err != nil {
		t.Fatalf("SearchThreads: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("got %v, want exactly one t1 (must be deduplicated)", threadIDs(got))
	}
}

func TestSearchThreadsAfterCursorAppliesToArchiveOnlyMatches(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, 1)

	base := mustTime(t, "2026-01-01T00:00:00Z")

	// A main thread that will serve as the after-cursor -- deliberately
	// does not contain "wombat" itself, so it never appears in either
	// side's query results and only serves as the cursor's reference row.
	cursorThread := &domain.Message{
		ID: "m1", ThreadID: "t1", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "cursor thread", Body: "unrelated content", Priority: "normal", SentAt: base, Trust: "verified", Status: "received",
	}
	if err := s.SaveMessage(ctx, cursorThread); err != nil {
		t.Fatalf("SaveMessage(cursorThread): %v", err)
	}

	// An archive-only thread created before the cursor -- must be excluded.
	before := &domain.Message{
		ID: "m2", ThreadID: "t0", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "before cursor", Body: "wombat early notes", Priority: "normal", SentAt: base.Add(-time.Minute), Trust: "verified", Status: "received", Read: true,
	}
	seedFullyArchivedMessage(t, s, before)

	// An archive-only thread created after the cursor -- must be included.
	after := &domain.Message{
		ID: "m3", ThreadID: "t2", AgentID: 1, Direction: "in", From: "a@x", To: "me@local", SenderDomain: "x",
		Subject: "after cursor", Body: "wombat later notes", Priority: "normal", SentAt: base.Add(time.Minute), Trust: "verified", Status: "received", Read: true,
	}
	seedFullyArchivedMessage(t, s, after)

	got, err := s.SearchThreads(ctx, 1, "wombat", domain.ThreadFilter{After: "t1"})
	if err != nil {
		t.Fatalf("SearchThreads: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t2" {
		t.Errorf("got %v, want only t2 (t0 excluded by after cursor)", threadIDs(got))
	}
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
