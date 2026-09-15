package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"cdamp/internal/domain"
)

// protocolVersion is the only "version" value ReceiveMessage accepts on an
// inbound envelope, per 01-PROTOCOL.md's Envelope shape.
const protocolVersion = "cdamp/0.1"

// ReceiveMessageRequest mirrors 01-PROTOCOL.md's Envelope fields 1:1,
// already JSON-decoded by federation.go (task 4). ReceiveMessage itself
// takes no adapter-specific types (no http.Request, no json.RawMessage),
// per the go-hexagonal-style skill. Signature/KID are both "" when the
// envelope carried no signature at all (external trust case) —
// federation.go must not synthesize empty-string defaults for a field that
// actually came through with a value; "" means "absent."
type ReceiveMessageRequest struct {
	Version        string
	ID             string
	From           string
	To             string
	Subject        string
	Priority       string
	Timestamp      time.Time
	ExpiresAt      *time.Time
	InReplyTo      string
	ThreadID       string
	IdempotencyKey string
	Signature      string // base64, "" if absent
	KID            string // "" if absent
	PayloadType    string
	PayloadMessage string
	PayloadContext map[string]any // nil/empty if absent
}

// ReceiveMessageResult signals the outcome to task 4's HTTP handler, which
// owns the actual status-code mapping (03-API.md: /deliver always returns
// 200 for both a fresh accept and a duplicate no-op — this struct just
// distinguishes the two cases for whatever the caller wants to do with
// that, e.g. logging).
type ReceiveMessageResult struct {
	ID        string
	Duplicate bool
	Trust     string // "verified" | "external" | "untrusted"; "" if Duplicate
}

// ReceiveMessage implements 02-ARCHITECTURE.md's ReceiveMessage flow: it is
// called by task 4's /deliver handler after size-limit/rate-limit/blocklist
// checks (all HTTP-layer, task 4/Phase 8) and after the request body has
// been JSON-decoded into req.
//
// Pipeline (order matters, per 02-ARCHITECTURE.md and the cdamp-federation
// skill):
//  1. Validate required fields.
//  2. Dedupe on idempotency_key if present, else (sender_domain, id).
//  3. Resolve the local recipient from req.To's agent-name part.
//  4. Assign a trust level per 01-PROTOCOL.md's three-row table.
//  5. Save with direction=in, status=received.
func ReceiveMessage(
	ctx context.Context,
	store domain.InboxStore,
	directory domain.Directory,
	verifier domain.Verifier,
	req ReceiveMessageRequest,
) (*ReceiveMessageResult, error) {
	if req.Version != protocolVersion {
		return nil, fmt.Errorf("version %q is not recognized: %w", req.Version, domain.ErrValidation)
	}
	if strings.TrimSpace(req.ID) == "" {
		return nil, fmt.Errorf("id is required: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(req.From) == "" {
		return nil, fmt.Errorf("from is required: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(req.To) == "" {
		return nil, fmt.Errorf("to is required: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(req.Subject) == "" {
		return nil, fmt.Errorf("subject is required: %w", domain.ErrValidation)
	}

	senderDomain := domainPart(req.From)

	// Dedupe: idempotency_key first if present, else (sender_domain, id).
	if req.IdempotencyKey != "" {
		existing, err := store.FindByIdempotencyKey(ctx, req.IdempotencyKey)
		if err == nil {
			return &ReceiveMessageResult{ID: existing.ID, Duplicate: true}, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("checking idempotency key: %w", err)
		}
	} else {
		existing, err := store.GetMessage(ctx, req.ID)
		if err == nil && existing.SenderDomain == senderDomain {
			return &ReceiveMessageResult{ID: existing.ID, Duplicate: true}, nil
		}
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("checking existing message: %w", err)
		}
	}

	// Resolve the local recipient: req.To must be a valid
	// <agent-name>@<domain> address (the same "exactly one @" shape
	// SendMessage validates its own `to` against), then the agent-name part
	// must resolve to a local agent.
	if !toAddressRE.MatchString(req.To) {
		return nil, fmt.Errorf("to %q is not a valid <agent-name>@<domain> address: %w", req.To, domain.ErrValidation)
	}
	agentName := req.To[:strings.Index(req.To, "@")]
	recipient, err := store.FindAgentByName(ctx, agentName)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("to %q does not resolve to a local agent: %w", req.To, domain.ErrValidation)
		}
		return nil, fmt.Errorf("resolving local recipient: %w", err)
	}

	trust := assignTrust(ctx, directory, verifier, req)

	sentAt := req.Timestamp
	if sentAt.IsZero() {
		sentAt = time.Now().UTC()
	}

	m := &domain.Message{
		ID:             req.ID,
		ThreadID:       req.ThreadID,
		AgentID:        recipient.ID,
		Direction:      "in",
		From:           req.From,
		To:             req.To,
		SenderDomain:   senderDomain,
		Subject:        req.Subject,
		Body:           req.PayloadMessage,
		Priority:       req.Priority,
		InReplyTo:      req.InReplyTo,
		IdempotencyKey: req.IdempotencyKey,
		SentAt:         sentAt,
		ExpiresAt:      req.ExpiresAt,
		Trust:          trust,
		Status:         "received",
		Read:           false,
	}
	if m.ThreadID == "" {
		m.ThreadID = m.ID
	}

	if err := store.SaveMessage(ctx, m); err != nil {
		return nil, fmt.Errorf("saving message: %w", err)
	}

	return &ReceiveMessageResult{ID: m.ID, Duplicate: false, Trust: m.Trust}, nil
}

