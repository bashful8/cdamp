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
// (03-API.md's "Admin API" section): POST/GET /admin/agents, POST/GET
// /admin/blocklist, POST /admin/keys/rotate (Phase 8 task 3, not in
// 03-API.md -- see STATUS.md's Phase 8 task 3 spec, design decision 6),
// and the human-facing dashboard mounted at "/dashboard/". Every route
// requires the admin session cookie
// (adminAuthMiddleware) and the request body is capped at
// app.MaxBodyBytes, same as every other mux in this package.
//
// dashboard is internal/adapters/web.NewDashboardHandler's return value,
// passed in rather than constructed here: internal/adapters/web must
// never import this package (02-ARCHITECTURE.md's adapters/*-import-only-
// domain/app dependency rule), so it cannot reuse adminAuthMiddleware
// directly. Accepting it as an http.Handler parameter lets this mux
// mount it behind the same auth/size-limit wrapper as every other admin
// route without either package importing the other — see STATUS.md's
// Phase 6 task 5 spec "Design decisions" for the full reasoning.
//
// Wiring this into cmd/cdampd/main.go as its own, separately-bound
// http.Server (not merged into newMux's cfg.ListenAddr mux) is this
// task's job too — see cmd/cdampd/main.go's "admin http server" wiring
// for why a second listener, not a second route group on the existing
// one, is required.
func NewAdminMux(inbox domain.InboxStore, admin domain.AdminStore, blocklist domain.BlocklistStore, rotator domain.KeyRotator, dashboard http.Handler, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/agents", handleCreateAgentAdmin(inbox, cfg))
	mux.HandleFunc("GET /admin/agents", handleListAgents(inbox, cfg))
	mux.HandleFunc("POST /admin/blocklist", handleCreateBlocklistEntry(blocklist))
	mux.HandleFunc("GET /admin/blocklist", handleListBlocklist(blocklist))
	mux.HandleFunc("POST /admin/keys/rotate", handleRotateKey(rotator))
	mux.Handle("/dashboard/", dashboard)

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

// createBlocklistEntryRequest is the JSON body shape for POST
// /admin/blocklist, per 03-API.md: {domain, reason}.
type createBlocklistEntryRequest struct {
	Domain string `json:"domain"`
	Reason string `json:"reason"`
}

// blocklistEntryResponse is the JSON shape of one domain_blocklist
// entry: 03-API.md pins POST /admin/blocklist's 201 response to exactly
// {domain, reason, added_at}; GET /admin/blocklist's line only gives an
// elliptical {domains: [...]}, so this handler reuses the POST
// response's own shape for each list entry too (see this task's
// "Design decisions" section — the same "list mirrors create" pattern
// GET /admin/agents already established).
type blocklistEntryResponse struct {
	Domain  string    `json:"domain"`
	Reason  string    `json:"reason"`
	AddedAt time.Time `json:"added_at"`
}

// handleCreateBlocklistEntry implements POST /admin/blocklist: body
// {domain, reason} -> 201 {domain, reason, added_at}, per 03-API.md. A
// duplicate domain maps to domain.ErrConflict -> 409 conflict via
// mapDomainError's existing branch (Phase 6 task 3), per the
// human-resolved Blocklist port decision (STATUS.md, 2026-09-13) — no
// idempotent-200 special case. No validation beyond the store's own
// PRIMARY KEY constraint is invented here, matching
// handleCreateAgentAdmin's own "03-API.md documents no validation error
// shape for this endpoint" precedent.
func handleCreateBlocklistEntry(bl domain.BlocklistStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createBlocklistEntryRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			return // decodeJSONBody already wrote the error response
		}

		entry := &domain.BlocklistEntry{
			Domain:  req.Domain,
			Reason:  req.Reason,
			AddedAt: time.Now().UTC(),
		}
		if err := bl.SaveBlocklistEntry(r.Context(), entry); err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		writeJSON(w, http.StatusCreated, blocklistEntryResponse{
			Domain:  entry.Domain,
			Reason:  entry.Reason,
			AddedAt: entry.AddedAt,
		})
	}
}

// handleListBlocklist implements GET /admin/blocklist: -> 200
// {domains: [{domain, reason, added_at}]}, per 03-API.md (see
// blocklistEntryResponse's doc comment for the per-entry shape's own
// judgment call). Empty -> {"domains": []}, not {"domains": null}, same
// make([]T, 0, ...) discipline as handleListAgents.
func handleListBlocklist(bl domain.BlocklistStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := bl.ListBlocklist(r.Context())
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		items := make([]blocklistEntryResponse, 0, len(entries))
		for _, e := range entries {
			items = append(items, blocklistEntryResponse{
				Domain:  e.Domain,
				Reason:  e.Reason,
				AddedAt: e.AddedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"domains": items,
		})
	}
}

// rotateKeyResponse is the JSON shape of POST /admin/keys/rotate's 201
// response. Not in 03-API.md (a genuinely new endpoint, no existing
// shape to copy) -- see STATUS.md's Phase 8 task 3 spec, design
// decision 6, for the reasoning: kid (the newly active key),
// retired_kid (the key that just entered its grace period), and
// retire_at (when it stops verifying inbound signatures at all, per
// 01-PROTOCOL.md's 30-day window).
type rotateKeyResponse struct {
	KID        string    `json:"kid"`
	RetiredKID string    `json:"retired_kid"`
	RetireAt   time.Time `json:"retire_at"`
}

// handleRotateKey implements POST /admin/keys/rotate: no request body
// (design decision 7) -> 201 {kid, retired_kid, retire_at}.
func handleRotateKey(rotator domain.KeyRotator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kid, retiredKID, retireAt, err := rotator.Rotate(r.Context())
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		writeJSON(w, http.StatusCreated, rotateKeyResponse{
			KID:        kid,
			RetiredKID: retiredKID,
			RetireAt:   retireAt,
		})
	}
}
