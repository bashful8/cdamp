package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

func TestGetThreadFound(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	ctx := context.Background()

	m := &domain.Message{
		ID:        "msg_1700000000_abc123",
		ThreadID:  "msg_1700000000_abc123",
		AgentID:   1,
		Direction: "out",
		From:      "alice@example.dev",
		To:        "bob@other.dev",
		Subject:   "hi",
		Body:      "hello",
		SentAt:    time.Unix(1700000000, 0).UTC(),
		Trust:     "verified",
		Status:    "pending",
	}
	if err := store.SaveMessage(ctx, m); err != nil {
		t.Fatalf("seeding message: %v", err)
	}

	res, err := GetThread(ctx, store, m.ThreadID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Thread.ID != m.ThreadID {
		t.Errorf("expected thread id %q, got %q", m.ThreadID, res.Thread.ID)
	}
	if len(res.Messages) != 1 || res.Messages[0].ID != m.ID {
		t.Errorf("expected messages to contain the seeded message, got %+v", res.Messages)
	}
}

func TestGetThreadNotFound(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	_, err := GetThread(context.Background(), store, "msg_does_not_exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected domain.ErrNotFound, got %v", err)
	}
}