// assignTrust implements 01-PROTOCOL.md's three-row trust-level table,
// including its grace-period row: "verified | Signature checks out |
// Verify() succeeds against the sender domain's current OR previous
// (grace-period) key" (01-PROTOCOL.md line 123).
//
//   - No signature/kid at all -> "external". Directory.Resolve is
//     deliberately never called in this branch: there's nothing to verify
//     against, so skipping it avoids a wasted network round-trip for every
//     unsigned message.
//   - Signature+kid present: Verify is tried first against the sender
//     domain's current key (Directory.Resolve). If that fails for any
//     reason -- Resolve itself erroring (domain/agent unreachable or
//     unknown), or Verify reporting false -- it is tried a second time
//     against the domain's previous, grace-period key
//     (Directory.ResolvePrevious), added in Phase 8 task 5 specifically
//     for this fallback. ResolvePrevious is only ever called after the
//     current-key attempt has already failed, never in its place or in
//     parallel with it, so a message signed with the still-active current
//     key never pays for a second HTTP round-trip.
//   - Either attempt succeeding -> "verified". Both failing -> "untrusted".
//     Neither a Resolve/ResolvePrevious error nor a failed Verify is
//     treated as a hard error that aborts the request -- the message is
//     still stored and tagged, per 01-PROTOCOL.md's "why store untrusted
//     mail" rationale.
func assignTrust(ctx context.Context, directory domain.Directory, verifier domain.Verifier, req ReceiveMessageRequest) string {
	if req.Signature == "" && req.KID == "" {
		return "external"
	}

	payload := map[string]any{
		"type":    req.PayloadType,
		"message": req.PayloadMessage,
		"context": req.PayloadContext,
	}
	canonical, err := CanonicalString(req.From, req.To, req.Subject, req.Priority, req.InReplyTo, payload)
	if err != nil {
		return "untrusted"
	}

	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		return "untrusted"
	}

	if pubkey, _, _, err := directory.Resolve(ctx, req.From); err == nil && verifier.Verify(canonical, sig, pubkey) {
		return "verified"
	}

	if pubkey, _, err := directory.ResolvePrevious(ctx, req.From); err == nil && verifier.Verify(canonical, sig, pubkey) {
		return "verified"
	}

	return "untrusted"
}

// domainPart returns the domain part of a "<name>@<domain>" address, or ""
// if addr has no "@" — mirrors SendMessage's own senderDomain derivation
// exactly (internal/app/send_message.go), so both use cases treat the same
// address shape identically.
func domainPart(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}
