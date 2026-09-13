package fakes

import (
	"context"
	"sort"
	"sync"

	"cdamp/internal/domain"
)

// BlocklistStoreFake is an in-memory domain.BlocklistStore, mirroring
// AdminStoreFake's mutex-guarded in-memory struct style. Keyed by
// Domain (domain_blocklist.domain is the table's own PRIMARY KEY), the
// same way InboxStoreFake.agentsByName keys by agents.name's own
// UNIQUE column.
type BlocklistStoreFake struct {
	mu      sync.Mutex
	entries map[string]*domain.BlocklistEntry
}

// NewBlocklistStoreFake returns an empty BlocklistStoreFake ready to use.
func NewBlocklistStoreFake() *BlocklistStoreFake {
	return &BlocklistStoreFake{entries: make(map[string]*domain.BlocklistEntry)}
}

// SaveBlocklistEntry stores a copy of e, keyed by e.Domain. Returns
// domain.ErrConflict if e.Domain is already present — mirrors
// InboxStoreFake.CreateAgent's own bare-sentinel (not fmt.Errorf-wrapped)
// duplicate-key handling exactly.
func (f *BlocklistStoreFake) SaveBlocklistEntry(ctx context.Context, e *domain.BlocklistEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.entries[e.Domain]; exists {
		return domain.ErrConflict
	}
	cp := *e
	f.entries[e.Domain] = &cp
	return nil
}

// ListBlocklist returns every stored entry, sorted by Domain ascending
// — mirrors InboxStoreFake.ListAgents' own sort-for-determinism pattern
// and matches the SQLite adapter's own ORDER BY domain. Returns nil
// (not an empty non-nil slice) when no entries exist, matching
// ListAgents' own empty-map convention.
func (f *BlocklistStoreFake) ListBlocklist(ctx context.Context) ([]*domain.BlocklistEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.entries) == 0 {
		return nil, nil
	}
	out := make([]*domain.BlocklistEntry, 0, len(f.entries))
	for _, e := range f.entries {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}
