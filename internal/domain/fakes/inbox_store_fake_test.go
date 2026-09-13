package fakes

import (
	"context"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func TestInboxStoreFakeSaveAndGetMessage(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()

	m := &domain.Message{ID: "m1", ThreadID: "t1", Subject: "hello"}
	if err := f.SaveMessage(ctx, m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	got, err := f.GetMessage(ctx, "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.ID != "m1" || got.Subject != "hello" {
		t.Fatalf("GetMessage returned %+v, want ID=m1 Subject=hello", got)
	}
}

func TestInboxStoreFakeGetMessageNotFound(t *testing.T) {
	f := NewInboxStoreFake()
	_, err := f.GetMessage(context.Background(), "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetMessage(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestInboxStoreFakeGetThreadNotFound(t *testing.T) {
	f := NewInboxStoreFake()
	_, _, err := f.GetThread(context.Background(), "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetThread(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestInboxStoreFakeGetThreadReturnsMessages(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	if err := f.SaveMessage(ctx, &domain.Message{ID: "m1", ThreadID: "t1", Subject: "s"}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := f.SaveMessage(ctx, &domain.Message{ID: "m2", ThreadID: "t1", Subject: "s"}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	thread, msgs, err := f.GetThread(ctx, "t1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.ID != "t1" {
		t.Fatalf("GetThread thread.ID = %q, want t1", thread.ID)
	}
	if len(msgs) != 2 {
		t.Fatalf("GetThread returned %d messages, want 2", len(msgs))
	}
}

func TestInboxStoreFakeFindByIdempotencyKey(t *testing.T) {
	tests := []struct {
		name    string
		seed    []*domain.Message
		key     string
		wantID  string
		wantErr bool
	}{
		{
			name:   "found",
			seed:   []*domain.Message{{ID: "m1", IdempotencyKey: "key-1"}},
			key:    "key-1",
			wantID: "m1",
		},
		{
			name:    "not found",
			seed:    []*domain.Message{{ID: "m1", IdempotencyKey: "key-1"}},
			key:     "key-2",
			wantErr: true,
		},
		{
			name:    "empty key never matches",
			seed:    []*domain.Message{{ID: "m1", IdempotencyKey: ""}},
			key:     "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewInboxStoreFake()
			ctx := context.Background()
			for _, m := range tc.seed {
				if err := f.SaveMessage(ctx, m); err != nil {
					t.Fatalf("SaveMessage: %v", err)
				}
			}
			got, err := f.FindByIdempotencyKey(ctx, tc.key)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("FindByIdempotencyKey error = %v, want domain.ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindByIdempotencyKey: %v", err)
			}
			if got.ID != tc.wantID {
				t.Fatalf("FindByIdempotencyKey ID = %q, want %q", got.ID, tc.wantID)
			}
		})
	}
}

func TestInboxStoreFakeClaimPendingDoesNotDoubleClaim(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)

	for _, id := range []string{"m1", "m2", "m3"} {
		if err := f.SaveMessage(ctx, &domain.Message{
			ID: id, Status: "pending", NextAttempt: &past,
		}); err != nil {
			t.Fatalf("SaveMessage(%s): %v", id, err)
		}
	}

	first, err := f.ClaimPending(ctx, 2)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first ClaimPending returned %d messages, want 2", len(first))
	}

	second, err := f.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second ClaimPending returned %d messages, want 1 (the unclaimed remainder)", len(second))
	}

	seen := map[string]bool{}
	for _, m := range first {
		seen[m.ID] = true
	}
	for _, m := range second {
		if seen[m.ID] {
			t.Fatalf("message %s claimed twice", m.ID)
		}
	}
}

func TestInboxStoreFakeClaimPendingSkipsNotYetDue(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	future := time.Now().Add(time.Hour)

	if err := f.SaveMessage(ctx, &domain.Message{ID: "future", Status: "pending", NextAttempt: &future}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := f.SaveMessage(ctx, &domain.Message{ID: "due", Status: "pending"}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := f.SaveMessage(ctx, &domain.Message{ID: "delivered", Status: "delivered"}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	claimed, err := f.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "due" {
		t.Fatalf("ClaimPending returned %v, want only [due]", claimed)
	}
}

func TestInboxStoreFakeMarkDeliveredReleasesClaim(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	if err := f.SaveMessage(ctx, &domain.Message{ID: "m1", Status: "pending"}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	if _, err := f.ClaimPending(ctx, 10); err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if err := f.MarkDelivered(ctx, "m1"); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	got, err := f.GetMessage(ctx, "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Status != "delivered" {
		t.Fatalf("Status = %q, want delivered", got.Status)
	}

	// Claim released: since status is no longer "pending", it should not
	// be reclaimed either way, but MarkFailed on a still-pending message
	// after release is exercised separately below.
}

func TestInboxStoreFakeMarkFailedReleasesClaimForRetry(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	if err := f.SaveMessage(ctx, &domain.Message{ID: "m1", Status: "pending"}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	if _, err := f.ClaimPending(ctx, 10); err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if err := f.MarkFailed(ctx, "m1", &past, "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	got, err := f.GetMessage(ctx, "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Status != "failed" || got.Attempts != 1 {
		t.Fatalf("got Status=%q Attempts=%d, want failed/1", got.Status, got.Attempts)
	}

	// Reset status to pending (as a real retry scheduler would) and
	// confirm the released claim lets it be picked up again.
	got.Status = "pending"
	got.NextAttempt = &past
	claimed, err := f.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "m1" {
		t.Fatalf("ClaimPending after retry reset = %v, want [m1]", claimed)
	}
}

func TestInboxStoreFakeGetAgentByID(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	f.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	got, err := f.GetAgentByID(ctx, 1)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	if got.Name != "alice" {
		t.Fatalf("GetAgentByID Name = %q, want alice", got.Name)
	}

	if _, err := f.GetAgentByID(ctx, 2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetAgentByID(missing) error = %v, want domain.ErrNotFound", err)
	}
}

func TestInboxStoreFakeFindAgentByTokenHash(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	f.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	got, err := f.FindAgentByTokenHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindAgentByTokenHash: %v", err)
	}
	if got.ID != 1 {
		t.Fatalf("FindAgentByTokenHash ID = %d, want 1", got.ID)
	}

	if _, err := f.FindAgentByTokenHash(ctx, "wrong-hash"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAgentByTokenHash(wrong) error = %v, want domain.ErrNotFound", err)
	}
	if _, err := f.FindAgentByTokenHash(ctx, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAgentByTokenHash(\"\") error = %v, want domain.ErrNotFound", err)
	}
}

func TestInboxStoreFakeListMessagesFilters(t *testing.T) {
	f := NewInboxStoreFake()
	ctx := context.Background()
	if err := f.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, Status: "pending", Read: false}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := f.SaveMessage(ctx, &domain.Message{ID: "m2", AgentID: 2, Status: "delivered", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	out, err := f.ListMessages(ctx, domain.MessageFilter{AgentID: 1})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(out) != 1 || out[0].ID != "m1" {
		t.Fatalf("ListMessages(AgentID=1) = %v, want [m1]", out)
	}

	out, err = f.ListMessages(ctx, domain.MessageFilter{Unread: true})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(out) != 1 || out[0].ID != "m1" {
		t.Fatalf("ListMessages(Unread=true) = %v, want [m1]", out)
	}
}
