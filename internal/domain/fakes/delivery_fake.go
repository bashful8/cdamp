package fakes

import (
	"context"
	"fmt"
	"sync"

	"cdamp/internal/domain"
)

// delivered records one DeliveryFake.Deliver call.
type delivered struct {
	message  *domain.Message
	inboxURL string
}

// DeliveryFake is an in-memory domain.Delivery. By default every Deliver
// call succeeds and is recorded; SetFail makes delivery to a given
// inboxURL fail instead, so retry-scheduling logic can be exercised.
type DeliveryFake struct {
	mu       sync.Mutex
	sent     []delivered
	failURLs map[string]error
}

// NewDeliveryFake returns an empty DeliveryFake ready to use.
func NewDeliveryFake() *DeliveryFake {
	return &DeliveryFake{failURLs: map[string]error{}}
}

// SetFail makes every subsequent Deliver call targeting inboxURL return
// err instead of succeeding.
func (f *DeliveryFake) SetFail(inboxURL string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failURLs[inboxURL] = err
}

// Deliver implements domain.Delivery.
func (f *DeliveryFake) Deliver(ctx context.Context, m *domain.Message, inboxURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failURLs[inboxURL]; ok {
		return fmt.Errorf("deliver to %q: %w", inboxURL, err)
	}
	f.sent = append(f.sent, delivered{message: m, inboxURL: inboxURL})
	return nil
}

// Sent returns every message successfully delivered so far, in call
// order.
func (f *DeliveryFake) Sent() []*domain.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Message, len(f.sent))
	for i, d := range f.sent {
		out[i] = d.message
	}
	return out
}
