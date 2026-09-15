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

// previousEntry is what DirectoryFake.ResolvePrevious returns for one
// address's sender domain's previous (grace-period) key.
type previousEntry struct {
	pubkey ed25519.PublicKey
	kid    string
}

// DirectoryFake is an in-memory domain.Directory. Entries are registered
// with Add; Resolve on an address with no registered entry returns
// domain.ErrNotFound. AddPrevious registers a grace-period previous-key
// entry the same way, for ResolvePrevious.
type DirectoryFake struct {
	mu       sync.Mutex
	entries  map[string]directoryEntry
	previous map[string]previousEntry
}

// NewDirectoryFake returns an empty DirectoryFake ready to use.
func NewDirectoryFake() *DirectoryFake {
	return &DirectoryFake{
		entries:  map[string]directoryEntry{},
		previous: map[string]previousEntry{},
	}
}

// Add registers the resolution for address, so a later Resolve(ctx,
// address) returns it.
func (f *DirectoryFake) Add(address string, pubkey ed25519.PublicKey, kid, inboxURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[address] = directoryEntry{pubkey: pubkey, kid: kid, inboxURL: inboxURL}
}

// AddPrevious registers address's sender domain's previous (grace-period)
// key, so a later ResolvePrevious(ctx, address) returns it. Keyed by the
// same full address Add uses, purely for this fake's own lookup
// convenience -- a test registers whichever address(es) it actually calls
// ResolvePrevious with. This differs from HTTPDirectory's own real cache
// (internal/adapters/directory/directory.go's previousCacheEntry, keyed by
// bare domain name, per STATUS.md's Phase 8 task 5 spec design decision
// 4) because this fake has no HTTP/caching concept at all -- there is no
// shared endpoint response to key by domain for, just a lookup table a
// test populates directly.
func (f *DirectoryFake) AddPrevious(address string, pubkey ed25519.PublicKey, kid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previous[address] = previousEntry{pubkey: pubkey, kid: kid}
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

// ResolvePrevious implements domain.Directory.
func (f *DirectoryFake) ResolvePrevious(ctx context.Context, address string) (ed25519.PublicKey, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.previous[address]
	if !ok {
		return nil, "", fmt.Errorf("resolve previous %q: %w", address, domain.ErrNotFound)
	}
	return e.pubkey, e.kid, nil
}
