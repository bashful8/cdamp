package fakes

import "crypto/ed25519"

// VerifierFake is a domain.Verifier that does real Ed25519 verification
// (there's no useful "fake" signature check — this simply exposes the
// stdlib primitive behind the port so use-case tests exercise real
// pass/fail behavior).
type VerifierFake struct{}

// NewVerifierFake returns a VerifierFake.
func NewVerifierFake() *VerifierFake {
	return &VerifierFake{}
}

// Verify implements domain.Verifier.
func (VerifierFake) Verify(canonical, sig []byte, pubkey ed25519.PublicKey) bool {
	if len(pubkey) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pubkey, canonical, sig)
}
