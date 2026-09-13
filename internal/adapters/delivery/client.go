package delivery

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/domain"
)

// deliverTimeout bounds a single delivery attempt so a hung remote cannot
// stall the worker tick indefinitely — a context deadline per attempt (per
// the cdamp-federation skill), not a client-wide timeout shared with
// unrelated calls. Not pinned by any doc; a defensible default in the same
// category as directory.go's own 10s fallback.
const deliverTimeout = 10 * time.Second

// protocolVersion is this adapter's own copy of internal/app's unexported
// protocolVersion constant — that constant cannot be imported across the
// package boundary, so it's redeclared here per 01-PROTOCOL.md's fixed
// "cdamp/0.1" wire version.
const protocolVersion = "cdamp/0.1"

// deliverPayload mirrors 01-PROTOCOL.md's Envelope `payload` object:
// {type, message, context}. Local to this package — see clientEnvelope's
// doc comment for why this isn't shared with internal/adapters/http's own
// copy.
type deliverPayload struct {
	Type    string         `json:"type"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

// clientEnvelope mirrors 01-PROTOCOL.md's wire Envelope shape 1:1: version,
// id, from, to, subject, priority, timestamp, expires_at, signature, kid,
// in_reply_to, thread_id, idempotency_key, payload.
//
// This is intentionally its own type, duplicating internal/adapters/http's
// unexported deliverEnvelope/deliverPayload: adapters never import each
// other (go-hexagonal-style), and there is no shared domain/app-level wire
// type to reuse instead — inventing one now would be new, undocumented
// scope beyond this task.
type clientEnvelope struct {
	Version        string         `json:"version"`
	ID             string         `json:"id"`
	From           string         `json:"from"`
	To             string         `json:"to"`
	Subject        string         `json:"subject"`
	Priority       string         `json:"priority"`
	Timestamp      time.Time      `json:"timestamp"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	Signature      string         `json:"signature,omitempty"`
	KID            string         `json:"kid,omitempty"`
	InReplyTo      string         `json:"in_reply_to,omitempty"`
	ThreadID       string         `json:"thread_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Payload        deliverPayload `json:"payload"`
}

// Client implements domain.Delivery: it rebuilds a persisted outbound
// domain.Message into 01-PROTOCOL.md's wire Envelope, signs it with the
// local domain's active signing key, and POSTs it to the recipient
// instance's /deliver endpoint.
type Client struct {
	signer     domain.Signer
	httpClient *http.Client
}

// NewClient returns a Client that signs with signer and delivers over a
// plain stdlib *http.Client (no third-party HTTP dependency), mirroring
// directory.go's own established pattern for outbound adapters.
func NewClient(signer domain.Signer) *Client {
	return &Client{
		signer:     signer,
		httpClient: &http.Client{},
	}
}

// Deliver implements domain.Delivery. It signs a fresh envelope for m on
// every call — no signature is persisted or reused across retry attempts,
// since 01-PROTOCOL.md's Key rotation section guarantees outbound signing
// always uses whatever key is currently active, with no correctness
// requirement that retries reuse the same signature — and makes exactly
// one HTTP attempt; retry scheduling is the caller's (worker.go's) job.
func (c *Client) Deliver(ctx context.Context, m *domain.Message, inboxURL string) error {
	// payload is built once and reused for both the canonical string and
	// the envelope's own payload field, never rebuilt twice, so signing
	// and verification can never derive the hash from diverging values.
	//
	// domain.Message has no PayloadType/PayloadContext fields (03-API.md's
	// /send body never lets a caller set them either), so this adapter
	// synthesizes a fixed constant type and empty context — safe for
	// signature correctness because the canonical string and the wire
	// payload are computed from this exact same map value.
	payload := map[string]any{
		"type":    "request",
		"message": m.Body,
		"context": map[string]any{},
	}

	canonical, err := app.CanonicalString(m.From, m.To, m.Subject, m.Priority, m.InReplyTo, payload)
	if err != nil {
		return fmt.Errorf("delivering %s: building canonical string: %w", m.ID, err)
	}

	sig, kid, err := c.signer.Sign(canonical)
	if err != nil {
		return fmt.Errorf("delivering %s: signing envelope: %w", m.ID, err)
	}
	signature := base64.StdEncoding.EncodeToString(sig)

	env := clientEnvelope{
		Version:        protocolVersion,
		ID:             m.ID,
		From:           m.From,
		To:             m.To,
		Subject:        m.Subject,
		Priority:       m.Priority,
		Timestamp:      m.SentAt,
		ExpiresAt:      m.ExpiresAt,
		Signature:      signature,
		KID:            kid,
		InReplyTo:      m.InReplyTo,
		ThreadID:       m.ThreadID,
		IdempotencyKey: m.IdempotencyKey,
		Payload: deliverPayload{
			Type:    "request",
			Message: m.Body,
			Context: map[string]any{},
		},
	}

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("delivering %s: marshaling envelope: %w", m.ID, err)
	}

	attemptCtx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, inboxURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("delivering %s: building request: %w", m.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delivering %s: performing request: %w", m.ID, err)
	}
	defer resp.Body.Close()

	// Per domain.Delivery's own doc comment, any non-200 is a failure for
	// retry-scheduling purposes — no special-casing of specific status
	// codes here (that distinction is Phase 8's concern, not this one's).
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delivering %s: unexpected status %d", m.ID, resp.StatusCode)
	}

	return nil
}
