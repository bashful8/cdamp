package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func TestSaveBlocklistEntry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entry := &domain.BlocklistEntry{
		Domain:  "spam.example",
		Reason:  "spam source",
		AddedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.SaveBlocklistEntry(ctx, entry); err != nil {
		t.Fatalf("SaveBlocklistEntry: %v", err)
	}

	entries, err := s.ListBlocklist(ctx)
	if err != nil {
		t.Fatalf("ListBlocklist: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.Domain != entry.Domain {
		t.Errorf("Domain = %q, want %q", got.Domain, entry.Domain)
	}
	if got.Reason != entry.Reason {
		t.Errorf("Reason = %q, want %q", got.Reason, entry.Reason)
	}
	if !got.AddedAt.Equal(entry.AddedAt) {
		t.Errorf("AddedAt = %v, want %v", got.AddedAt, entry.AddedAt)
	}
}

// TestSaveBlocklistEntryDuplicateDomainConflict confirms a duplicate
// Domain (domain_blocklist.domain is a TEXT PRIMARY KEY, reported under
// the distinct SQLITE_CONSTRAINT_PRIMARYKEY code, 1555 — not the plain
// UNIQUE code 2067 CreateAgent's own conflict detection uses) resolves
// to domain.ErrConflict, exercised against the real SQLite driver, not
// a fake.
func TestSaveBlocklistEntryDuplicateDomainConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first := &domain.BlocklistEntry{
		Domain:  "spam.example",
		Reason:  "first reason",
		AddedAt: time.Now().UTC(),
	}
	if err := s.SaveBlocklistEntry(ctx, first); err != nil {
		t.Fatalf("SaveBlocklistEntry (first): %v", err)
	}

	second := &domain.BlocklistEntry{
		Domain:  "spam.example",
		Reason:  "second reason",
		AddedAt: time.Now().UTC(),
	}
	err := s.SaveBlocklistEntry(ctx, second)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("SaveBlocklistEntry (second, duplicate domain): got %v, want domain.ErrConflict", err)
	}
}

func TestSaveBlocklistEntryEmptyReason(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entry := &domain.BlocklistEntry{
		Domain:  "noreason.example",
		Reason:  "",
		AddedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.SaveBlocklistEntry(ctx, entry); err != nil {
		t.Fatalf("SaveBlocklistEntry: %v", err)
	}

	entries, err := s.ListBlocklist(ctx)
	if err != nil {
		t.Fatalf("ListBlocklist: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Reason != "" {
		t.Errorf("Reason = %q, want empty string (NULL round-trip)", entries[0].Reason)
	}
}

func TestListBlocklist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	addedAt := time.Now().UTC().Truncate(time.Second)

	// Seeded out of domain-sorted order on purpose.
	for _, d := range []string{"c.example", "a.example", "b.example"} {
		if err := s.SaveBlocklistEntry(ctx, &domain.BlocklistEntry{
			Domain: d, Reason: "reason for " + d, AddedAt: addedAt,
		}); err != nil {
			t.Fatalf("SaveBlocklistEntry(%q): %v", d, err)
		}
	}

	entries, err := s.ListBlocklist(ctx)
	if err != nil {
		t.Fatalf("ListBlocklist: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	wantOrder := []string{"a.example", "b.example", "c.example"}
	for i, want := range wantOrder {
		if entries[i].Domain != want {
			t.Errorf("entries[%d].Domain = %q, want %q (ORDER BY domain ascending)", i, entries[i].Domain, want)
		}
	}
}

func TestListBlocklistEmpty(t *testing.T) {
	s := newTestStore(t)

	entries, err := s.ListBlocklist(context.Background())
	if err != nil {
		t.Fatalf("ListBlocklist on empty table: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want empty", entries)
	}
}
