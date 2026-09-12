package fakes

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"cdamp/internal/domain"
)

func TestDirectoryFakeResolve(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tests := []struct {
		name        string
		add         bool
		address     string
		lookup      string
		wantErr     bool
		wantInboxes string
	}{
		{
			name:        "resolves registered address",
			add:         true,
			address:     "alice@example.com",
			lookup:      "alice@example.com",
			wantInboxes: "https://example.com/deliver",
		},
		{
			name:    "unregistered address is not found",
			add:     false,
			lookup:  "bob@example.com",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewDirectoryFake()
			if tc.add {
				f.Add(tc.address, pub, "kid-1", tc.wantInboxes)
			}

			gotPub, gotKID, gotURL, err := f.Resolve(context.Background(), tc.lookup)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("Resolve error = %v, want domain.ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if gotKID != "kid-1" || gotURL != tc.wantInboxes || !gotPub.Equal(pub) {
				t.Fatalf("Resolve = (%v, %q, %q), want (%v, kid-1, %q)", gotPub, gotKID, gotURL, pub, tc.wantInboxes)
			}
		})
	}
}
