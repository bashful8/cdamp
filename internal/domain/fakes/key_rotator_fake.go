package fakes

import (
	"context"
	"sync"
	"time"
)

// KeyRotatorFake is an in-memory domain.KeyRotator, mirroring
// SignerFake's canned-result/error-injection style. Rotate returns a
// settable canned (kid, retiredKID, retireAt) result unless
// SetRotateError has been called, in which case it returns that error
// instead. Calls() reports how many times Rotate has been invoked, so
// HTTP-layer tests can assert a handler actually delegated to the port
// exactly once.
type KeyRotatorFake struct {
	mu         sync.Mutex
	kid        string
	retiredKID string
	retireAt   time.Time
	rotateErr  error
	calls      int
}

// NewKeyRotatorFake returns a KeyRotatorFake with an empty canned result
// (Rotate returns zero values until SetRotateResult is called).
func NewKeyRotatorFake() *KeyRotatorFake {
	return &KeyRotatorFake{}
}

// SetRotateResult sets the (kid, retiredKID, retireAt) tuple every
// subsequent Rotate call returns, clearing any previously configured
// error.
func (f *KeyRotatorFake) SetRotateResult(kid, retiredKID string, retireAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kid = kid
	f.retiredKID = retiredKID
	f.retireAt = retireAt
	f.rotateErr = nil
}

// SetRotateError makes every subsequent Rotate call return err instead
// of the canned result, so callers can exercise their error handling.
func (f *KeyRotatorFake) SetRotateError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotateErr = err
}

// Rotate implements domain.KeyRotator.
func (f *KeyRotatorFake) Rotate(ctx context.Context) (newKID, retiredKID string, retireAt time.Time, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.rotateErr != nil {
		return "", "", time.Time{}, f.rotateErr
	}
	return f.kid, f.retiredKID, f.retireAt, nil
}

// Calls reports how many times Rotate has been called so far.
func (f *KeyRotatorFake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
