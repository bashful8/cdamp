package app

import (
	"context"
	"fmt"

	"cdamp/internal/domain"
)

// ListMessagesResult is the result of ListMessages, matching 03-API.md's
// `GET /messages` response shape (`{messages: [...], next_cursor:
// "msg_..." | null}`) — JSON field-name mapping is the HTTP adapter's job
// (Phase 3 task 2), not this use case's.
type ListMessagesResult struct {
	Messages   []*domain.Message
	NextCursor *string // nil means there is no next page
}

// ListMessages is a thin wrapper over InboxStore.ListMessages. The SQLite
// adapter (already verified, Phase 2) already enforces the default-50/
// cap-200 rule on f.Limit and cursor semantics on f.After internally (see
// store.go's ListMessages doc comment), so this use case doesn't
// re-implement filtering — its only added value is computing next_cursor,
// since the port itself returns a plain slice with no cursor. It mirrors
// the store's own default/cap rule only to decide whether the returned
// page might be full: if the page is exactly that size, next_cursor is
// the last result's ID; otherwise nil.
func ListMessages(ctx context.Context, store domain.InboxStore, f domain.MessageFilter) (*ListMessagesResult, error) {
	results, err := store.ListMessages(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}

	effectiveLimit := f.Limit
	if effectiveLimit <= 0 {
		effectiveLimit = 50
	}
	if effectiveLimit > 200 {
		effectiveLimit = 200
	}

	res := &ListMessagesResult{Messages: results}
	if len(results) == effectiveLimit {
		cursor := results[len(results)-1].ID
		res.NextCursor = &cursor
	}
	return res, nil
}
