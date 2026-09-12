package app

import (
	"context"
	"fmt"

	"cdamp/internal/domain"
)

// GetThreadResult carries a thread and its ordered messages, matching
// 03-API.md's `GET /threads/{id}` response shape (`{thread: {...},
// messages: [...ordered by sent_at...]}`).
type GetThreadResult struct {
	Thread   *domain.Thread
	Messages []*domain.Message
}

// GetThread is a thin pass-through to InboxStore.GetThread, propagating
// domain.ErrNotFound so task 2's HTTP layer can map it to 03-API.md's
// `404 not_found`.
//
// Note (non-blocking — flagged for a human to resolve, not guessed past):
// domain.InboxStore.GetThread takes no agentID parameter, and 03-API.md's
// documented GET /threads/{id} doesn't take one either, nor does it
// document any 403/ownership error — even though 02-ARCHITECTURE.md's
// auth table says an agent should "read only its own inbox." This use
// case therefore does not add agent-ownership filtering to GetThread: the
// port and the documented endpoint are both silent on how that would even
// work (reject the whole thread lookup? filter individual messages out of
// the result?), and inventing an answer here would be guessing at
// semantics the docs don't specify. Any ownership enforcement for this
// endpoint belongs in the HTTP layer, consistent with
// 02-ARCHITECTURE.md's "REST is the single enforcement point for auth"
// rule.
func GetThread(ctx context.Context, store domain.InboxStore, id string) (*GetThreadResult, error) {
	thread, messages, err := store.GetThread(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting thread %s: %w", id, err)
	}
	return &GetThreadResult{Thread: thread, Messages: messages}, nil
}
