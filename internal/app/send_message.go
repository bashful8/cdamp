package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	"cdamp/internal/domain"
)

// MaxBodyBytes is the maximum allowed size, in bytes, of a message body,
// matching 03-API.md/02-ARCHITECTURE.md's documented default (256KB).
// internal/app doesn't import internal/config, so this is a package
// constant rather than something threaded in from message.default_ttl-
// style config.
const MaxBodyBytes = 262144

// toAddressRE matches 01-PROTOCOL.md's Addressing shape: a lowercase
// local-part of 1-63 chars from [a-z0-9-], an "@", then a non-empty
// domain part containing no further "@" (which is what pins this to
// "exactly one @" without over-validating the domain's own format — the
// recipient can be any external domain).
var toAddressRE = regexp.MustCompile(`^[a-z0-9-]{1,63}@[^@]+$`)

const base36Alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// SendMessageRequest is the input to SendMessage. From is the caller's
// already-resolved local address (name@domain) — turning a bearer token
// into AgentID/From is the HTTP adapter's job (Phase 3 task 2), not this
// use case's. expires_at is deliberately absent: 03-API.md's /send body
// doesn't accept it as an input field in this phase.
type SendMessageRequest struct {
	AgentID        int64
	From           string
	To             string
	Subject        string
	Body           string
	Priority       string
	InReplyTo      string
	IdempotencyKey string
}

// SendMessageResult is returned to the caller on acceptance, matching
// 03-API.md's `202 {id, status: "pending"}` shape. Turning this into the
// actual HTTP 202 response is task 2's job.
type SendMessageResult struct {
	ID     string
	Status string
}

// SendMessage validates req and, if valid, assigns every server-owned
// envelope field per 01-PROTOCOL.md (ID, SentAt, ThreadID, Direction,
// Status, Trust, NextAttempt, Attempts, Read) and persists the resulting
// message via store.SaveMessage.
//
// Signing is intentionally out of scope in this phase: domain.Message has
// no field to carry a signature yet, and the only domain.Signer
// implementation doesn't exist until Phase 4 (see STATUS.md's "Signer
// decision" for Phase 3 task 1 for the full reasoning) — so SendMessage
// takes no domain.Signer dependency and never calls Sign. That step is
// deferred to whichever component actually transmits the message later.
func SendMessage(ctx context.Context, store domain.InboxStore, req SendMessageRequest) (*SendMessageResult, error) {
	if strings.TrimSpace(req.To) == "" {
		return nil, fmt.Errorf("to is required: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(req.Subject) == "" {
		return nil, fmt.Errorf("subject is required: %w", domain.ErrValidation)
	}
	if req.Body == "" {
		return nil, fmt.Errorf("body is required: %w", domain.ErrValidation)
	}
	if !toAddressRE.MatchString(req.To) {
		return nil, fmt.Errorf("to %q is not a valid <agent-name>@<domain> address: %w", req.To, domain.ErrValidation)
	}
	if len(req.Body) > MaxBodyBytes {
		return nil, fmt.Errorf("body exceeds %d bytes: %w", MaxBodyBytes, domain.ErrValidation)
	}

	// thread_id derivation per 01-PROTOCOL.md: a brand-new (non-reply)
	// message sets thread_id = its own id; a reply's thread_id is the
	// resolved parent's own thread_id (the id of the first message in the
	// conversation), never the parent's id.
	threadID := ""
	if req.InReplyTo != "" {
		parent, err := store.GetMessage(ctx, req.InReplyTo)
		if err != nil {
			return nil, fmt.Errorf("in_reply_to %q does not resolve to an existing message: %w", req.InReplyTo, domain.ErrValidation)
		}
		threadID = parent.ThreadID
	}

	priority := req.Priority
	if priority == "" {
		priority = "normal"
	}

	id, err := newMessageID(time.Now())
	if err != nil {
		return nil, fmt.Errorf("generating message id: %w", err)
	}
	if threadID == "" {
		threadID = id
	}

	sentAt := time.Now().UTC()

	senderDomain := ""
	if i := strings.LastIndex(req.From, "@"); i >= 0 {
		senderDomain = req.From[i+1:]
	}

	m := &domain.Message{
		ID:             id,
		ThreadID:       threadID,
		AgentID:        req.AgentID,
		Direction:      "out",
		From:           req.From,
		To:             req.To,
		SenderDomain:   senderDomain,
		Subject:        req.Subject,
		Body:           req.Body,
		Priority:       priority,
		InReplyTo:      req.InReplyTo,
		IdempotencyKey: req.IdempotencyKey,
		SentAt:         sentAt,
		ExpiresAt:      nil,
		// Trust is not explicitly spec'd by 01-PROTOCOL.md for outbound
		// mail, but messages.trust is NOT NULL with a 3-value CHECK
		// constraint, so some value is required here. "verified" is the
		// defensible reading — a judgment call, not a documented fact —
		// since outbound mail originates from (and will eventually be
		// signed by) the local domain itself.
		Trust:  "verified",
		Status: "pending",
		Read:   false,
		// NextAttempt is set to SentAt (immediately claimable): Phase 5's
		// ClaimPending query filters on next_attempt<=now, so a freshly
		// accepted message needs a non-nil due time from the start.
		Attempts:    0,
		NextAttempt: &sentAt,
	}

	if err := store.SaveMessage(ctx, m); err != nil {
		return nil, fmt.Errorf("saving message: %w", err)
	}

	return &SendMessageResult{ID: m.ID, Status: m.Status}, nil
}

// newMessageID generates an ID of the form msg_<unix-seconds>_<6-char
// lowercase base36 random suffix>, per 01-PROTOCOL.md's envelope field
// notes on server-assigned IDs.
func newMessageID(now time.Time) (string, error) {
	suffix, err := randomBase36(6)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("msg_%d_%s", now.Unix(), suffix), nil
}

func randomBase36(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = base36Alphabet[int(b)%len(base36Alphabet)]
	}
	return string(out), nil
}
