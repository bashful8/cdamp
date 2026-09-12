package domain

import "errors"

// ErrNotFound is returned by InboxStore lookups (GetMessage, GetThread,
// FindByIdempotencyKey) when nothing matches the given id/key.
var ErrNotFound = errors.New("not found")

// ErrDuplicateIdempotencyKey is returned by InboxStore.SaveMessage when
// the message's IdempotencyKey collides with an existing row's
// idx_messages_idem unique-index entry. This is only the store-layer
// contract for surfacing that DB constraint cleanly as a distinguishable
// error rather than a raw SQL error — the "dedupe -> 200 no-op" use-case
// behavior built on top of it belongs to ReceiveMessage (Phase 4), not the
// store.
var ErrDuplicateIdempotencyKey = errors.New("duplicate idempotency key")
