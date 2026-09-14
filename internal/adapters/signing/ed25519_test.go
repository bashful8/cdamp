package signing

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

const testPassphrase = "s3cr3t-passphrase"

func TestSignVerifyRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	canonical := []byte("researcher@example.dev|reviewer@other.dev|Question about the API|normal||Zm9v==")
	sig, kid, err := signer.Sign(canonical)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if kid == "" {
		t.Fatalf("Sign returned empty kid")
	}

	verifier := NewVerifier()
	if !verifier.Verify(canonical, sig, signer.publicKey) {
		t.Fatalf("Verify(canonical, sig, signer's own public key) = false, want true")
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	canonicalA := []byte("researcher@example.dev|reviewer@other.dev|Question about the API|normal||Zm9v==")
	canonicalB := []byte("researcher@example.dev|reviewer@other.dev|A different subject|normal||Zm9v==")

	sig, _, err := signer.Sign(canonicalA)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	verifier := NewVerifier()
	if verifier.Verify(canonicalB, sig, signer.publicKey) {
		t.Fatalf("Verify(tampered canonical, original sig, signer's public key) = true, want false")
	}
}

func TestVerifyRejectsWrongPublicKey(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	canonical := []byte("researcher@example.dev|reviewer@other.dev|Question about the API|normal||Zm9v==")
	sig, _, err := signer.Sign(canonical)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating unrelated keypair: %v", err)
	}

	verifier := NewVerifier()
	if verifier.Verify(canonical, sig, otherPub) {
		t.Fatalf("Verify(canonical, sig, unrelated public key) = true, want false")
	}
}

func TestVerifyMalformedPublicKeyLengthDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Verify panicked on malformed pubkey length: %v", r)
		}
	}()

	verifier := NewVerifier()
	canonical := []byte("from|to|subject|normal||hash")
	sig := make([]byte, ed25519.SignatureSize)

	shortKey := ed25519.PublicKey(make([]byte, 4)) // wrong length, not ed25519.PublicKeySize
	if got := verifier.Verify(canonical, sig, shortKey); got {
		t.Errorf("Verify with malformed (too short) pubkey = true, want false")
	}

	longKey := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize*2))
	if got := verifier.Verify(canonical, sig, longKey); got {
		t.Errorf("Verify with malformed (too long) pubkey = true, want false")
	}
}

func TestNewSignerBootstrapPersistsKey(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	saved, err := store.GetActiveSigningKey(ctx, testPassphrase)
	if err != nil {
		t.Fatalf("GetActiveSigningKey after bootstrap: %v", err)
	}
	if !saved.Active {
		t.Errorf("saved key Active = false, want true")
	}
	if saved.KID != signer.kid {
		t.Errorf("saved key KID = %q, want %q (signer's cached kid)", saved.KID, signer.kid)
	}
	if !saved.PublicKey.Equal(signer.publicKey) {
		t.Errorf("saved key PublicKey = %x, want %x (signer's cached public key)", saved.PublicKey, signer.publicKey)
	}
}

func TestNewSignerBootstrapIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	// Seed an active key directly (not via NewSigner), simulating a
	// second boot against an already-initialized store.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating seed keypair: %v", err)
	}
	seeded := &domain.SigningKey{
		KID:        "k1",
		PublicKey:  pub,
		PrivateKey: priv,
		Active:     true,
		CreatedAt:  time.Now().UTC(),
	}
	store.AddSigningKey(seeded)

	if got := store.Len(); got != 1 {
		t.Fatalf("store.Len() before NewSigner = %d, want 1", got)
	}

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	if got := store.Len(); got != 1 {
		t.Fatalf("store.Len() after NewSigner against a pre-seeded store = %d, want 1 (no second row created)", got)
	}
	if signer.kid != seeded.KID {
		t.Errorf("signer.kid = %q, want %q (loaded the seeded key, not a fresh bootstrap)", signer.kid, seeded.KID)
	}
	if !signer.publicKey.Equal(seeded.PublicKey) {
		t.Errorf("signer.publicKey = %x, want %x (loaded the seeded key's public key)", signer.publicKey, seeded.PublicKey)
	}
}

