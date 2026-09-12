package domain

// ThreadFilter narrows InboxStore.SearchThreads results. agentID and query
// are pulled out as explicit SearchThreads parameters, so only the
// remaining `GET /threads?agent=&q=&after=&limit=` params live here.
type ThreadFilter struct {
	After string // opaque cursor = last thread id from previous page
	Limit int    // default 50, max 200 — enforced by the HTTP adapter in Phase 3
}

// MessageFilter narrows InboxStore.ListMessages results. Unlike
// SearchThreads, ListMessages pulls nothing out separately, so every
// `GET /messages?...` query param lives here.
type MessageFilter struct {
	AgentID int64 // `agent` — resolved from the address to an Agent.ID by the
	// HTTP adapter in Phase 3; ports stay HTTP-agnostic
	Query  string // `q`, optional FTS5 match
	From   string // `from`
	Thread string // `thread` — thread ID
	Status string // `status`
	Unread bool   // `unread` — presence/true means "unread only"; false is
	// the zero value for "no filter", matching how the other
	// string fields use "" for "no filter"
	After string // opaque cursor = last message id from previous page
	Limit int    // default 50, max 200 — enforced by the HTTP adapter in Phase 3
}
