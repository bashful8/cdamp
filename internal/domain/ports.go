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

	// GetAgentByID and FindAgentByTokenHash resolve a local Agent record —
	// added additively per STATUS.md's "Agent-lookup decision
	// (human-resolved, 2026-09-12)": 02-ARCHITECTURE.md's SendMessage flow
	// requires resolving a bearer token to a domain.Agent, but neither this
	// interface nor a separate port anywhere provided a way to do that
	// lookup. Both return ErrNotFound on a miss, consistent with every
	// other InboxStore method. FindAgentByTokenHash takes an already-hashed
	// token (see the go-hexagonal-style skill: ports never take
	// adapter-specific or raw-secret types) — callers hash the raw bearer
	// token (crypto/sha256, hex-encoded) before calling this.
	GetAgentByID(ctx context.Context, id int64) (*Agent, error)
	FindAgentByTokenHash(ctx context.Context, tokenHash string) (*Agent, error)
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

// SigningKeyStore persists and retrieves the local domain's Ed25519
// signing key lifecycle (bootstrap, active/previous lookup, retirement).
// Added additively per STATUS.md's "Signing-key port decision
// (human-resolved, 2026-09-12)": signing-key lifecycle is a genuinely
// separate concern from messages/threads/agents, so it gets its own port
// rather than folding onto InboxStore, whose own doc comment scopes it to
// "messages, threads, and the outbound retry queue". *sqlite.Store already
// implements this exact shape (internal/adapters/storage/sqlite/keys.go,
// verified in Phase 2) — this is a pure interface declaration, no new
// adapter code. passphrase is a plain string, carrying forward keys.go's
// own already-approved Phase 2 shape rather than introducing a new one.
type SigningKeyStore interface {
	SaveSigningKey(ctx context.Context, key *SigningKey, passphrase string) error
	GetActiveSigningKey(ctx context.Context, passphrase string) (*SigningKey, error)
	GetPreviousSigningKey(ctx context.Context, passphrase string) (*SigningKey, error)
	RetireSigningKey(ctx context.Context, kid string, retireAt time.Time) error
}
