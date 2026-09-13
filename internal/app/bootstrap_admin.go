package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cdamp/internal/domain"
)

// BootstrapAdminCredential ensures exactly one admin_credential row
// exists per instance lifetime, generating and persisting a fresh one
// only on the very first boot — mirrors
// internal/adapters/signing.NewSigner's bootstrap-if-absent shape: check
// the store, and only on domain.ErrNotFound, generate+persist a new
// value. On every subsequent boot this finds the existing row and
// returns created=false with no plaintext — the original value was
// shown once, on the boot that created it, and is never recoverable
// (only its hash is ever stored, same as agent tokens).
//
// Printing plaintext to stdout is the caller's job (cmd/cdampd/main.go's
// run()), not this function's: that's a process-startup I/O side effect
// belonging in the composition root, not internal/app — consistent with
// how config.Load/NewLogger's own construction is wired up only in
// main.go, and it keeps this function's own imports domain-plus-stdlib
// only, per 02-ARCHITECTURE.md's dependency rule.
func BootstrapAdminCredential(ctx context.Context, store domain.AdminStore) (plaintext string, created bool, err error) {
	_, err = store.GetAdminCredential(ctx)
	if err == nil {
		return "", false, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return "", false, fmt.Errorf("loading admin credential: %w", err)
	}

	token, err := newBearerToken()
	if err != nil {
		return "", false, fmt.Errorf("generating admin credential: %w", err)
	}

	cred := &domain.AdminCredential{
		TokenHash: hashBearerToken(token),
		CreatedAt: time.Now().UTC(),
	}
	if err := store.SaveAdminCredential(ctx, cred); err != nil {
		return "", false, fmt.Errorf("saving admin credential: %w", err)
	}

	return token, true, nil
}
