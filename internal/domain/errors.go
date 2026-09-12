package domain

import "errors"

// ErrNotFound is returned by InboxStore lookups (GetMessage, GetThread,
// FindByIdempotencyKey) when nothing matches the given id/key.
var ErrNotFound = errors.New("not found")
