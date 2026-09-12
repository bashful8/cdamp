package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// seedListMessages saves count messages owned by agentID into store, with
// IDs derived from idOffset so multiple calls in the same test don't
// collide.
func seedListMessages(t *testing.T, store *fakes.InboxStoreFake, agentID int64, count, idOffset int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		n := idOffset + i
		id := fmt.Sprintf("msg_%d_%06d", 1700000000+n, n)
		m := &domain.Message{
			ID:        id,
			ThreadID:  id,
			AgentID:   agentID,
			Direction: "out",
			From:      "alice@example.dev",
			To:        "bob@other.dev",
			Subject:   "s",
			Body:      "b",
			SentAt:    time.Unix(int64(1700000000+n), 0).UTC(),
			Trust:     "verified",
			Status:    "pending",
		}
		if err := store.SaveMessage(ctx, m); err != nil {
			t.Fatalf("seeding message: %v", err)
		}
	}
}

func TestListMessagesNextCursor(t *testing.T) {
	tests := []struct {
		name       string
		seed       int
		limit      int
		wantCursor bool
	}{
		{name: "page full at default limit (50)", seed: 50, limit: 0, wantCursor: true},
		{name: "page not full at default limit", seed: 3, limit: 0, wantCursor: false},
		{name: "page full at explicit limit", seed: 5, limit: 5, wantCursor: true},
		{name: "page not full at explicit limit", seed: 4, limit: 5, wantCursor: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := fakes.NewInboxStoreFake()
			seedListMessages(t, store, 1, tc.seed, 0)

			res, err := ListMessages(context.Background(), store, domain.MessageFilter{AgentID: 1, Limit: tc.limit})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantCursor && res.NextCursor == nil {
				t.Errorf("expected non-nil next_cursor")
			}
			if !tc.wantCursor && res.NextCursor != nil {
				t.Errorf("expected nil next_cursor, got %q", *res.NextCursor)
			}
		})
	}
}

func TestListMessagesFilterPassthrough(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedListMessages(t, store, 1, 2, 0)
	seedListMessages(t, store, 2, 3, 100)

	res, err := ListMessages(context.Background(), store, domain.MessageFilter{AgentID: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("expected 2 messages for agent 1, got %d", len(res.Messages))
	}
	for _, m := range res.Messages {
		if m.AgentID != 1 {
			t.Errorf("expected only agent 1's messages, got AgentID=%d", m.AgentID)
		}
	}
}
