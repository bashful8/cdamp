// Package signing implements domain.Signer and domain.Verifier using
// stdlib crypto/ed25519, per the cdamp-signing skill ("use stdlib
// crypto/ed25519 directly ... unless a specific need for
// golang.org/x/crypto arises"). Only Task 1 of Phase 4 lives here: the
// Signer/Verifier implementations and first-boot key bootstrap. Building
// the canonical string from a domain.Message, wiring into cmd/cdampd, the
// /.well-known/cdamp/keys HTTP response shape, and key rotation are all
// explicitly out of scope — see STATUS.md's Task 1 spec.
package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"cdamp/internal/domain"
)

// bootstrapKID is the kid assigned to the very first signing key a fresh
// instance generates on first boot. Not pinned by any doc — the
// cdamp-signing skill's own example literally uses "k1" — and safe by
// construction here since NewSigner only assigns it when
// GetActiveSigningKey has just reported domain.ErrNotFound (i.e. the
// signing_keys table is empty, so no kid collision is possible). Phase 8's
// key rotation will need its own scheme for subsequent kids (k2, ...);
// that's not this task's concern.
const bootstrapKID = "k1"

// Ed25519Signer implements domain.Signer. It holds the local domain's
// active signing key in memory, resolved once at construction time, so
// that Sign (whose interface signature takes no context.Context and no
// store parameter) never needs a store round-trip per call.
type Ed25519Signer struct {
	mu         sync.RWMutex
	kid        string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// NewSigner resolves the local domain's active signing key from store,
// bootstrapping a new one on first boot if none exists yet, and returns
// an Ed25519Signer with that key cached in memory.
//
// Bootstrap: if store.GetActiveSigningKey reports domain.ErrNotFound (an
// empty signing_keys table), a new Ed25519 keypair is generated, assigned
// kid "k1", and saved with Active: true before proceeding — this is
// first-boot-only; any other error from GetActiveSigningKey is returned
// wrapped, not treated as "needs bootstrap."
func NewSigner(ctx context.Context, store domain.SigningKeyStore, passphrase string) (*Ed25519Signer, error) {
	key, err := store.GetActiveSigningKey(ctx, passphrase)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("loading active signing key: %w", err)
		}

		pub, priv, genErr := ed25519.GenerateKey(rand.Reader)
		if genErr != nil {
			return nil, fmt.Errorf("bootstrapping signing key: generating keypair: %w", genErr)
		}
		key = &domain.SigningKey{
			KID:        bootstrapKID,
			PublicKey:  pub,
			PrivateKey: priv,
			Active:     true,
			CreatedAt:  time.Now().UTC(),
		}
		if saveErr := store.SaveSigningKey(ctx, key, passphrase); saveErr != nil {
			return nil, fmt.Errorf("bootstrapping signing key: saving new keypair: %w", saveErr)
		}
		// Proceed with the freshly-created key as the cached active key —
		// no need to re-Get after Save.
	}

	return &Ed25519Signer{
		kid:        key.KID,
		privateKey: key.PrivateKey,
		publicKey:  key.PublicKey,
	}, nil
}

// Sign signs canonical with the signer's currently-active cached key. No
// store call happens here — all store access happened once, in NewSigner
// (subsequent updates arrive via SetActiveKey, called by key rotation).
// stdlib ed25519.Sign never returns an error, so this implementation's
// error return is always nil; it exists only to satisfy domain.Signer's
// general shape.
//
// Read-locked so a concurrent SetActiveKey (key rotation) can never be
// observed mid-swap.
func (s *Ed25519Signer) Sign(canonical []byte) (sig []byte, kid string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ed25519.Sign(s.privateKey, canonical), s.kid, nil
}

// SetActiveKey atomically replaces the signer's cached in-memory active
// key. Called by Rotator.Rotate (rotation.go) the moment the new key is
// persisted as active in the store -- without this, Sign would keep
// using the retired key in memory until the daemon restarts,
// contradicting 01-PROTOCOL.md's "all outbound signing switches to the
// new key immediately at rotation." See STATUS.md's Phase 8 task 3 spec,
// design decision 2, for why this gap existed and why this is the fix.
func (s *Ed25519Signer) SetActiveKey(kid string, priv ed25519.PrivateKey, pub ed25519.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kid = kid
	s.privateKey = priv
	s.publicKey = pub
}

// Ed25519Verifier implements domain.Verifier. It is stateless: the public
// key to verify against is supplied by the caller per call (resolved via
// domain.Directory, a later task), so no store dependency or bootstrap
// logic is needed here.
type Ed25519Verifier struct{}

// NewVerifier returns a stateless Ed25519Verifier.
func NewVerifier() *Ed25519Verifier {
	return &Ed25519Verifier{}
}

// Verify reports whether sig is a valid Ed25519 signature over canonical
// under pubkey. Per the cdamp-signing skill, a wrong/tampered signature
// returns false, not an error — verification failure is an expected,
// handled outcome for the caller to turn into a trust-level decision, not
// a fault.
//
// Defensive guard (a judgment call, not one of 01-PROTOCOL.md's three
// named trust-level triggers verbatim, but it composes correctly with
// them: a malformed key can't verify, so it naturally becomes untrusted
// upstream in ReceiveMessage): stdlib ed25519.Verify panics if
// len(pubkey) != ed25519.PublicKeySize. Since pubkey here ultimately comes
// from a remote domain's /.well-known/cdamp/keys response — an untrusted,
// attacker-influenceable input once federation exists — the length is
// checked before calling ed25519.Verify, and a malformed key returns
// false instead of panicking the process.
func (v *Ed25519Verifier) Verify(canonical, sig []byte, pubkey ed25519.PublicKey) bool {
	if len(pubkey) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pubkey, canonical, sig)
}
