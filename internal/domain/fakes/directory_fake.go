package fakes

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"

	"cdamp/internal/domain"
)

// directoryEntry is what DirectoryFake.Resolve returns for one address.
type directoryEntry struct {
	pubkey   ed25519.PublicKey
	kid      string
	inboxURL string
}

// DirectoryFake is an in-memory domain.Directory. Entries are registered
// with Add; Resolve on an address with no registered entry returns
// domain.ErrNotFound.
type DirectoryFake struct {
	mu      sync.Mutex
	entries map[string]directoryEntry
}

// NewDirectoryFake returns an empty DirectoryFake ready to use.
func NewDirectoryFake() *DirectoryFake {
	return &DirectoryFake{entries: map[string]directoryEntry{}}
}

// Add registers the resolution for address, so a later Resolve(ctx,
// address) returns it.
func (f *DirectoryFake) Add(address string, pubkey ed25519.PublicKey, kid, inboxURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[address] = directoryEntry{pubkey: pubkey, kid: kid, inboxURL: inboxURL}
}

// Resolve implements domain.Directory.
func (f *DirectoryFake) Resolve(ctx context.Context, address string) (ed25519.PublicKey, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[address]
	if !ok {
		return nil, "", "", fmt.Errorf("resolve %q: %w", address, domain.ErrNotFound)
	}
	return e.pubkey, e.kid, e.inboxURL, nil
}
