package sqlite

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func mustGenerateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating ed25519 key: %v", err)
	}
	return pub, priv
}

func TestEncryptDecryptPrivateKeyRoundTrip(t *testing.T) {
	_, priv := mustGenerateKey(t)

	blob, err := encryptPrivateKey(priv, "correct horse battery staple")
	if err != nil {
		t.Fatalf("encryptPrivateKey: %v", err)
	}

	got, err := decryptPrivateKey(blob, "correct horse battery staple")
	if err != nil {
		t.Fatalf("decryptPrivateKey: %v", err)
	}

	if !bytes.Equal(got, priv) {
		t.Fatalf("round-tripped private key does not match original\ngot:  %x\nwant: %x", got, priv)
	}
}

func TestDecryptPrivateKeyWrongPassphraseFailsCleanly(t *testing.T) {
	_, priv := mustGenerateKey(t)

	blob, err := encryptPrivateKey(priv, "the right passphrase")
	if err != nil {
		t.Fatalf("encryptPrivateKey: %v", err)
	}

	got, err := decryptPrivateKey(blob, "the wrong passphrase")
	if err == nil {
		t.Fatalf("decryptPrivateKey with wrong passphrase: got nil error, key = %x", got)
	}
	if got != nil {
		t.Fatalf("decryptPrivateKey with wrong passphrase: expected nil key alongside error, got %x", got)
	}
}

func TestDecryptPrivateKeyTruncatedBlobFailsCleanly(t *testing.T) {
	_, priv := mustGenerateKey(t)

	blob, err := encryptPrivateKey(priv, "passphrase")
	if err != nil {
		t.Fatalf("encryptPrivateKey: %v", err)
	}

	// Truncate below the minimum header length (version + salt + nonce).
	short := blob[:5]
	if _, err := decryptPrivateKey(short, "passphrase"); err == nil {
		t.Fatalf("decryptPrivateKey on truncated blob: expected error, got nil")
	}
}

func newTestSigningKey(t *testing.T, kid string, active bool) *domain.SigningKey {
	t.Helper()
	pub, priv := mustGenerateKey(t)
	return &domain.SigningKey{
		KID:        kid,
		PublicKey:  pub,
		PrivateKey: priv,
		Active:     active,
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
	}
}

func TestSaveSigningKeyAndGetActiveSigningKeyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	key := newTestSigningKey(t, "k1", true)
	const passphrase = "s3cr3t-passphrase"

	if err := s.SaveSigningKey(ctx, key, passphrase); err != nil {
		t.Fatalf("SaveSigningKey: %v", err)
	}

	got, err := s.GetActiveSigningKey(ctx, passphrase)
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
	if !got.CreatedAt.Equal(key.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, key.CreatedAt)
	}
}

func TestGetActiveSigningKeyNotFoundWhenNoActiveKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// No rows at all.
	if _, err := s.GetActiveSigningKey(ctx, "whatever"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetActiveSigningKey on empty table: got %v, want domain.ErrNotFound", err)
	}

	// A row exists but isn't active.
	key := newTestSigningKey(t, "k1", false)
	if err := s.SaveSigningKey(ctx, key, "pw"); err != nil {
		t.Fatalf("SaveSigningKey: %v", err)
	}
	if _, err := s.GetActiveSigningKey(ctx, "pw"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetActiveSigningKey with only an inactive row: got %v, want domain.ErrNotFound", err)
	}
}

func TestGetPreviousSigningKeyNotFoundWhenNoneOrExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const passphrase = "pw"

	// No rows at all.
	if _, err := s.GetPreviousSigningKey(ctx, passphrase); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPreviousSigningKey on empty table: got %v, want domain.ErrNotFound", err)
	}

	// An active-only key: not a "previous" key.
	active := newTestSigningKey(t, "k1", true)
	if err := s.SaveSigningKey(ctx, active, passphrase); err != nil {
		t.Fatalf("SaveSigningKey: %v", err)
	}
	if _, err := s.GetPreviousSigningKey(ctx, passphrase); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPreviousSigningKey with only an active row: got %v, want domain.ErrNotFound", err)
	}

	// Retire k1 with a retire_at already in the past: past its grace
	// period, so it must NOT show up as "previous" (per cdamp-signing's
	// "/.well-known/cdamp/keys" wording: omitted entirely once past
	// retire_at).
	pastRetire := time.Now().Add(-1 * time.Hour)
	if err := s.RetireSigningKey(ctx, "k1", pastRetire); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}
	if _, err := s.GetPreviousSigningKey(ctx, passphrase); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPreviousSigningKey with retire_at in the past: got %v, want domain.ErrNotFound", err)
	}
}

func TestGetPreviousSigningKeyReturnsKeyStillInGracePeriod(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const passphrase = "pw"

	key := newTestSigningKey(t, "k1", false)
	if err := s.SaveSigningKey(ctx, key, passphrase); err != nil {
		t.Fatalf("SaveSigningKey: %v", err)
	}

	futureRetire := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	if err := s.RetireSigningKey(ctx, "k1", futureRetire); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}

	got, err := s.GetPreviousSigningKey(ctx, passphrase)
	if err != nil {
		t.Fatalf("GetPreviousSigningKey: %v", err)
	}
	if got.KID != "k1" {
		t.Errorf("KID = %q, want %q", got.KID, "k1")
	}
	if !bytes.Equal(got.PrivateKey, key.PrivateKey) {
		t.Errorf("PrivateKey = %x, want %x", got.PrivateKey, key.PrivateKey)
	}
	if got.Active {
		t.Errorf("Active = true, want false")
	}
}

func TestRetireSigningKeyNotFoundForUnknownKID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.RetireSigningKey(ctx, "does-not-exist", time.Now())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("RetireSigningKey on unknown kid: got %v, want domain.ErrNotFound", err)
	}
}

func TestRetireSigningKeyUpdatesActiveKeySoGetActiveSigningKeyReflectsIt(t *testing.T) {
	// This exercises RetireSigningKey's plain SQL update in isolation: it
	// only sets active=0/retire_at on the named row, with no rotation
	// orchestration (that's Phase 8) to insert a replacement active key.
	// Retiring the sole active key without inserting a new one leaves the
	// table with zero active rows — a valid state for this method to
	// produce even though a real rotation caller would always pair it
	// with inserting a new active row first; this test only proves the
	// update itself is correctly visible afterward, not that this
	// sequence is how Phase 8's orchestration will call it.
	s := newTestStore(t)
	ctx := context.Background()
	const passphrase = "pw"

	key := newTestSigningKey(t, "k1", true)
	if err := s.SaveSigningKey(ctx, key, passphrase); err != nil {
		t.Fatalf("SaveSigningKey: %v", err)
	}

	if _, err := s.GetActiveSigningKey(ctx, passphrase); err != nil {
		t.Fatalf("GetActiveSigningKey before retire: %v", err)
	}

	retireAt := time.Now().Add(30 * 24 * time.Hour)
	if err := s.RetireSigningKey(ctx, "k1", retireAt); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}

	if _, err := s.GetActiveSigningKey(ctx, passphrase); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetActiveSigningKey after retiring the only active key: got %v, want domain.ErrNotFound", err)
	}

	// And it should now show up as the previous/grace-period key.
	prev, err := s.GetPreviousSigningKey(ctx, passphrase)
	if err != nil {
		t.Fatalf("GetPreviousSigningKey after retire: %v", err)
	}
	if prev.KID != "k1" {
		t.Errorf("KID = %q, want %q", prev.KID, "k1")
	}
}
