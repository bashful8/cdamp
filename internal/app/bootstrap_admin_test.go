package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"cdamp/internal/domain/fakes"
)

func TestBootstrapAdminCredential_FirstCallCreatesAndHashesCorrectly(t *testing.T) {
	store := fakes.NewAdminStoreFake()

	plaintext, created, err := BootstrapAdminCredential(context.Background(), store)
	if err != nil {
		t.Fatalf("BootstrapAdminCredential returned error: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true on first call against an empty store")
	}
	if plaintext == "" {
		t.Fatal("plaintext is empty, want a generated token")
	}

	// The single most important correctness property: the fake's stored
	// TokenHash must equal an independently computed sha256/hex of the
	// returned plaintext, mirroring create_agent_test.go's
	// TestCreateAgent_HappyPath discipline.
	sum := sha256.Sum256([]byte(plaintext))
	wantHash := hex.EncodeToString(sum[:])

	got, err := store.GetAdminCredential(context.Background())
	if err != nil {
		t.Fatalf("GetAdminCredential after bootstrap: %v", err)
	}
	if got.TokenHash != wantHash {
		t.Errorf("stored TokenHash = %q, want %q (independently computed sha256/hex of plaintext)", got.TokenHash, wantHash)
	}
}

func TestBootstrapAdminCredential_SecondCallDoesNotRegenerate(t *testing.T) {
	store := fakes.NewAdminStoreFake()
	ctx := context.Background()

	_, created1, err := BootstrapAdminCredential(ctx, store)
	if err != nil {
		t.Fatalf("first BootstrapAdminCredential returned error: %v", err)
	}
	if !created1 {
		t.Fatal("first call: created = false, want true")
	}

	before, err := store.GetAdminCredential(ctx)
	if err != nil {
		t.Fatalf("GetAdminCredential after first bootstrap: %v", err)
	}

	plaintext2, created2, err := BootstrapAdminCredential(ctx, store)
	if err != nil {
		t.Fatalf("second BootstrapAdminCredential returned error: %v", err)
	}
	if created2 {
		t.Fatal("second call: created = true, want false (already bootstrapped)")
	}
	if plaintext2 != "" {
		t.Errorf("second call: plaintext = %q, want empty", plaintext2)
	}

	after, err := store.GetAdminCredential(ctx)
	if err != nil {
		t.Fatalf("GetAdminCredential after second bootstrap: %v", err)
	}
	if after.TokenHash != before.TokenHash {
		t.Error("stored TokenHash changed after second call, want unchanged (no second generate/save)")
	}
}

func TestBootstrapAdminCredential_PropagatesNonNotFoundError(t *testing.T) {
	store := fakes.NewAdminStoreFake()
	sentinel := errors.New("store exploded")
	store.SetGetErr(sentinel)

	_, created, err := BootstrapAdminCredential(context.Background(), store)
	if err == nil {
		t.Fatal("BootstrapAdminCredential returned nil error, want the propagated store failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is(err, sentinel)", err)
	}
	if created {
		t.Error("created = true, want false on error")
	}
}