func TestNewSignerPropagatesNonNotFoundStoreError(t *testing.T) {
	ctx := context.Background()
	errStore := &erroringSigningKeyStore{err: errors.New("boom")}

	_, err := NewSigner(ctx, errStore, testPassphrase)
	if err == nil {
		t.Fatalf("NewSigner with a failing (non-ErrNotFound) store: got nil error, want a wrapped error")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("NewSigner error unexpectedly satisfies errors.Is(domain.ErrNotFound): %v", err)
	}
}

// erroringSigningKeyStore is a minimal domain.SigningKeyStore whose
// GetActiveSigningKey always fails with a non-ErrNotFound error, used to
// confirm NewSigner distinguishes "needs bootstrap" from a genuine store
// failure rather than treating every error as "needs bootstrap."
type erroringSigningKeyStore struct {
	err error
}

func (e *erroringSigningKeyStore) SaveSigningKey(ctx context.Context, key *domain.SigningKey, passphrase string) error {
	return e.err
}

func (e *erroringSigningKeyStore) GetActiveSigningKey(ctx context.Context, passphrase string) (*domain.SigningKey, error) {
	return nil, e.err
}

func (e *erroringSigningKeyStore) GetPreviousSigningKey(ctx context.Context, passphrase string) (*domain.SigningKey, error) {
	return nil, e.err
}

func (e *erroringSigningKeyStore) RetireSigningKey(ctx context.Context, kid string, retireAt time.Time) error {
	return e.err
}

func TestSignReflectsMostRecentSetActiveKey(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating second keypair: %v", err)
	}
	signer.SetActiveKey("k2", priv, pub)

	canonical := []byte("researcher@example.dev|reviewer@other.dev|subject|normal||Zm9v==")
	sig, kid, err := signer.Sign(canonical)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if kid != "k2" {
		t.Errorf("Sign kid after SetActiveKey = %q, want k2", kid)
	}

	verifier := NewVerifier()
	if !verifier.Verify(canonical, sig, pub) {
		t.Errorf("Verify(sig, second key's public key) = false, want true")
	}
	if verifier.Verify(canonical, sig, signer.publicKey) && !signer.publicKey.Equal(pub) {
		t.Errorf("signature unexpectedly verifies against a stale public key")
	}
}

// TestSignAndSetActiveKeyConcurrentAccessDoesNotRace exercises Sign's
// RLock and SetActiveKey's Lock concurrently -- run with `go test -race`,
// the whole point of the mu sync.RWMutex added for key rotation (see
// STATUS.md's Phase 8 task 3 spec, design decision 2). It doesn't assert
// on Sign's output (which key wins a given call is inherently racy by
// design), only that concurrent access is never observed as a data race
// and every returned signature verifies against a key SetActiveKey
// installed (never a torn kid/privateKey/publicKey combination).
func TestSignAndSetActiveKeyConcurrentAccessDoesNotRace(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewSigningKeyStoreFake()

	signer, err := NewSigner(ctx, store, testPassphrase)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	const rotations = 20
	const signsPerRotation = 20

	// keysByKID lets the verifying goroutine check each signature against
	// the exact public key that was active under that kid, so a torn
	// (kid, publicKey) pair would fail verification instead of merely
	// going undetected.
	var mu sync.Mutex
	keysByKID := map[string]ed25519.PublicKey{signer.kid: signer.publicKey}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < rotations; i++ {
			pub, priv, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Errorf("generating rotation keypair: %v", err)
				return
			}
			kid := "rotated-key"
			if i%2 == 1 {
				kid = "rotated-key-alt"
			}
			mu.Lock()
			keysByKID[kid] = pub
			mu.Unlock()
			signer.SetActiveKey(kid, priv, pub)
		}
	}()

	go func() {
		defer wg.Done()
		verifier := NewVerifier()
		canonical := []byte("researcher@example.dev|reviewer@other.dev|subject|normal||Zm9v==")
		for i := 0; i < rotations*signsPerRotation; i++ {
			sig, kid, err := signer.Sign(canonical)
			if err != nil {
				t.Errorf("Sign: %v", err)
				return
			}
			mu.Lock()
			pub, ok := keysByKID[kid]
			mu.Unlock()
			if !ok {
				t.Errorf("Sign returned kid %q that was never installed via SetActiveKey", kid)
				return
			}
			if !verifier.Verify(canonical, sig, pub) {
				t.Errorf("Verify(sig, public key for kid %q) = false, want true (torn read of kid/private/public key)", kid)
				return
			}
		}
	}()

	wg.Wait()
}
