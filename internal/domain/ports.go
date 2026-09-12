package domain

import (
	"context"
	"crypto/ed25519"
	"time"
)

// InboxStore persists and queries messages, threads, and the outbound
// retry queue. Backed by SQLite in v1; nothing above this interface
// should know that.
type InboxStore interface {
	SaveMessage(ctx context.Context, m *Message) error
	GetMessage(ctx context.Context, id string) (*Message, error)
	ListMessages(ctx context.Context, f MessageFilter) ([]*Message, error)
	GetThread(ctx context.Context, id string) (*Thread, []*Message, error)
	SearchThreads(ctx context.Context, agentID int64, query string, f ThreadFilter) ([]*Thread, error)

	// ClaimPending atomically selects up to `limit` outbound messages due
	// for a delivery attempt (status=pending, next_attempt<=now) so two
	// worker ticks never double-send the same message.
	ClaimPending(ctx context.Context, limit int) ([]*Message, error)
	MarkDelivered(ctx context.Context, id string) error
	MarkFailed(ctx context.Context, id string, nextAttempt *time.Time, reason string) error

	FindByIdempotencyKey(ctx context.Context, key string) (*Message, error)
}

// Directory resolves an address to the data needed to deliver to it,
// caching results for 1 hour per 01-PROTOCOL.md.
type Directory interface {
	Resolve(ctx context.Context, address string) (pubkey ed25519.PublicKey, kid, inboxURL string, err error)
}

// Signer signs on behalf of the local domain; agents never call this
// directly, only SendMessage does.
type Signer interface {
	Sign(canonical []byte) (sig []byte, kid string, err error)
}

// Verifier checks a signature against a resolved public key. Returns
// false (not an error) for "signature present but invalid" — that's a
// trust-level decision for the caller, not a hard failure.
type Verifier interface {
	Verify(canonical, sig []byte, pubkey ed25519.PublicKey) bool
}

// Delivery performs the actual outbound HTTP call to a remote instance's
// /deliver endpoint. Implementations must treat any non-200 as a failure
// for retry-scheduling purposes.
type Delivery interface {
	Deliver(ctx context.Context, m *Message, inboxURL string) error
}
