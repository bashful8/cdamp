package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func TestGetAdminCredentialNotFoundOnEmptyTable(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.GetAdminCredential(context.Background()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetAdminCredential on empty table: got %v, want domain.ErrNotFound", err)
	}
}

func TestSaveAndGetAdminCredentialRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cred := &domain.AdminCredential{
		TokenHash: "deadbeef",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.SaveAdminCredential(ctx, cred); err != nil {
		t.Fatalf("SaveAdminCredential: %v", err)
	}

	got, err := s.GetAdminCredential(ctx)
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

func TestSaveAdminCredentialSecondCallFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first := &domain.AdminCredential{
		TokenHash: "deadbeef",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.SaveAdminCredential(ctx, first); err != nil {
		t.Fatalf("SaveAdminCredential (first): %v", err)
	}

	second := &domain.AdminCredential{
		TokenHash: "cafef00d",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.SaveAdminCredential(ctx, second); err == nil {
		t.Fatal("SaveAdminCredential (second) returned nil error, want a failure (id=1 singleton collision)")
	}
}
