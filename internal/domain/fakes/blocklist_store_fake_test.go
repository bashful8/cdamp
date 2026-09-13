package fakes

import (
	"context"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
)

func TestBlocklistStoreFakeSaveAndListRoundTrip(t *testing.T) {
	f := NewBlocklistStoreFake()
	ctx := context.Background()

	entry := &domain.BlocklistEntry{
		Domain:  "spam.example",
		Reason:  "spam source",
		AddedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := f.SaveBlocklistEntry(ctx, entry); err != nil {
		t.Fatalf("SaveBlocklistEntry: %v", err)
	}

	entries, err := f.ListBlocklist(ctx)
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

func TestBlocklistStoreFakeSaveDuplicateDomainConflict(t *testing.T) {
	f := NewBlocklistStoreFake()
	ctx := context.Background()

	first := &domain.BlocklistEntry{Domain: "spam.example", Reason: "first", AddedAt: time.Now().UTC()}
	if err := f.SaveBlocklistEntry(ctx, first); err != nil {
		t.Fatalf("SaveBlocklistEntry (first): %v", err)
	}

	second := &domain.BlocklistEntry{Domain: "spam.example", Reason: "second", AddedAt: time.Now().UTC()}
	err := f.SaveBlocklistEntry(ctx, second)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("SaveBlocklistEntry (second, duplicate domain): got %v, want domain.ErrConflict", err)
	}
}

func TestBlocklistStoreFakeSaveEmptyReason(t *testing.T) {
	f := NewBlocklistStoreFake()
	ctx := context.Background()

	entry := &domain.BlocklistEntry{Domain: "noreason.example", Reason: "", AddedAt: time.Now().UTC()}
	if err := f.SaveBlocklistEntry(ctx, entry); err != nil {
		t.Fatalf("SaveBlocklistEntry: %v", err)
	}

	entries, err := f.ListBlocklist(ctx)
	if err != nil {
		t.Fatalf("ListBlocklist: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Reason != "" {
		t.Errorf("Reason = %q, want empty string", entries[0].Reason)
	}
}

func TestBlocklistStoreFakeListOrderedByDomain(t *testing.T) {
	f := NewBlocklistStoreFake()
	ctx := context.Background()
	addedAt := time.Now().UTC()

	for _, d := range []string{"c.example", "a.example", "b.example"} {
		if err := f.SaveBlocklistEntry(ctx, &domain.BlocklistEntry{
			Domain: d, Reason: "reason for " + d, AddedAt: addedAt,
		}); err != nil {
			t.Fatalf("SaveBlocklistEntry(%q): %v", d, err)
		}
	}

	entries, err := f.ListBlocklist(ctx)
	if err != nil {
		t.Fatalf("ListBlocklist: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	wantOrder := []string{"a.example", "b.example", "c.example"}
	for i, want := range wantOrder {
		if entries[i].Domain != want {
			t.Errorf("entries[%d].Domain = %q, want %q", i, entries[i].Domain, want)
		}
	}
}

func TestBlocklistStoreFakeListEmpty(t *testing.T) {
	f := NewBlocklistStoreFake()

	entries, err := f.ListBlocklist(context.Background())
	if err != nil {
		t.Fatalf("ListBlocklist on empty fake: %v", err)
	}
	if entries != nil {
		t.Fatalf("entries = %+v, want nil", entries)
	}
}
