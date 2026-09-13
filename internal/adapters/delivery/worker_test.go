package delivery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// retrySchedule mirrors 01-PROTOCOL.md's fixed backoff schedule exactly —
// tests never invent their own numbers here.
var retrySchedule = []time.Duration{
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	12 * time.Hour,
}

// recordingStore wraps *fakes.InboxStoreFake to additionally record the
// `reason` argument passed to each MarkFailed call, keyed by message ID.
// fakes.InboxStoreFake (Phase 2, unchanged by this task) never stores
// `reason` anywhere a test could read it back, and modifying that fake is
// outside this task's two-file scope (worker.go/worker_test.go only) — see
// STATUS.md's Task 2 spec, "Known pre-existing test fixture limitation".
// This wrapper is a test-local workaround, not a change to the fake
// itself: it delegates every other method untouched via embedding.
type recordingStore struct {
	*fakes.InboxStoreFake

	mu      sync.Mutex
	reasons map[string]string
}

func newRecordingStore() *recordingStore {
	return &recordingStore{
		InboxStoreFake: fakes.NewInboxStoreFake(),
		reasons:        map[string]string{},
	}
}

func (s *recordingStore) MarkFailed(ctx context.Context, id string, nextAttempt *time.Time, reason string) error {
	s.mu.Lock()
	s.reasons[id] = reason
	s.mu.Unlock()
	return s.InboxStoreFake.MarkFailed(ctx, id, nextAttempt, reason)
}

func (s *recordingStore) reasonFor(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reasons[id]
}

// seedMessage saves a due, pending outbound message ready for ClaimPending
// (NextAttempt left nil, so fakes.InboxStoreFake.ClaimPending's real-time
// due-check never excludes it regardless of the worker's own injected
// now).
func seedMessage(t *testing.T, store domain.InboxStore, id string, attempts int, expiresAt *time.Time) {
	t.Helper()
	m := &domain.Message{
		ID:        id,
		ThreadID:  id,
		AgentID:   1,
		Direction: "out",
		From:      "sender@example.dev",
		To:        "reviewer@other.dev",
		Subject:   "test subject",
		Body:      "test body",
		Priority:  "normal",
		SentAt:    time.Now(),
		ExpiresAt: expiresAt,
		Status:    "pending",
		Attempts:  attempts,
	}
	if err := store.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
}

const testInboxURL = "https://other.dev/deliver"

func TestRunOnce_HappyPath_MarksDelivered(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	dir := fakes.NewDirectoryFake()
	deliv := fakes.NewDeliveryFake() // defaults to success

	dir.Add("reviewer@other.dev", nil, "k1", testInboxURL)
	seedMessage(t, store, "m1", 0, nil)

	w := NewWorker(store, dir, deliv, retrySchedule)
	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: unexpected error: %v", err)
	}

	m, err := store.GetMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.Status != "delivered" {
		t.Errorf("status = %q, want %q", m.Status, "delivered")
	}
	if len(deliv.Sent()) != 1 {
		t.Errorf("len(Sent()) = %d, want 1", len(deliv.Sent()))
	}
}

func TestRunOnce_ResolveFailure_SchedulesFirstBackoff(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	dir := fakes.NewDirectoryFake() // no entry registered: Resolve fails
	deliv := fakes.NewDeliveryFake()

	seedMessage(t, store, "m1", 0, nil)

	fakeNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	w := NewWorker(store, dir, deliv, retrySchedule)
	w.now = func() time.Time { return fakeNow }

	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: unexpected error: %v", err)
	}

	m, err := store.GetMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	want := fakeNow.Add(time.Minute)
	if m.NextAttempt == nil || !m.NextAttempt.Equal(want) {
		t.Errorf("NextAttempt = %v, want %v", m.NextAttempt, want)
	}
	if len(deliv.Sent()) != 0 {
		t.Errorf("Deliver was called after a Resolve failure, want it skipped")
	}
}

func TestRunOnce_DeliverFailure_BackoffSchedule(t *testing.T) {
	for attempts := 0; attempts < len(retrySchedule); attempts++ {
		t.Run(retrySchedule[attempts].String(), func(t *testing.T) {
			store := fakes.NewInboxStoreFake()
			dir := fakes.NewDirectoryFake()
			deliv := fakes.NewDeliveryFake()

			dir.Add("reviewer@other.dev", nil, "k1", testInboxURL)
			deliv.SetFail(testInboxURL, errors.New("connection refused"))
			seedMessage(t, store, "m1", attempts, nil)

			fakeNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			w := NewWorker(store, dir, deliv, retrySchedule)
			w.now = func() time.Time { return fakeNow }

			if err := w.runOnce(context.Background()); err != nil {
				t.Fatalf("runOnce: unexpected error: %v", err)
			}

			m, err := store.GetMessage(context.Background(), "m1")
			if err != nil {
				t.Fatalf("GetMessage: %v", err)
			}
			want := fakeNow.Add(retrySchedule[attempts])
			if m.NextAttempt == nil || !m.NextAttempt.Equal(want) {
				t.Errorf("attempts=%d: NextAttempt = %v, want %v", attempts, m.NextAttempt, want)
			}
		})
	}
}

