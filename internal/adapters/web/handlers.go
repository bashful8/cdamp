package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/config"
	"cdamp/internal/domain"
)

//go:embed templates
var templateFS embed.FS

var pages = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// NewDashboardHandler returns the http.Handler for CDAMP's admin
// dashboard (04-BUILD-PLAN.md's Phase 6 "Dashboard" bullet): a thin,
// server-rendered client of the same domain.InboxStore port and
// internal/app use cases the local agent API (internal/adapters/http/
// local.go) already uses — no parallel business logic, per
// 02-ARCHITECTURE.md's golden rule. Mounted at "/dashboard/" by
// internal/adapters/http.NewAdminMux, behind the same adminAuthMiddleware
// that already gates /admin/agents and /admin/blocklist (see STATUS.md's
// Phase 6 task 5 spec "Design decisions" for why this package never
// imports internal/adapters/http directly).
//
// This first pass implements only the agents+unread-counts view
// (GET /dashboard). Per-agent inbox, thread view, and search are
// follow-on tasks, not built here.
func NewDashboardHandler(store domain.InboxStore, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard/", handleDashboardHome(store, cfg))
	mux.HandleFunc("GET /dashboard/agents/{id}", handleDashboardAgentInbox(store, cfg))
	mux.HandleFunc("GET /dashboard/threads/{id}", handleDashboardThread(store))
	mux.Handle("GET /dashboard/static/", http.StripPrefix("/dashboard/static/", http.FileServerFS(mustSub(templateFS, "templates/static"))))
	return mux
}

// mustSub is a tiny fs.Sub + panic-on-error wrapper, same "must" idiom as
// template.Must above — used to serve the embedded templates/static
// subtree (the vendored htmx build) without leaking the "templates/"
// prefix into served URLs.
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// agentRow is the per-agent view-model handleDashboardHome's template
// renders: the domain.Agent's own address plus its unread count,
// computed via app.ListMessages (see spec's "Unread count" design
// decision for why there's no dedicated count port method yet).
type agentRow struct {
	ID      int64
	Address string
	Unread  int
}

// messageRow is the per-message view-model handleDashboardAgentInbox's
// template renders — a small, page-specific projection of domain.Message,
// not messageResponse (that's the JSON API's shape, internal/adapters/http
// only).
type messageRow struct {
	ThreadID string
	From     string
	Subject  string
	SentAt   time.Time
	Read     bool
}

// messageDetailRow is the per-message view-model handleDashboardThread's
// template renders — like messageRow but also carries To and Body, since
// the thread view is exactly the "full message body" surface task 6's
// spec deferred to this task.
type messageDetailRow struct {
	From, To string
	Subject  string
	Body     string
	SentAt   time.Time
	Read     bool
}

// searchResultRow is the per-thread view-model handleDashboardHome
// renders when a search query is present — a small, page-specific
// projection of app.SearchThreadsItem, not that type itself, same
// rationale messageRow/messageDetailRow's own doc comments give.
type searchResultRow struct {
	ThreadID     string
	Subject      string
	MessageCount int
	CreatedAt    time.Time
}

func handleDashboardHome(store domain.InboxStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")

		data := struct {
			Query   string
			Agents  []agentRow
			Results []searchResultRow
		}{Query: q}

		if q != "" {
			results, err := searchAllAgents(r.Context(), store, q)
			if err != nil {
				http.Error(w, "failed to search threads", http.StatusInternalServerError)
				return
			}
			data.Results = results
		} else {
			agents, err := store.ListAgents(r.Context())
			if err != nil {
				http.Error(w, "failed to list agents", http.StatusInternalServerError)
				return
			}
			rows := make([]agentRow, 0, len(agents))
			for _, a := range agents {
				unread, err := unreadCount(r.Context(), store, a.ID)
				if err != nil {
					http.Error(w, "failed to count unread messages", http.StatusInternalServerError)
					return
				}
				rows = append(rows, agentRow{ID: a.ID, Address: a.Name + "@" + cfg.Domain, Unread: unread})
			}
			data.Agents = rows
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pages.ExecuteTemplate(w, "agents.html", data); err != nil {
			http.Error(w, "failed to render", http.StatusInternalServerError)
		}
	}
}

