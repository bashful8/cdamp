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

// ErrValidation is returned by internal/app use cases (e.g. SendMessage)
// when a request fails input validation: a missing/empty required field,
// a malformed `to` address, a body over the size cap, or an in_reply_to
// that doesn't resolve to an existing message. Wrap it with fmt.Errorf's
// %w to attach a specific reason string; callers should distinguish it
// with errors.Is (never by matching error text), and 03-API.md's HTTP
// adapter (Phase 3 task 2) maps it to the documented
// `400 {"error":{"code":"bad_request",...}}` shape.
var ErrValidation = errors.New("validation failed")

// ErrConflict is returned by InboxStore.CreateAgent when a.Name already
// exists (agents.name is UNIQUE). Distinguish it with errors.Is, same
// convention as ErrNotFound/ErrValidation; the HTTP adapter (a later
// task) maps it to 409 conflict.
var ErrConflict = errors.New("conflict")
