package fakes

import (
	"context"
	"sync"
	"time"

	"cdamp/internal/domain"
)

// signingKeyRecord pairs a domain.SigningKey with the retire_at timestamp
// keys.go's real schema stores alongside it. domain.SigningKey itself
// carries no RetireAt field (see internal/domain/signing_key.go, and
// keys.go's scanSigningKey, which never selects retire_at back out — only
// kid, public_key, private_key_encrypted, active, created_at), so the fake
// tracks retire_at separately in order to reproduce
// GetPreviousSigningKey's "active=0 AND retire_at > now" filter. A zero
// retireAt means "never retired" (retire_at NULL in the real schema),
// which must never satisfy that filter.
type signingKeyRecord struct {
	key      *domain.SigningKey
	retireAt time.Time
}

// SigningKeyStoreFake is an in-memory domain.SigningKeyStore, keyed by
// kid, per the go-hexagonal-style skill's "hand-written fake" convention
// (real, if simplified, behavior — no mocking library).
type SigningKeyStoreFake struct {
	mu   sync.Mutex
	keys map[string]*signingKeyRecord
}

// NewSigningKeyStoreFake returns an empty SigningKeyStoreFake ready to use.
func NewSigningKeyStoreFake() *SigningKeyStoreFake {
	return &SigningKeyStoreFake{keys: map[string]*signingKeyRecord{}}
}

// AddSigningKey seeds the fake directly with key (as-is, including its
// Active flag), bypassing SaveSigningKey. Used by tests that need to set
// up state without going through the port under test — e.g. a bootstrap-
// idempotence test seeds an active key this way, then confirms a second
// NewSigner call loads it rather than inserting a second row. retire_at
// starts unset (never retired); call RetireSigningKey afterward to also
// set it, if a test needs a pre-seeded grace-period key.
func (f *SigningKeyStoreFake) AddSigningKey(key *domain.SigningKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *key
	f.keys[key.KID] = &signingKeyRecord{key: &cp}
}

// Len reports how many signing keys the fake currently holds. Exposed so
// tests outside this package (e.g. internal/adapters/signing's bootstrap-
// idempotence test) can assert a second NewSigner call didn't insert a
// second row, without reaching into this package's unexported fields.
func (f *SigningKeyStoreFake) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys)
}

// SaveSigningKey inserts key, keyed by its KID, overwriting any existing
// entry with the same KID. passphrase is accepted, per the port's
// signature, but ignored: encryption at rest is store.go/keys.go's
// concern (scrypt+AES-256-GCM), not something an in-memory fake needs to
// reproduce to be useful to callers.
func (f *SigningKeyStoreFake) SaveSigningKey(ctx context.Context, key *domain.SigningKey, passphrase string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *key
	f.keys[key.KID] = &signingKeyRecord{key: &cp}
	return nil
}

// GetActiveSigningKey returns the most recently created key with
// Active == true, or domain.ErrNotFound if none exists — mirroring
// keys.go's GetActiveSigningKey (ORDER BY created_at DESC LIMIT 1, a
// defensive tie-break since nothing enforces at most one active row).
func (f *SigningKeyStoreFake) GetActiveSigningKey(ctx context.Context, passphrase string) (*domain.SigningKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var best *domain.SigningKey
	for _, rec := range f.keys {
		if !rec.key.Active {
			continue
		}
		if best == nil || rec.key.CreatedAt.After(best.CreatedAt) {
			best = rec.key
		}
	}
	if best == nil {
		return nil, domain.ErrNotFound
	}
	cp := *best
	return &cp, nil
}

// GetPreviousSigningKey returns the most recently created retired-but-
// still-in-grace key (Active == false AND retire_at > now, strict), or
// domain.ErrNotFound if no such key exists — mirroring keys.go's
// GetPreviousSigningKey exactly, including its boundary judgment call: a
// key's exact retire_at instant already counts as "past retire_at" and
// must be excluded, not included (strict ">", not ">=").
func (f *SigningKeyStoreFake) GetPreviousSigningKey(ctx context.Context, passphrase string) (*domain.SigningKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	var best *signingKeyRecord
	for _, rec := range f.keys {
		if rec.key.Active {
			continue
		}
		if rec.retireAt.IsZero() || !rec.retireAt.After(now) {
			continue
		}
		if best == nil || rec.key.CreatedAt.After(best.key.CreatedAt) {
			best = rec
		}
	}
	if best == nil {
		return nil, domain.ErrNotFound
	}
	cp := *best.key
	return &cp, nil
}

// RetireSigningKey marks the key identified by kid inactive and records
// retireAt, mirroring keys.go's RetireSigningKey. Returns
// domain.ErrNotFound if no signing key with the given kid exists.
func (f *SigningKeyStoreFake) RetireSigningKey(ctx context.Context, kid string, retireAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	rec, ok := f.keys[kid]
	if !ok {
		return domain.ErrNotFound
	}
	rec.key.Active = false
	rec.retireAt = retireAt
	return nil
}
