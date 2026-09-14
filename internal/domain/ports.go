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

	// FindAgentByName resolves a local Agent by its bare agent-name (the
	// part of an address before the "@"), added additively per STATUS.md's
	// "Agent-by-name lookup decision (human-resolved, 2026-09-12)":
	// ReceiveMessage needs to turn an inbound envelope's `to` address into
	// the local recipient's Agent.ID, and neither GetAgentByID (keyed by
	// numeric id) nor FindAgentByTokenHash (keyed by a hashed bearer token)
	// can do that lookup. Returns ErrNotFound on a miss, consistent with
	// every other InboxStore lookup method.
	FindAgentByName(ctx context.Context, name string) (*Agent, error)

	// ListAgents returns every local Agent registered with this instance,
	// ordered by ID ascending (creation order) for deterministic pagination-
	// free listing — 03-API.md's GET /admin/agents returns the full set with
	// no filter/paging params documented, so no MessageFilter/ThreadFilter-
	// style parameter is needed here. Added additively for GET /admin/agents
	// (Phase 6 task 3), the same established pattern as GetAgentByID/
	// FindAgentByTokenHash/FindAgentByName.
	ListAgents(ctx context.Context) ([]*Agent, error)

	// CreateAgent persists a newly created local Agent. a.Name and
	// a.TokenHash must already be set by the caller (internal/app's
	// CreateAgent use case generates the plaintext token and hashes it —
	// crypto/sha256, hex, the same scheme
	// internal/adapters/http/middleware.go's hashBearerToken uses —
	// before calling this; InboxStore never sees a raw secret). a.ID and
	// a.CreatedAt are assigned by this call and written back onto a.
	// Returns a wrapped ErrConflict if a.Name already exists.
	CreateAgent(ctx context.Context, a *Agent) error
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

// KeyRotator performs a signing-key rotation end to end: generate a
// fresh Ed25519 keypair, persist it as the new active key, retire the
// previously-active key with a grace-period retire_at, and update the
// live in-process signer so outbound signing switches to the new key
// immediately (01-PROTOCOL.md's "no grace period on the sending side").
// Implemented by internal/adapters/signing.Rotator. See STATUS.md's
// Phase 8 task 3 spec, design decision 1, for why this is its own
// narrow port rather than a method on SigningKeyStore/Signer, and for
// why internal/adapters/http depends on this interface rather than
// importing internal/adapters/signing directly.
type KeyRotator interface {
	// Rotate returns the newly active key's kid, the just-retired key's
	// kid, and the retired key's retire_at.
	Rotate(ctx context.Context) (newKID, retiredKID string, retireAt time.Time, err error)
}

// AdminStore persists the single bootstrap admin credential that
// authenticates dashboard/admin-API sessions. Added additively per
// STATUS.md's "Gap 2 decision — admin session auth (human-resolved,
// 2026-09-13)": kept as its own narrow port rather than folded onto
// InboxStore, mirroring SigningKeyStore's own precedent and stated
// rationale — an admin credential authenticates a human operating the
// dashboard, not an agent sending/receiving mail, one of
// 02-ARCHITECTURE.md's Auth table's three explicitly "never conflated"
// mechanisms. Exactly one row ever exists per instance — enforced at the
// schema level (0002_admin.up.sql's CHECK(id = 1)), not by this
// interface.
type AdminStore interface {
	// GetAdminCredential returns the singleton admin credential, or
	// ErrNotFound if none has been bootstrapped yet — mirrors
	// SigningKeyStore.GetActiveSigningKey's ErrNotFound-on-empty-table
	// contract exactly, so BootstrapAdminCredential (internal/app) can
	// tell "needs bootstrapping" apart from a real error the same way
	// signing.NewSigner already does.
	GetAdminCredential(ctx context.Context) (*AdminCredential, error)

	// SaveAdminCredential persists c as the singleton admin credential.
	// Called at most once per instance lifetime, by
	// BootstrapAdminCredential, only after GetAdminCredential has
	// reported ErrNotFound — enforcing that ordering is the caller's
	// job, not this method's (a second call in violation of that
	// ordering fails closed on the schema's own id=1 PRIMARY KEY, not
	// via any in-application check here).
	SaveAdminCredential(ctx context.Context, c *AdminCredential) error
}

// BlocklistStore persists and lists federation-wide blocked sender
// domains (domain_blocklist). Added additively per STATUS.md's
// "Blocklist port decision (human-resolved, 2026-09-13)": kept as its
// own dedicated port rather than folded onto InboxStore or AdminStore
// — domain_blocklist is federation-wide policy, a third category
// distinct from both "one agent's inbox" (InboxStore's own stated
// scope) and "a human's dashboard session" (AdminStore's own stated
// scope). Actual enforcement of the blocklist on POST /deliver
// (03-API.md: -> 403 blocklisted) is Phase 8 (Hardening) per
// 04-BUILD-PLAN.md — this port only backs the admin read/write API
// (03-API.md's Admin API section); nothing calls it from the
// federation-delivery path yet.
type BlocklistStore interface {
	// SaveBlocklistEntry persists e as a new domain_blocklist row.
	// Returns a wrapped ErrConflict if e.Domain is already blocklisted
	// (domain_blocklist.domain is PRIMARY KEY) — mirrors CreateAgent's
	// own UNIQUE-constraint-to-ErrConflict translation, per the
	// Blocklist port decision's resolved conflict-handling
	// sub-question (409, no idempotent-200 special case).
	SaveBlocklistEntry(ctx context.Context, e *BlocklistEntry) error

	// ListBlocklist returns every blocklisted domain, ordered by
	// Domain ascending (the table's own primary key) for deterministic
	// listing — mirrors ListAgents' own "no filter/paging params
	// documented" reasoning; 03-API.md's GET /admin/blocklist returns
	// the full set with no filter/paging shape documented either.
	// Returns nil (not an error) for zero rows, same "empty is a valid
	// non-error state" contract ListAgents already established.
	ListBlocklist(ctx context.Context) ([]*BlocklistEntry, error)
}
