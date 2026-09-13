package domain

import "time"

// BlocklistEntry mirrors domain_blocklist's schema
// (internal/adapters/storage/sqlite/migrations/0001_init.up.sql):
// federation-wide policy blocking inbound mail from a given sender
// domain (01-PROTOCOL.md / 03-API.md's POST /deliver -> 403
// blocklisted enforcement, Phase 8, not built by this type or its
// port). Domain is the table's own TEXT PRIMARY KEY — a duplicate
// Domain passed to BlocklistStore.SaveBlocklistEntry resolves to a
// wrapped ErrConflict, per the human-resolved Blocklist port decision
// (STATUS.md, 2026-09-13).
type BlocklistEntry struct {
	Domain  string
	Reason  string
	AddedAt time.Time
}
