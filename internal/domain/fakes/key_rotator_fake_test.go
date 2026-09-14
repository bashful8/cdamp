package fakes

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestKeyRotatorFakeRotateReturnsCannedResult(t *testing.T) {
	f := NewKeyRotatorFake()
	retireAt := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	f.SetRotateResult("k2", "k1", retireAt)

	kid, retiredKID, gotRetireAt, err := f.Rotate(context.Background())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if kid != "k2" {
		t.Errorf("kid = %q, want %q", kid, "k2")
	}
	if retiredKID != "k1" {
		t.Errorf("retiredKID = %q, want %q", retiredKID, "k1")
	}
	if !gotRetireAt.Equal(retireAt) {
		t.Errorf("retireAt = %v, want %v", gotRetireAt, retireAt)
	}
}

func TestKeyRotatorFakeSetRotateErrorOverridesResult(t *testing.T) {
	f := NewKeyRotatorFake()
	f.SetRotateResult("k2", "k1", time.Now().UTC())

	sentinel := errors.New("boom")
	f.SetRotateError(sentinel)

	_, _, _, err := f.Rotate(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Rotate after SetRotateError: got %v, want %v", err, sentinel)
	}
}

func TestKeyRotatorFakeCallsCountsInvocations(t *testing.T) {
	f := NewKeyRotatorFake()
	if got := f.Calls(); got != 0 {
		t.Fatalf("Calls() before any Rotate call = %d, want 0", got)
	}

	f.SetRotateResult("k2", "k1", time.Now().UTC())
	if _, _, _, err := f.Rotate(context.Background()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if _, _, _, err := f.Rotate(context.Background()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if got := f.Calls(); got != 2 {
		t.Fatalf("Calls() after two Rotate calls = %d, want 2", got)
	}
}
