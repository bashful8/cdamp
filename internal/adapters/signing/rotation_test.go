package signing

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

func TestRotatorRotateHappyPath(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	originalKID := signer.kid
	originalPub := signer.publicKey

	grace := 720 * time.Hour
	rotator := NewRotator(store, signer, testPassphrase, grace)

	before := time.Now().UTC()
	newKID, retiredKID, retireAt, err := rotator.Rotate(ctx)
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if retiredKID != originalKID {
		t.Errorf("retiredKID = %q, want %q (the key that was active before rotation)", retiredKID, originalKID)
	}
	if newKID == "" {
		t.Fatalf("newKID is empty")
	}
	if newKID == originalKID {
		t.Errorf("newKID = %q, want a different kid than the retired key %q", newKID, originalKID)
	}
	if retireAt.Before(before.Add(grace)) || retireAt.After(after.Add(grace)) {
		t.Errorf("retireAt = %v, want within [%v, %v] (now+grace at call time)", retireAt, before.Add(grace), after.Add(grace))
	}

	// The store must reflect the new key as active and the old key as
	// retired with the returned retireAt.
	active, err := store.GetActiveSigningKey(ctx, testPassphrase)
	if err != nil {
		t.Fatalf("GetActiveSigningKey after Rotate: %v", err)
	}
	if active.KID != newKID {
		t.Errorf("store's active key KID = %q, want %q", active.KID, newKID)
	}
	if active.PublicKey.Equal(originalPub) {
		t.Errorf("store's active key public key still matches the retired key's — new keypair was not actually generated")
	}

	previous, err := store.GetPreviousSigningKey(ctx, testPassphrase)
	if err != nil {
		t.Fatalf("GetPreviousSigningKey after Rotate: %v", err)
	}
	if previous.KID != originalKID {
		t.Errorf("store's previous key KID = %q, want %q", previous.KID, originalKID)
	}

	// The live in-process signer must be updated immediately: Sign should
	// now use the new key, not the retired one, with no restart required
	// (01-PROTOCOL.md's "outbound signing switches to the new key
	// immediately").
	canonical := []byte("researcher@example.dev|reviewer@other.dev|subject|normal||Zm9v==")
	sig, kid, err := signer.Sign(canonical)
	if err != nil {
		t.Fatalf("Sign after Rotate: %v", err)
	}
	if kid != newKID {
		t.Errorf("signer.Sign kid after Rotate = %q, want %q (the newly active key)", kid, newKID)
	}
	verifier := NewVerifier()
	if !verifier.Verify(canonical, sig, active.PublicKey) {
		t.Errorf("Verify(post-rotation signature, store's new active public key) = false, want true")
	}
}

func TestRotatorRotateSecondRotationRotatesAgain(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	rotator := NewRotator(store, signer, testPassphrase, 720*time.Hour)

	firstNewKID, _, _, err := rotator.Rotate(ctx)
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}

	secondNewKID, secondRetiredKID, _, err := rotator.Rotate(ctx)
	if err != nil {
		t.Fatalf("second Rotate: %v", err)
	}

	if secondRetiredKID != firstNewKID {
		t.Errorf("second Rotate's retiredKID = %q, want %q (the first rotation's new key)", secondRetiredKID, firstNewKID)
	}
	if secondNewKID == firstNewKID {
		t.Errorf("second Rotate's newKID = %q, same as the first rotation's new key, want a fresh kid", secondNewKID)
	}

	active, err := store.GetActiveSigningKey(ctx, testPassphrase)
	if err != nil {
		t.Fatalf("GetActiveSigningKey after two rotations: %v", err)
	}
	if active.KID != secondNewKID {
		t.Errorf("active key KID = %q, want %q (the second rotation's new key)", active.KID, secondNewKID)
	}
}

