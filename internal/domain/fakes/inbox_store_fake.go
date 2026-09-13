// Package fakes provides hand-written, in-memory fakes for CDAMP's
// domain ports (InboxStore, Directory, Signer, Verifier, Delivery), per
// the go-hexagonal-style skill's "Ports and fakes" convention. No mocking
// library — each fake has real, if simplified, behavior so later phases'
// use-case tests can rely on it.
package fakes

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"cdamp/internal/domain"
)

// InboxStoreFake is an in-memory domain.InboxStore.
type InboxStoreFake struct {
	mu       sync.Mutex
	messages map[string]*domain.Message
	threads  map[string]*domain.Thread
	// claimed tracks message IDs that have already been handed out by
	// ClaimPending and not yet marked delivered/failed, so a second
	// ClaimPending call never double-claims a row still in flight.
	claimed map[string]bool
	// agents holds seeded agents, keyed by ID, for GetAgentByID/
	// FindAgentByTokenHash — see AddAgent's doc comment. Added per
	// STATUS.md's "Agent-lookup decision (human-resolved, 2026-09-12)".
	agents map[int64]*domain.Agent
	// agentsByName is a secondary index over agents, keyed by Agent.Name,
	// for FindAgentByName — added per STATUS.md's "Agent-by-name lookup
	// decision (human-resolved, 2026-09-12)". Kept in sync with agents by
	// AddAgent.
	agentsByName map[string]*domain.Agent
}

// NewInboxStoreFake returns an empty InboxStoreFake ready to use.
func NewInboxStoreFake() *InboxStoreFake {
	return &InboxStoreFake{
		messages:     map[string]*domain.Message{},
		threads:      map[string]*domain.Thread{},
		claimed:      map[string]bool{},
		agents:       map[int64]*domain.Agent{},
		agentsByName: map[string]*domain.Agent{},
	}
}

// AddAgent seeds the fake with an agent, keyed by its ID, so tests can set
// up GetAgentByID/FindAgentByTokenHash lookups the same way other tests
// seed messages/threads via SaveMessage — the go-hexagonal-style skill's
// "hand-written fake" pattern, applied to the agents Task 2 additively
// requires. Overwrites any existing agent with the same ID.
func (f *InboxStoreFake) AddAgent(a *domain.Agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents[a.ID] = a
	f.agentsByName[a.Name] = a
}

// SaveMessage stores m (and, if new, an implicit thread record keyed by
// m.ThreadID) keyed by its ID, overwriting any existing message with the
// same ID.
func (f *InboxStoreFake) SaveMessage(ctx context.Context, m *domain.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[m.ID] = m
	if m.ThreadID != "" {
		if _, ok := f.threads[m.ThreadID]; !ok {
			f.threads[m.ThreadID] = &domain.Thread{ID: m.ThreadID, Subject: m.Subject, CreatedAt: m.SentAt}
		}
	}
	return nil
}

// GetMessage returns the message with the given id, or domain.ErrNotFound
// if none exists.
func (f *InboxStoreFake) GetMessage(ctx context.Context, id string) (*domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return m, nil
}

