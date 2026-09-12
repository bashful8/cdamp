package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// seedThread saves n messages, all belonging to threadID and matching
// subject, owned by agentID.
func seedThread(t *testing.T, store *fakes.InboxStoreFake, agentID int64, threadID, subject string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s_%02d", threadID, i)
		m := &domain.Message{
			ID:        id,
			ThreadID:  threadID,
			AgentID:   agentID,
			Direction: "out",
			From:      "alice@example.dev",
			To:        "bob@other.dev",
			Subject:   subject,
			Body:      "body",
			SentAt:    time.Now().UTC(),
			Trust:     "verified",
			Status:    "pending",
		}
		if err := store.SaveMessage(ctx, m); err != nil {
			t.Fatalf("seeding message: %v", err)
		}
	}
}

func TestSearchThreadsMessageCount(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedThread(t, store, 1, "msg_1700000000_thread1", "hello world", 3)

	res, err := SearchThreads(context.Background(), store, 1, "hello", domain.ThreadFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Threads) != 1 {
		t.Fatalf("expected 1 matching thread, got %d", len(res.Threads))
	}
	if res.Threads[0].MessageCount != 3 {
		t.Errorf("expected message_count=3, got %d", res.Threads[0].MessageCount)
	}
	if res.Threads[0].Thread.ID != "msg_1700000000_thread1" {
		t.Errorf("expected thread id msg_1700000000_thread1, got %q", res.Threads[0].Thread.ID)
	}
}

func TestSearchThreadsNextCursor(t *testing.T) {
	tests := []struct {
		name       string
		numThreads int
		limit      int
		wantCursor bool
	}{
		{name: "page full at explicit limit", numThreads: 3, limit: 3, wantCursor: true},
		{name: "page not full at explicit limit", numThreads: 2, limit: 3, wantCursor: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := fakes.NewInboxStoreFake()
			for i := 0; i < tc.numThreads; i++ {
				threadID := fmt.Sprintf("msg_%d_thread%d", 1700000000+i, i)
				seedThread(t, store, 1, threadID, "match-me", 1)
			}

			res, err := SearchThreads(context.Background(), store, 1, "match-me", domain.ThreadFilter{Limit: tc.limit})
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
