package app

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// messageIDRE matches SendMessage's server-assigned ID shape exactly:
// msg_<unix-seconds>_<6-char lowercase base36 random suffix>. Tests must
// never assert an exact random suffix, only this shape.
var messageIDRE = regexp.MustCompile(`^msg_[0-9]+_[0-9a-z]{6}$`)

func baseSendMessageRequest() SendMessageRequest {
	return SendMessageRequest{
		AgentID: 1,
		From:    "alice@example.dev",
		To:      "bob@other.dev",
		Subject: "hi",
		Body:    "hello",
	}
}

func TestSendMessageValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SendMessageRequest)
		presave func(t *testing.T, store *fakes.InboxStoreFake) // optional store setup
	}{
		{
			name: "missing to",
			mutate: func(r *SendMessageRequest) {
				r.To = ""
			},
		},
		{
			name: "missing subject",
			mutate: func(r *SendMessageRequest) {
				r.Subject = ""
			},
		},
		{
			name: "missing body",
			mutate: func(r *SendMessageRequest) {
				r.Body = ""
			},
		},
		{
			name: "to missing @",
			mutate: func(r *SendMessageRequest) {
				r.To = "bobother.dev"
			},
		},
		{
			name: "to uppercase local part",
			mutate: func(r *SendMessageRequest) {
				r.To = "Bob@other.dev"
			},
		},
		{
			name: "to bad characters in local part",
			mutate: func(r *SendMessageRequest) {
				r.To = "bob!@other.dev"
			},
		},
		{
			name: "to has no domain part",
			mutate: func(r *SendMessageRequest) {
				r.To = "bob@"
			},
		},
		{
			name: "body over max size",
			mutate: func(r *SendMessageRequest) {
				r.Body = strings.Repeat("a", MaxBodyBytes+1)
			},
		},
		{
			name: "unresolvable in_reply_to",
			mutate: func(r *SendMessageRequest) {
				r.InReplyTo = "msg_1700000000_zzzzzz"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := fakes.NewInboxStoreFake()
			req := baseSendMessageRequest()
			tc.mutate(&req)

			_, err := SendMessage(context.Background(), store, req)
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected domain.ErrValidation, got %v", err)
			}
		})
	}
}

func TestSendMessageNewConversationThreadID(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	res, err := SendMessage(context.Background(), store, baseSendMessageRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	saved, err := store.GetMessage(context.Background(), res.ID)
	if err != nil {
		t.Fatalf("saved message not found: %v", err)
	}
	if saved.ThreadID != saved.ID {
		t.Errorf("expected ThreadID == ID for a new conversation, got ThreadID=%q ID=%q", saved.ThreadID, saved.ID)
	}
}

func TestSendMessageReplyThreadID(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	ctx := context.Background()

	first, err := SendMessage(ctx, store, baseSendMessageRequest())
	if err != nil {
		t.Fatalf("unexpected error on first message: %v", err)
	}

	replyReq := SendMessageRequest{
		AgentID:   2,
		From:      "bob@other.dev",
		To:        "alice@example.dev",
		Subject:   "re: hi",
		Body:      "hello back",
		InReplyTo: first.ID,
	}
	reply, err := SendMessage(ctx, store, replyReq)
	if err != nil {
		t.Fatalf("unexpected error on reply: %v", err)
	}

	parent, err := store.GetMessage(ctx, first.ID)
	if err != nil {
		t.Fatalf("parent message not found: %v", err)
	}
	replyMsg, err := store.GetMessage(ctx, reply.ID)
	if err != nil {
		t.Fatalf("reply message not found: %v", err)
	}

	if replyMsg.ThreadID != parent.ThreadID {
		t.Errorf("expected reply ThreadID %q to equal parent's ThreadID %q", replyMsg.ThreadID, parent.ThreadID)
	}
	if replyMsg.ThreadID == replyMsg.ID {
		t.Errorf("expected reply ThreadID to be the parent's ThreadID, not the reply's own ID")
	}
}

func TestSendMessageHappyPath(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	res, err := SendMessage(context.Background(), store, baseSendMessageRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Status != "pending" {
		t.Errorf("expected result Status=pending, got %q", res.Status)
	}
	if !messageIDRE.MatchString(res.ID) {
		t.Errorf("id %q does not match msg_<unix-seconds>_<6-char base36> shape", res.ID)
	}

	saved, err := store.GetMessage(context.Background(), res.ID)
	if err != nil {
		t.Fatalf("saved message not found: %v", err)
	}
	if saved.Direction != "out" {
		t.Errorf("expected Direction=out, got %q", saved.Direction)
	}
	if saved.Status != "pending" {
		t.Errorf("expected Status=pending, got %q", saved.Status)
	}
	if saved.Trust != "verified" {
		t.Errorf("expected Trust=verified, got %q", saved.Trust)
	}
	if saved.NextAttempt == nil {
		t.Errorf("expected NextAttempt to be set")
	}
	if saved.Priority != "normal" {
		t.Errorf("expected default Priority=normal, got %q", saved.Priority)
	}
	if saved.Attempts != 0 {
		t.Errorf("expected Attempts=0, got %d", saved.Attempts)
	}
	if saved.Read {
		t.Errorf("expected Read=false")
	}
	if saved.SenderDomain != "example.dev" {
		t.Errorf("expected SenderDomain=example.dev, got %q", saved.SenderDomain)
	}
}

func TestSendMessageExplicitPriorityPreserved(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	req := baseSendMessageRequest()
	req.Priority = "urgent"

	res, err := SendMessage(context.Background(), store, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	saved, err := store.GetMessage(context.Background(), res.ID)
	if err != nil {
		t.Fatalf("saved message not found: %v", err)
	}
	if saved.Priority != "urgent" {
		t.Errorf("expected explicit Priority=urgent to be preserved, got %q", saved.Priority)
	}
}
