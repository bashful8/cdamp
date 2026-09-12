package fakes

import (
	"context"
	"errors"
	"testing"

	"cdamp/internal/domain"
)

func TestDeliveryFakeDeliverAndSent(t *testing.T) {
	tests := []struct {
		name      string
		failURL   string
		failErr   error
		inboxURL  string
		wantErr   bool
		wantSents int
	}{
		{
			name:      "succeeds and records",
			inboxURL:  "https://example.com/deliver",
			wantSents: 1,
		},
		{
			name:      "configured failure returns error",
			failURL:   "https://example.com/deliver",
			failErr:   errors.New("connection refused"),
			inboxURL:  "https://example.com/deliver",
			wantErr:   true,
			wantSents: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewDeliveryFake()
			if tc.failURL != "" {
				f.SetFail(tc.failURL, tc.failErr)
			}
			m := &domain.Message{ID: "m1"}

			err := f.Deliver(context.Background(), m, tc.inboxURL)
			if tc.wantErr {
				if !errors.Is(err, tc.failErr) {
					t.Fatalf("Deliver error = %v, want wrapping %v", err, tc.failErr)
				}
			} else if err != nil {
				t.Fatalf("Deliver: %v", err)
			}

			if got := len(f.Sent()); got != tc.wantSents {
				t.Fatalf("len(Sent()) = %d, want %d", got, tc.wantSents)
			}
		})
	}
}
