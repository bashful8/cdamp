package fakes

import (
	"context"
	"sync"

	"cdamp/internal/domain"
)

// AdminStoreFake is an in-memory domain.AdminStore, mirroring
// SigningKeyStoreFake's mutex-guarded in-memory struct style. It also
// carries a DeliveryFake-style SetGetErr forced-error injection, needed
// to test BootstrapAdminCredential's non-ErrNotFound-error propagation
// path, since nothing else in this fake's normal operation produces one.
type AdminStoreFake struct {
	mu     sync.Mutex
	cred   *domain.AdminCredential // nil until SaveAdminCredential succeeds
	getErr error                   // when set, GetAdminCredential returns this instead
}

// NewAdminStoreFake returns an empty AdminStoreFake ready to use.
func NewAdminStoreFake() *AdminStoreFake { return &AdminStoreFake{} }

// SetGetErr makes every subsequent GetAdminCredential call return err
// instead of its normal result — used to test BootstrapAdminCredential's
// handling of a real (non-ErrNotFound) store failure.
func (f *AdminStoreFake) SetGetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

// GetAdminCredential returns the forced error (if SetGetErr was called),
// else domain.ErrNotFound if no credential has been saved yet, else a
// copy of the stored credential.
func (f *AdminStoreFake) GetAdminCredential(ctx context.Context) (*domain.AdminCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.cred == nil {
		return nil, domain.ErrNotFound
	}
	cp := *f.cred
	return &cp, nil
}

// SaveAdminCredential stores a copy of c, overwriting any existing
// value — the fake itself doesn't enforce "at most once," matching how
// SigningKeyStoreFake.SaveSigningKey doesn't enforce KID-uniqueness
// either; that discipline belongs to the real caller/schema.
func (f *AdminStoreFake) SaveAdminCredential(ctx context.Context, c *domain.AdminCredential) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	cp := *c
	f.cred = &cp
	return nil
}
