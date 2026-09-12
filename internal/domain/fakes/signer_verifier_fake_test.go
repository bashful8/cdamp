package fakes

import (
	"errors"
	"testing"
)

func TestSignerFakeSignIsVerifiable(t *testing.T) {
	signer := NewSignerFake("kid-1")
	verifier := NewVerifierFake()
	msg := []byte("canonical message bytes")

	sig, kid, err := signer.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if kid != "kid-1" {
		t.Fatalf("kid = %q, want kid-1", kid)
	}
	if !verifier.Verify(msg, sig, signer.PublicKey()) {
		t.Fatalf("Verify(genuine signature) = false, want true")
	}
	if verifier.Verify([]byte("tampered"), sig, signer.PublicKey()) {
		t.Fatalf("Verify(tampered message) = true, want false")
	}
}

func TestSignerFakeSignError(t *testing.T) {
	signer := NewSignerFake("kid-1")
	wantErr := errors.New("boom")
	signer.SetSignError(wantErr)

	_, _, err := signer.Sign([]byte("x"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Sign error = %v, want %v", err, wantErr)
	}
}

func TestVerifierFakeRejectsWrongKeySize(t *testing.T) {
	verifier := NewVerifierFake()
	if verifier.Verify([]byte("x"), []byte("sig"), []byte("too-short")) {
		t.Fatalf("Verify with malformed pubkey = true, want false")
	}
}
