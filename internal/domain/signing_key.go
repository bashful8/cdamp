package domain

import (
	"crypto/ed25519"
	"time"
)

// SigningKey is an Ed25519 keypair used to sign outbound messages on
// behalf of the local domain.
type SigningKey struct {
	KID        string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey // only ever held by the local instance
	Active     bool               // true = current signing key, false = previous/grace-period
	CreatedAt  time.Time
}