// ListMessages returns messages matching f, ordered by ID for determinism.
func (f *InboxStoreFake) ListMessages(ctx context.Context, filter domain.MessageFilter) ([]*domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*domain.Message
	for _, m := range f.messages {
		if filter.AgentID != 0 && m.AgentID != filter.AgentID {
			continue
		}
		if filter.From != "" && m.From != filter.From {
			continue
		}
		if filter.Thread != "" && m.ThreadID != filter.Thread {
			continue
		}
		if filter.Status != "" && m.Status != filter.Status {
			continue
		}
		if filter.Unread && m.Read {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// GetThread returns the thread with the given id and every message filed
// under it (ordered by ID), or domain.ErrNotFound if the thread doesn't
// exist.
func (f *InboxStoreFake) GetThread(ctx context.Context, id string) (*domain.Thread, []*domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.threads[id]
	if !ok {
		return nil, nil, domain.ErrNotFound
	}
	var msgs []*domain.Message
	for _, m := range f.messages {
		if m.ThreadID == id {
			msgs = append(msgs, m)
		}
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	return t, msgs, nil
}

// SearchThreads returns threads containing at least one message for
// agentID whose subject or body contains query (a simple substring match
// standing in for FTS5, which belongs to the SQLite adapter), ordered by
// ID for determinism.
func (f *InboxStoreFake) SearchThreads(ctx context.Context, agentID int64, query string, filter domain.ThreadFilter) ([]*domain.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	matches := map[string]bool{}
	for _, m := range f.messages {
		if agentID != 0 && m.AgentID != agentID {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(m.Subject), strings.ToLower(query)) &&
			!strings.Contains(strings.ToLower(m.Body), strings.ToLower(query)) {
			continue
		}
		matches[m.ThreadID] = true
	}
	var out []*domain.Thread
	for id := range matches {
		if t, ok := f.threads[id]; ok {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// ClaimPending atomically selects up to limit pending, due messages
// (status=pending, next_attempt<=now) not already claimed, marks them
// claimed, and returns them ordered by ID for determinism. A message
// stays claimed until MarkDelivered or MarkFailed is called for it, so a
// second ClaimPending call never returns the same message twice while an
// attempt is in flight.
func (f *InboxStoreFake) ClaimPending(ctx context.Context, limit int) ([]*domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	var candidates []*domain.Message
	for _, m := range f.messages {
		if m.Status != "pending" {
			continue
		}
		if f.claimed[m.ID] {
			continue
		}
		if m.NextAttempt != nil && m.NextAttempt.After(now) {
			continue
		}
		candidates = append(candidates, m)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	for _, m := range candidates {
		f.claimed[m.ID] = true
	}
	return candidates, nil
}

// MarkDelivered sets the message's status to "delivered" and clears its
// claim.
func (f *InboxStoreFake) MarkDelivered(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[id]
	if !ok {
		return domain.ErrNotFound
	}
	m.Status = "delivered"
	delete(f.claimed, id)
	return nil
}

// MarkFailed sets the message's status to "failed", records nextAttempt
// for a future retry, increments Attempts, and clears its claim so a
// later ClaimPending can pick it up again once due.
func (f *InboxStoreFake) MarkFailed(ctx context.Context, id string, nextAttempt *time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[id]
	if !ok {
		return domain.ErrNotFound
	}
	m.Status = "failed"
	m.NextAttempt = nextAttempt
	m.Attempts++
	delete(f.claimed, id)
	return nil
}

// FindByIdempotencyKey returns the message with the given idempotency
// key, or domain.ErrNotFound if none exists.
func (f *InboxStoreFake) FindByIdempotencyKey(ctx context.Context, key string) (*domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key == "" {
		return nil, domain.ErrNotFound
	}
	for _, m := range f.messages {
		if m.IdempotencyKey == key {
			return m, nil
		}
	}
	return nil, domain.ErrNotFound
}

// GetAgentByID returns the seeded agent with the given id, or
// domain.ErrNotFound if none exists. See AddAgent.
func (f *InboxStoreFake) GetAgentByID(ctx context.Context, id int64) (*domain.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return a, nil
}

// FindAgentByTokenHash returns the seeded agent whose TokenHash exactly
// matches tokenHash, or domain.ErrNotFound if none matches. See AddAgent.
func (f *InboxStoreFake) FindAgentByTokenHash(ctx context.Context, tokenHash string) (*domain.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tokenHash == "" {
		return nil, domain.ErrNotFound
	}
	for _, a := range f.agents {
		if a.TokenHash == tokenHash {
			return a, nil
		}
	}
	return nil, domain.ErrNotFound
}

// FindAgentByName returns the seeded agent whose Name exactly matches name,
// or domain.ErrNotFound if none matches. See AddAgent. Added per
// STATUS.md's "Agent-by-name lookup decision (human-resolved,
// 2026-09-12)".
func (f *InboxStoreFake) FindAgentByName(ctx context.Context, name string) (*domain.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agentsByName[name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return a, nil
}
