// Package signing's rotation.go implements Phase 8 task 3: the
// orchestration behind key rotation. Triggered exclusively by the
// admin-initiated POST /admin/keys/rotate endpoint
// (internal/adapters/http/admin.go) -- see STATUS.md's "Phase 8 task 3 —
// key rotation trigger gap (human-resolved, 2026-09-13)" for why no
// automatic/background trigger exists.
package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"cdamp/internal/domain"
)

// keyIDAlphabet is the lowercase base36 alphabet used by newKeyID,
// mirroring internal/app/send_message.go's own randomBase36 alphabet.
const keyIDAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// Rotator implements domain.KeyRotator. A new, narrow adapter type (not
// a bare function) so internal/adapters/http never needs to import
// internal/adapters/signing to invoke a rotation -- main.go (the
// composition root) constructs the concrete *Rotator and hands it to
// NewAdminMux as a domain.KeyRotator, the same "concrete adapter type ->
// port-typed parameter" shape already used for
// internal/adapters/web.NewDashboardHandler's http.Handler parameter
// (see admin.go's own NewAdminMux doc comment, and STATUS.md's Phase 8
// task 3 spec, design decision 1).
type Rotator struct {
	store      domain.SigningKeyStore
	signer     *Ed25519Signer
	passphrase string
	grace      time.Duration
}

// NewRotator returns a Rotator that rotates keys in store, keeping
// signer (the same *Ed25519Signer instance main.go also wires into
// delivery.NewClient) updated in-process immediately, using passphrase
// to encrypt/decrypt at rest and grace (cfg.KeyRotationGrace, 720h per
// 02-ARCHITECTURE.md) as every retired key's grace period.
func NewRotator(store domain.SigningKeyStore, signer *Ed25519Signer, passphrase string, grace time.Duration) *Rotator {
	return &Rotator{store: store, signer: signer, passphrase: passphrase, grace: grace}
}

// Rotate implements domain.KeyRotator. Sequencing is deliberate --
// see STATUS.md's Phase 8 task 3 spec, design decision 4, for the full
// reasoning: the new key is generated and saved as active *before* the
// old key is retired, never the reverse, so at every observable instant
// (including a process crash between the two store calls) at least one
// active=1 row exists.
func (r *Rotator) Rotate(ctx context.Context) (newKID, retiredKID string, retireAt time.Time, err error) {
	current, err := r.store.GetActiveSigningKey(ctx, r.passphrase)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("rotating signing key: loading current active key: %w", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("rotating signing key: generating new keypair: %w", err)
	}
	kid, err := newKeyID()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("rotating signing key: generating new kid: %w", err)
	}

	newKey := &domain.SigningKey{
		KID:        kid,
		PublicKey:  pub,
		PrivateKey: priv,
		Active:     true,
		CreatedAt:  time.Now().UTC(),
	}
	if err := r.store.SaveSigningKey(ctx, newKey, r.passphrase); err != nil {
		return "", "", time.Time{}, fmt.Errorf("rotating signing key: saving new key: %w", err)
	}

	// Update the live in-process signer immediately, before retiring the
	// old row -- see design decision 2. Outbound signing switches to the
	// new key from this point on, exactly per 01-PROTOCOL.md.
	r.signer.SetActiveKey(kid, priv, pub)

	retireAt = time.Now().UTC().Add(r.grace)
	if err := r.store.RetireSigningKey(ctx, current.KID, retireAt); err != nil {
		return "", "", time.Time{}, fmt.Errorf("rotating signing key: retiring old key %s: %w", current.KID, err)
	}

	return kid, current.KID, retireAt, nil
}

// newKeyID generates a fresh signing_keys.kid of the form
// k_<unix-seconds>_<6-char lowercase base36 random suffix> -- see
// STATUS.md's Phase 8 task 3 spec, design decision 5, for why this
// scheme (mirroring internal/app/send_message.go's newMessageID/
// randomBase36 exactly, duplicated rather than imported since
// randomBase36 is unexported in a different package).
func newKeyID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	suffix := make([]byte, 6)
	for i, b := range buf {
		suffix[i] = keyIDAlphabet[int(b)%len(keyIDAlphabet)]
	}
	return fmt.Sprintf("k_%d_%s", time.Now().Unix(), string(suffix)), nil
}
