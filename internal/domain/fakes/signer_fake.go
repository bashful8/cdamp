package fakes

import (
	"crypto/ed25519"
	"sync"
)

// SignerFake is an in-memory domain.Signer backed by a real Ed25519
// keypair generated at construction, so signatures it produces are
// genuinely verifiable (e.g. against VerifierFake or the real
// crypto/ed25519 verification the SQLite/HTTP phases will use).
type SignerFake struct {
	mu      sync.Mutex
	kid     string
	priv    ed25519.PrivateKey
	pub     ed25519.PublicKey
	signErr error
}

// NewSignerFake generates a fresh Ed25519 keypair identified by kid and
// returns a SignerFake that signs with it.
func NewSignerFake(kid string) *SignerFake {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		// crypto/ed25519.GenerateKey with a nil reader only fails if
		// crypto/rand.Reader itself fails, which is unrecoverable in
		// practice for a test fake.
		panic(err)
	}
	return &SignerFake{kid: kid, priv: priv, pub: pub}
}

// PublicKey returns the fake's public key, e.g. to register with a
// DirectoryFake or feed to a VerifierFake.
func (f *SignerFake) PublicKey() ed25519.PublicKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pub
}

// SetSignError makes every subsequent Sign call return err instead of
// signing, so callers can exercise their error handling.
func (f *SignerFake) SetSignError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signErr = err
}

// Sign implements domain.Signer.
func (f *SignerFake) Sign(canonical []byte) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signErr != nil {
		return nil, "", f.signErr
	}
	return ed25519.Sign(f.priv, canonical), f.kid, nil
}
