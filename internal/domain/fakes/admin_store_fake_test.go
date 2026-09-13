package fakes

import (
	"context"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func TestAdminStoreFakeGetAdminCredentialNotFoundWhenEmpty(t *testing.T) {
	f := NewAdminStoreFake()
	ctx := context.Background()

	if _, err := f.GetAdminCredential(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetAdminCredential on empty fake: got %v, want domain.ErrNotFound", err)
	}
}

func TestAdminStoreFakeSaveAndGetRoundTrip(t *testing.T) {
	f := NewAdminStoreFake()
	ctx := context.Background()

	cred := &domain.AdminCredential{
		TokenHash: "deadbeef",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := f.SaveAdminCredential(ctx, cred); err != nil {
		t.Fatalf("SaveAdminCredential: %v", err)
	}

	got, err := f.GetAdminCredential(ctx)
	if err != nil {
		t.Fatalf("GetAdminCredential: %v", err)
	}
	if got.TokenHash != cred.TokenHash {
		t.Errorf("TokenHash = %q, want %q", got.TokenHash, cred.TokenHash)
	}
	if !got.CreatedAt.Equal(cred.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, cred.CreatedAt)
	}
}

func TestAdminStoreFakeSetGetErrForcesConfiguredError(t *testing.T) {
	f := NewAdminStoreFake()
	ctx := context.Background()

	// Even with a saved credential present, a forced error takes
	// precedence over the normal successful path.
	if err := f.SaveAdminCredential(ctx, &domain.AdminCredential{
		TokenHash: "deadbeef",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveAdminCredential: %v", err)
	}

	sentinel := errors.New("boom")
	f.SetGetErr(sentinel)

	if _, err := f.GetAdminCredential(ctx); !errors.Is(err, sentinel) {
		t.Fatalf("GetAdminCredential after SetGetErr: got %v, want %v", err, sentinel)
	}
}
