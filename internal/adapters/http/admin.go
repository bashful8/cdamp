package http

import (
	"net/http"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/config"
	"cdamp/internal/domain"
)

// adminCookieName is the name of the cookie whose value is the admin
// bootstrap credential itself (Gap 2 decision, STATUS.md 2026-09-13: "the
// cookie value *is* the bootstrap credential"). Not pinned by any doc — a
// builder judgment call in the same low-risk category as tokenBytes/
// shutdownTimeout.
const adminCookieName = "cdampd_admin"

// adminAuthMiddleware implements the Gap 2 decision's session mechanism:
// hash-and-compare the Cookie header's value against the stored
// AdminCredential.TokenHash, exactly like bearerAuthMiddleware's bearer-
// token check but read from a Cookie instead of an Authorization header,
// and checked against domain.AdminStore instead of domain.InboxStore. A
// missing cookie, no credential yet bootstrapped (domain.ErrNotFound —
// should never actually happen once main.go's BootstrapAdminCredential
// call has run, but handled the same way as any other GetAdminCredential
// failure rather than assumed away), and a value that doesn't match all
// map to the same 401 {"error":{"code":"unauthorized",...}} shape,
// mirroring bearerAuthMiddleware's own "don't let a caller distinguish
// which case" reasoning.
func adminAuthMiddleware(store domain.AdminStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(adminCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing admin session cookie")
			return
		}

		cred, err := store.GetAdminCredential(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid admin session")
			return
		}

		if hashBearerToken(cookie.Value) != cred.TokenHash {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid admin session")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// NewAdminMux returns the HTTP handler for CDAMP's admin API
// (03-API.md's "Admin API" section): POST/GET /admin/agents only — the
// blocklist routes are a separate, still-blocked task (Phase 6 task 4,
// see STATUS.md). Every route requires the admin session cookie
// (adminAuthMiddleware) and the request body is capped at
// app.MaxBodyBytes, same as every other mux in this package.
//
// Wiring this into cmd/cdampd/main.go as its own, separately-bound
// http.Server (not merged into newMux's cfg.ListenAddr mux) is this
// task's job too — see cmd/cdampd/main.go's "admin http server" wiring
// for why a second listener, not a second route group on the existing
// one, is required.
func NewAdminMux(inbox domain.InboxStore, admin domain.AdminStore, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/agents", handleCreateAgentAdmin(inbox, cfg))
	mux.HandleFunc("GET /admin/agents", handleListAgents(inbox, cfg))

	return sizeLimitMiddleware(adminAuthMiddleware(admin, mux))
}

// createAgentAdminRequest is the JSON body shape for POST /admin/agents,
// per 03-API.md: {name}.
type createAgentAdminRequest struct {
	Name string `json:"name"`
}

// handleCreateAgentAdmin implements POST /admin/agents: body {name} ->
// 201 {address, token} (token shown once, never retrievable again), per
// 03-API.md. app.CreateAgent itself does no name validation beyond what
// store.CreateAgent's UNIQUE constraint enforces — 03-API.md documents no
// validation error shape for this endpoint, so none is invented here.
func handleCreateAgentAdmin(inbox domain.InboxStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createAgentAdminRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			return // decodeJSONBody already wrote the error response
		}

		result, err := app.CreateAgent(r.Context(), inbox, cfg.Domain, app.CreateAgentRequest{Name: req.Name})
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"address": result.Address,
			"token":   result.Token,
		})
	}
}

// adminAgentListItem is the JSON shape of one entry in GET
// /admin/agents' "agents" array, per 03-API.md: {address, created_at}.
type adminAgentListItem struct {
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
}

// handleListAgents implements GET /admin/agents: -> 200 {agents:
// [{address, created_at}]}, per 03-API.md. An empty instance returns
// {"agents": []}, not {"agents": null} — same make([]T, 0, ...) discipline
// handleListMessages/handleSearchThreads already use for their own array
// fields.
func handleListAgents(inbox domain.InboxStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agents, err := inbox.ListAgents(r.Context())
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		items := make([]adminAgentListItem, 0, len(agents))
		for _, a := range agents {
			items = append(items, adminAgentListItem{
				Address:   a.Name + "@" + cfg.Domain,
				CreatedAt: a.CreatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"agents": items,
		})
	}
}