// searchAllAgents runs app.SearchThreads once per local agent and merges
// the results, since domain.InboxStore.SearchThreads is scoped to one
// agentID and has no "every agent" mode (see this task's spec "Research"
// section) — the dashboard's own "sees all agents on the instance"
// property (02-ARCHITECTURE.md) means a real cross-agent search, not a
// single scoped call. Deliberately never calls SearchThreads with
// agentID=0 as an "all agents" sentinel: InboxStoreFake happens to treat
// 0 as "no filter" (test-fixture convenience only, undocumented on the
// InboxStore interface), but the real SQLite Store filters
// unconditionally on m.agent_id = ? and real agent IDs start at 1, so
// agentID=0 always returns zero rows there — relying on the fake's
// behavior would pass in tests and silently return nothing in
// production. Results are deduped by thread ID (a thread can carry
// messages from more than one local agent) and capped at 50 total with
// no next_cursor — see "Design decisions" #4 for why that's a documented
// scope cut, not an oversight.
func searchAllAgents(ctx context.Context, store domain.InboxStore, q string) ([]searchResultRow, error) {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var rows []searchResultRow
	for _, a := range agents {
		result, err := app.SearchThreads(ctx, store, a.ID, q, domain.ThreadFilter{Limit: 50})
		if err != nil {
			return nil, err
		}
		for _, item := range result.Threads {
			if seen[item.Thread.ID] {
				continue
			}
			seen[item.Thread.ID] = true
			rows = append(rows, searchResultRow{
				ThreadID:     item.Thread.ID,
				Subject:      item.Thread.Subject,
				MessageCount: item.MessageCount,
				CreatedAt:    item.Thread.CreatedAt,
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	if len(rows) > 50 {
		rows = rows[:50]
	}
	return rows, nil
}

// unreadCount returns agentID's unread-message count via the same
// ListMessages use case local.go already uses, capped at 200 per
// ListMessages' own default/cap rule — see this task's "Unread count"
// design decision for the known undercount-above-200 limitation.
func unreadCount(ctx context.Context, store domain.InboxStore, agentID int64) (int, error) {
	result, err := app.ListMessages(ctx, store, domain.MessageFilter{AgentID: agentID, Unread: true, Limit: 200})
	if err != nil {
		return 0, err
	}
	return len(result.Messages), nil
}

// handleDashboardAgentInbox implements GET /dashboard/agents/{id}: one
// agent's message list (first page only, no unread filter — see this
// task's "Design decisions" for the scope cut). A malformed id or a
// domain.ErrNotFound from GetAgentByID both render 404; any other error
// is a genuine failure, rendered 500 — mirrors handleDashboardHome's own
// error-to-status mapping exactly.
func handleDashboardAgentInbox(store domain.InboxStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}

		agent, err := store.GetAgentByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				http.Error(w, "agent not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to look up agent", http.StatusInternalServerError)
			return
		}

		result, err := app.ListMessages(r.Context(), store, domain.MessageFilter{AgentID: id, Limit: 50})
		if err != nil {
			http.Error(w, "failed to list messages", http.StatusInternalServerError)
			return
		}

		rows := make([]messageRow, 0, len(result.Messages))
		for _, m := range result.Messages {
			rows = append(rows, messageRow{ThreadID: m.ThreadID, From: m.From, Subject: m.Subject, SentAt: m.SentAt, Read: m.Read})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := struct {
			Address  string
			Messages []messageRow
		}{Address: agent.Name + "@" + cfg.Domain, Messages: rows}
		if err := pages.ExecuteTemplate(w, "agent_inbox.html", data); err != nil {
			http.Error(w, "failed to render", http.StatusInternalServerError)
		}
	}
}

// handleDashboardThread implements GET /dashboard/threads/{id}: one
// thread's subject plus every message filed under it, bodies included
// (03-API.md: "full ordered conversation, all message bodies, one call,
// no pagination needed"). domain.ErrNotFound (unknown thread id) renders
// 404; any other error is a genuine failure, rendered 500 — mirrors
// handleDashboardAgentInbox's own error-to-status mapping. No ownership
// check: see this task's "Design decisions" #2 for why that's correct
// here, unlike internal/adapters/http/local.go's agent-facing GET
// /threads/{id}.
func handleDashboardThread(store domain.InboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		result, err := app.GetThread(r.Context(), store, id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				http.Error(w, "thread not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to look up thread", http.StatusInternalServerError)
			return
		}

		rows := make([]messageDetailRow, 0, len(result.Messages))
		for _, m := range result.Messages {
			rows = append(rows, messageDetailRow{From: m.From, To: m.To, Subject: m.Subject, Body: m.Body, SentAt: m.SentAt, Read: m.Read})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := struct {
			Subject  string
			Messages []messageDetailRow
		}{Subject: result.Thread.Subject, Messages: rows}
		if err := pages.ExecuteTemplate(w, "thread.html", data); err != nil {
			http.Error(w, "failed to render", http.StatusInternalServerError)
		}
	}
}