func TestRotatorRotatePropagatesSaveError(t *testing.T) {
	ctx := context.Background()
	store := &saveFailingSigningKeyStore{
		SigningKeyStoreFake: fakes.NewSigningKeyStoreFake(),
	}

	signer, err := NewSigner(ctx, store.SigningKeyStoreFake, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	originalKID := signer.kid

	rotator := NewRotator(store, signer, testPassphrase, 720*time.Hour)

	// SaveSigningKey fails after NewSigner's own bootstrap save already
	// succeeded (store.saveErr is only wired in below, after
	// NewSigner returns), so it only affects Rotate's save call.
	store.saveErr = errors.New("save boom")

	if _, _, _, err := rotator.Rotate(ctx); err == nil {
		t.Fatalf("Rotate when SaveSigningKey fails: got nil error, want an error")
	}

	// SetActiveKey must never be called before a successful save
	// (design decision 4/ordering) -- the signer's cached key must be
	// exactly what it was before the failed Rotate call.
	if signer.kid != originalKID {
		t.Errorf("signer.kid after a failed save = %q, want unchanged %q", signer.kid, originalKID)
	}
	sig, kid, err := signer.Sign([]byte("canonical"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if kid != originalKID {
		t.Errorf("Sign kid after a failed save = %q, want unchanged %q", kid, originalKID)
	}
	verifier := NewVerifier()
	if !verifier.Verify([]byte("canonical"), sig, signer.publicKey) {
		t.Errorf("Verify(post-failed-rotation signature, signer's still-original public key) = false, want true")
	}
}

// saveFailingSigningKeyStore wraps a real fakes.SigningKeyStoreFake but
// makes SaveSigningKey always fail with saveErr, used to exercise
// Rotate's save-error path (see TestRotatorRotatePropagatesSaveError).
type saveFailingSigningKeyStore struct {
	*fakes.SigningKeyStoreFake
	saveErr error
}

func (s *saveFailingSigningKeyStore) SaveSigningKey(ctx context.Context, key *domain.SigningKey, passphrase string) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.SigningKeyStoreFake.SaveSigningKey(ctx, key, passphrase)
}

var keyIDShape = regexp.MustCompile(`^k_\d+_[0-9a-z]{6}$`)

func TestNewKeyIDUniqueAcrossManyCalls(t *testing.T) {
	const n = 500
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		kid, err := newKeyID()
		if err != nil {
			t.Fatalf("newKeyID: %v", err)
		}
		if !keyIDShape.MatchString(kid) {
			t.Fatalf("newKeyID() = %q, want it to match %s", kid, keyIDShape.String())
		}
		if seen[kid] {
			t.Fatalf("newKeyID() produced a duplicate: %q", kid)
		}
		seen[kid] = true
	}
}

func TestRotatorRotatePropagatesGetActiveError(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake() // empty: no active key seeded

	rotator := NewRotator(store, &Ed25519Signer{}, testPassphrase, 720*time.Hour)

	_, _, _, err := rotator.Rotate(ctx)
	if err == nil {
		t.Fatalf("Rotate with no active key in the store: got nil error, want an error")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Rotate error = %v, want it to wrap domain.ErrNotFound", err)
	}
}

func TestRotatorRotatePropagatesRetireError(t *testing.T) {
	ctx := context.Background()
	store := &retireFailingSigningKeyStore{
		SigningKeyStoreFake: fakes.NewSigningKeyStoreFake(),
		retireErr:           errors.New("retire boom"),
	}

	signer, err := NewSigner(ctx, store.SigningKeyStoreFake, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	rotator := NewRotator(store, signer, testPassphrase, 720*time.Hour)

	_, _, _, err = rotator.Rotate(ctx)
	if err == nil {
		t.Fatalf("Rotate when RetireSigningKey fails: got nil error, want an error")
	}

	// Even though RetireSigningKey failed, the new key must already be
	// saved active and the in-process signer already switched — save-
	// before-retire is deliberate (design decision 4): a failure here
	// must never leave the store with zero active rows.
	active, activeErr := store.SigningKeyStoreFake.GetActiveSigningKey(ctx, testPassphrase)
	if activeErr != nil {
		t.Fatalf("GetActiveSigningKey after a failed retire: %v", activeErr)
	}
	if active.KID == "" {
		t.Fatalf("active key KID is empty after a failed retire")
	}
	if signer.kid != active.KID {
		t.Errorf("signer.kid = %q, want %q (in-process signer already switched despite the retire failure)", signer.kid, active.KID)
	}
}

// retireFailingSigningKeyStore wraps a real fakes.SigningKeyStoreFake but
// makes RetireSigningKey always fail, used to exercise Rotate's error path
// after the new key has already been saved and the signer already
// switched (see TestRotatorRotatePropagatesRetireError).
type retireFailingSigningKeyStore struct {
	*fakes.SigningKeyStoreFake
	retireErr error
}

func (s *retireFailingSigningKeyStore) RetireSigningKey(ctx context.Context, kid string, retireAt time.Time) error {
	return s.retireErr
}