func TestRunOnce_DeliverFailure_ScheduleExhausted(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	dir := fakes.NewDirectoryFake()
	deliv := fakes.NewDeliveryFake()

	dir.Add("reviewer@other.dev", nil, "k1", testInboxURL)
	deliv.SetFail(testInboxURL, errors.New("connection refused"))
	seedMessage(t, store, "m1", len(retrySchedule), nil) // Attempts == 5, schedule len == 5

	w := NewWorker(store, dir, deliv, retrySchedule)

	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: unexpected error: %v", err)
	}

	m, err := store.GetMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.NextAttempt != nil {
		t.Errorf("NextAttempt = %v, want nil (terminal failure)", m.NextAttempt)
	}
	if m.Status != "failed" {
		t.Errorf("status = %q, want %q", m.Status, "failed")
	}
}

func TestRunOnce_Expired_NoHTTPCallsAtAll(t *testing.T) {
	store := newRecordingStore()
	dir := fakes.NewDirectoryFake() // deliberately no entry: Resolve must never be called
	deliv := fakes.NewDeliveryFake()

	fakeNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	expiresAt := fakeNow.Add(-time.Minute) // already expired
	seedMessage(t, store, "m1", 0, &expiresAt)

	w := NewWorker(store, dir, deliv, retrySchedule)
	w.now = func() time.Time { return fakeNow }

	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: unexpected error: %v", err)
	}

	m, err := store.GetMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.NextAttempt != nil {
		t.Errorf("NextAttempt = %v, want nil", m.NextAttempt)
	}
	if got := store.reasonFor("m1"); got != "expired" {
		t.Errorf("reason = %q, want %q", got, "expired")
	}
	if len(deliv.Sent()) != 0 {
		t.Errorf("Deliver was called for an already-expired message, want it skipped entirely")
	}
}

func TestRunOnce_NextAttemptWouldExceedExpiry_GivesUpEarly(t *testing.T) {
	store := newRecordingStore()
	dir := fakes.NewDirectoryFake()
	deliv := fakes.NewDeliveryFake()

	dir.Add("reviewer@other.dev", nil, "k1", testInboxURL)
	deliv.SetFail(testInboxURL, errors.New("connection refused"))

	fakeNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// Not yet expired, but schedule[0] (1m) would land after expiresAt
	// (30s from fakeNow) — the worker must give up now rather than wait
	// out the rest of the schedule, per 01-PROTOCOL.md's Expiry section.
	expiresAt := fakeNow.Add(30 * time.Second)
	seedMessage(t, store, "m1", 0, &expiresAt)

	w := NewWorker(store, dir, deliv, retrySchedule)
	w.now = func() time.Time { return fakeNow }

	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: unexpected error: %v", err)
	}

	m, err := store.GetMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.NextAttempt != nil {
		t.Errorf("NextAttempt = %v, want nil (terminal, expiry reached before next scheduled attempt)", m.NextAttempt)
	}
	// The reason must be the fixed "expired" string, not the Deliver
	// error's own text — distinguishes this case from plain schedule
	// exhaustion.
	if got := store.reasonFor("m1"); got != "expired" {
		t.Errorf("reason = %q, want %q", got, "expired")
	}
}

// TestRunOnce_ClaimPendingError is intentionally not written:
// fakes.InboxStoreFake.ClaimPending never returns a non-nil error, and no
// other task needs an error-returning variant of the fake, so fabricating
// one solely for this case would be new, unused test scaffolding per
// STATUS.md's Task 2 spec ("skip this case explicitly rather than
// fabricating an error-returning fake variant not otherwise needed by any
// other task").

func TestRun_ImmediateFirstPass_ThenReturnsOnCancel(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	dir := fakes.NewDirectoryFake()
	deliv := fakes.NewDeliveryFake() // defaults to success

	dir.Add("reviewer@other.dev", nil, "k1", testInboxURL)
	seedMessage(t, store, "m1", 0, nil)

	w := NewWorker(store, dir, deliv, retrySchedule)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Run is even started

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return within 200ms of an already-canceled context")
	}

	m, err := store.GetMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.Status != "delivered" {
		t.Errorf("status = %q, want %q (proves the immediate first pass ran before Run returned)", m.Status, "delivered")
	}
}
