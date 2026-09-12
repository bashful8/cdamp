package app

import (
	"context"
	"fmt"

	"cdamp/internal/domain"
)

// SearchThreadsItem pairs a thread with its message count. domain.Thread
// itself has no such field, but 03-API.md's `GET /threads` response shape
// needs one (`{id, subject, created_at, message_count}`).
type SearchThreadsItem struct {
	Thread       *domain.Thread
	MessageCount int
}

// SearchThreadsResult is the result of SearchThreads, matching
// 03-API.md's `GET /threads` response shape (`{threads: [...],
// next_cursor}`).
type SearchThreadsResult struct {
	Threads    []SearchThreadsItem
	NextCursor *string // nil means there is no next page
}

// SearchThreads wraps InboxStore.SearchThreads, adding the two things the
// port doesn't provide: a per-thread message_count and next_cursor.
//
// message_count is computed with one extra store.GetThread call per
// result and len(messages) as the count — an N+1 query pattern, but
// store.go isn't in scope for this task and CDAMP's own non-goals reject
// premature optimization at this scale, so this is acceptable here rather
// than adding a dedicated count query to the store.
//
// next_cursor mirrors ThreadFilter's own default-50/cap-200 rule the same
// way list_messages.go does for MessageFilter: if the page returned is
// exactly that size, next_cursor is the last thread's ID; otherwise nil.
func SearchThreads(ctx context.Context, store domain.InboxStore, agentID int64, query string, f domain.ThreadFilter) (*SearchThreadsResult, error) {
	threads, err := store.SearchThreads(ctx, agentID, query, f)
	if err != nil {
		return nil, fmt.Errorf("searching threads: %w", err)
	}

	items := make([]SearchThreadsItem, 0, len(threads))
	for _, t := range threads {
		_, messages, err := store.GetThread(ctx, t.ID)
		if err != nil {
			return nil, fmt.Errorf("getting thread %s for message count: %w", t.ID, err)
		}
		items = append(items, SearchThreadsItem{Thread: t, MessageCount: len(messages)})
	}

	effectiveLimit := f.Limit
	if effectiveLimit <= 0 {
		effectiveLimit = 50
	}
	if effectiveLimit > 200 {
		effectiveLimit = 200
	}

	res := &SearchThreadsResult{Threads: items}
	if len(threads) == effectiveLimit {
		cursor := threads[len(threads)-1].ID
		res.NextCursor = &cursor
	}
	return res, nil
}
