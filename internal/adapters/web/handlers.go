package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"

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
	Address string
	Unread  int
}

func handleDashboardHome(store domain.InboxStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			rows = append(rows, agentRow{Address: a.Name + "@" + cfg.Domain, Unread: unread})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pages.ExecuteTemplate(w, "agents.html", struct{ Agents []agentRow }{Agents: rows}); err != nil {
			http.Error(w, "failed to render", http.StatusInternalServerError)
		}
	}
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
