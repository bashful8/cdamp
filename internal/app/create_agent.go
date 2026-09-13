package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"cdamp/internal/domain"
)

// tokenBytes is the number of random bytes read from crypto/rand to build
// a freshly created agent's plaintext bearer token, before base64
// URL-encoding. Not pinned by any doc; a defensible default (256 bits of
// entropy) in the same low-risk judgment-call category as client.go's
// deliverTimeout.
const tokenBytes = 32

// CreateAgentRequest is the input to CreateAgent.
type CreateAgentRequest struct {
	// Name is the bare agent-name, before "@" — 03-API.md: POST
	// /admin/agents body {name}.
	Name string
}

// CreateAgentResult is returned once, on creation, matching 03-API.md's
// `201 {address, token}` shape. Neither field is ever persisted or
// logged by this package — Token in particular is the only place the
// plaintext bearer token ever exists outside the caller's own hands.
type CreateAgentResult struct {
	// Address is "<name>@<domain>", shown once.
	Address string
	// Token is the plaintext bearer token, shown once, never persisted
	// or logged anywhere — 04-BUILD-PLAN.md's Testing note is explicit
	// about this.
	Token string
}

// CreateAgent generates a fresh bearer token for a brand-new local agent,
// persists only its SHA-256 hash (via store.CreateAgent), and returns the
// plaintext token exactly once — it is never stored, hashed-and-compared
// aside, or logged by this function.
//
// localDomain is the resolved local domain name (a caller building
// against a real daemon passes cfg.Domain) used to build Address, mirroring
// federation.go's own "https://" + cfg.Domain + ... construction style.
// This takes a plain string rather than *config.Config: internal/app may
// only import cdamp/internal/domain per 02-ARCHITECTURE.md's dependency
// rule ("app imports only domain"), the same reason send_message.go's
// MaxBodyBytes is a package constant instead of being threaded in from
// config directly.
//
// A wrapped domain.ErrConflict (a.Name already exists) propagates up
// unwrapped-of-context to this function's own caller via errors.Is — not
// swallowed, not translated to a generic error.
func CreateAgent(ctx context.Context, store domain.InboxStore, localDomain string, req CreateAgentRequest) (*CreateAgentResult, error) {
	token, err := newBearerToken()
	if err != nil {
		return nil, fmt.Errorf("generating agent token: %w", err)
	}

	a := &domain.Agent{
		Name:      req.Name,
		TokenHash: hashBearerToken(token),
	}
	if err := store.CreateAgent(ctx, a); err != nil {
		return nil, fmt.Errorf("creating agent %q: %w", req.Name, err)
	}

	return &CreateAgentResult{
		Address: req.Name + "@" + localDomain,
		Token:   token,
	}, nil
}

// newBearerToken generates a fresh plaintext bearer token: tokenBytes
// random bytes from crypto/rand, base64 URL-encoded (no padding, so the
// result is safely usable as an "Authorization: Bearer <token>" header
// value with no further escaping).
func newBearerToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashBearerToken hashes a raw bearer token the exact same way
// internal/adapters/http/middleware.go's hashBearerToken does (plain
// SHA-256, hex-encoded): internal/app cannot import internal/adapters/http
// (adapters import app, never the reverse, per 02-ARCHITECTURE.md's
// dependency rule), so this is an intentional duplication of that
// single-line scheme, not a divergent one — it must keep producing the
// byte-identical hash for the same token, since
// InboxStore.FindAgentByTokenHash's later lookups depend on it.
func hashBearerToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
