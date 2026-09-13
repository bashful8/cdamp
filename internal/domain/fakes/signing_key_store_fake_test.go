package fakes

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func mustGenerateFakeKey(t *testing.T, kid string, active bool) *domain.SigningKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating ed25519 key: %v", err)
	}
	return &domain.SigningKey{
		KID:        kid,
		PublicKey:  pub,
		PrivateKey: priv,
		Active:     active,
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
	}
}

func TestSigningKeyStoreFakeSaveAndGetActiveRoundTrip(t *testing.T) {
	f := NewSigningKeyStoreFake()
	ctx := context.Background()

	key := mustGenerateFakeKey(t, "k1", true)
	if err := f.SaveSigningKey(ctx, key, "passphrase"); err != nil {
		t.Fatalf("SaveSigningKey: %v", err)
	}

	got, err := f.GetActiveSigningKey(ctx, "passphrase")
	if err != nil {
		t.Fatalf("GetActiveSigningKey: %v", err)
	}
	if got.KID != key.KID {
		t.Errorf("KID = %q, want %q", got.KID, key.KID)
	}
	if !bytes.Equal(got.PublicKey, key.PublicKey) {
		t.Errorf("PublicKey = %x, want %x", got.PublicKey, key.PublicKey)
	}
	if !bytes.Equal(got.PrivateKey, key.PrivateKey) {
		t.Errorf("PrivateKey = %x, want %x", got.PrivateKey, key.PrivateKey)
	}
	if !got.Active {
		t.Errorf("Active = false, want true")
	}
}

func TestSigningKeyStoreFakeGetActiveSigningKeyNotFound(t *testing.T) {
	f := NewSigningKeyStoreFake()
	ctx := context.Background()

	if _, err := f.GetActiveSigningKey(ctx, "pw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetActiveSigningKey on empty fake: got %v, want domain.ErrNotFound", err)
	}

	// A key exists but isn't active.
	f.AddSigningKey(mustGenerateFakeKey(t, "k1", false))
	if _, err := f.GetActiveSigningKey(ctx, "pw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetActiveSigningKey with only an inactive key: got %v, want domain.ErrNotFound", err)
	}
}

func TestSigningKeyStoreFakeGetActiveSigningKeyPicksMostRecent(t *testing.T) {
	// Defensive tie-break, mirroring keys.go's own "nothing prevents more
	// than one active row" comment: with two active keys, the most
	// recently created one wins.
	f := NewSigningKeyStoreFake()
	ctx := context.Background()

	older := mustGenerateFakeKey(t, "k1", true)
	older.CreatedAt = time.Now().Add(-1 * time.Hour)
	newer := mustGenerateFakeKey(t, "k2", true)
	newer.CreatedAt = time.Now()

	f.AddSigningKey(older)
	f.AddSigningKey(newer)

	got, err := f.GetActiveSigningKey(ctx, "pw")
	if err != nil {
		t.Fatalf("GetActiveSigningKey: %v", err)
	}
	if got.KID != "k2" {
		t.Errorf("KID = %q, want %q (the more recently created active key)", got.KID, "k2")
	}
}

func TestSigningKeyStoreFakeGetPreviousSigningKeyNotFoundWhenNoneOrExpired(t *testing.T) {
	f := NewSigningKeyStoreFake()
	ctx := context.Background()

	if _, err := f.GetPreviousSigningKey(ctx, "pw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPreviousSigningKey on empty fake: got %v, want domain.ErrNotFound", err)
	}

	// An active-only key: not a "previous" key.
	f.AddSigningKey(mustGenerateFakeKey(t, "k1", true))
	if _, err := f.GetPreviousSigningKey(ctx, "pw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPreviousSigningKey with only an active key: got %v, want domain.ErrNotFound", err)
	}

	// Retire k1 with a retire_at already in the past: past its grace
	// period, so it must NOT show up as "previous".
	if err := f.RetireSigningKey(ctx, "k1", time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}
	if _, err := f.GetPreviousSigningKey(ctx, "pw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPreviousSigningKey with retire_at in the past: got %v, want domain.ErrNotFound", err)
	}
}

func TestSigningKeyStoreFakeGetPreviousSigningKeyReturnsKeyStillInGracePeriod(t *testing.T) {
	f := NewSigningKeyStoreFake()
	ctx := context.Background()

	key := mustGenerateFakeKey(t, "k1", false)
	f.AddSigningKey(key)

	futureRetire := time.Now().Add(30 * 24 * time.Hour)
	if err := f.RetireSigningKey(ctx, "k1", futureRetire); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}

	got, err := f.GetPreviousSigningKey(ctx, "pw")
	if err != nil {
		t.Fatalf("GetPreviousSigningKey: %v", err)
	}
	if got.KID != "k1" {
		t.Errorf("KID = %q, want %q", got.KID, "k1")
	}
	if got.Active {
		t.Errorf("Active = true, want false")
	}
}

func TestSigningKeyStoreFakeGetPreviousSigningKeyBoundaryExcludesExactRetireAt(t *testing.T) {
	// Mirrors keys.go's own documented boundary judgment call: retire_at
	// is compared with strict ">" against now, so a key whose retire_at is
	// (effectively) now or in the past must not be returned.
	f := NewSigningKeyStoreFake()
	ctx := context.Background()

	key := mustGenerateFakeKey(t, "k1", false)
	f.AddSigningKey(key)

	// retire_at exactly now (already elapsed by the time the Get call
	// runs, since AddSigningKey/RetireSigningKey take nonzero time).
	if err := f.RetireSigningKey(ctx, "k1", time.Now()); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}
	if _, err := f.GetPreviousSigningKey(ctx, "pw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPreviousSigningKey with retire_at == now: got %v, want domain.ErrNotFound", err)
	}
}

func TestSigningKeyStoreFakeRetireSigningKeyNotFoundForUnknownKID(t *testing.T) {
	f := NewSigningKeyStoreFake()
	ctx := context.Background()

	if err := f.RetireSigningKey(ctx, "does-not-exist", time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("RetireSigningKey on unknown kid: got %v, want domain.ErrNotFound", err)
	}
}

func TestSigningKeyStoreFakeAddSigningKeyAndLen(t *testing.T) {
	f := NewSigningKeyStoreFake()

	if got := f.Len(); got != 0 {
		t.Fatalf("Len on empty fake = %d, want 0", got)
	}

	f.AddSigningKey(mustGenerateFakeKey(t, "k1", true))
	if got := f.Len(); got != 1 {
		t.Fatalf("Len after one AddSigningKey = %d, want 1", got)
	}

	// Seeding the same kid again overwrites rather than adding a second
	// entry, mirroring SaveSigningKey's own overwrite-by-kid behavior.
	f.AddSigningKey(mustGenerateFakeKey(t, "k1", true))
	if got := f.Len(); got != 1 {
		t.Fatalf("Len after re-adding the same kid = %d, want 1", got)
	}
}
