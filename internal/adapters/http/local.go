package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/config"
	"cdamp/internal/domain"
)

// NewLocalMux returns the HTTP handler for CDAMP's local agent API
// (03-API.md's "Local agent API (bearer token auth)" section): POST /send,
// GET /messages, GET /messages/{id}, GET /threads, GET /threads/{id}, GET
// /agents/me. Every route in this group requires bearer auth (there is no
// public route among these six — /.well-known/... and /deliver belong to
// federation.go, a different, not-yet-built file) and every request body is
// capped at app.MaxBodyBytes. Both are enforced by middleware wrapping the
// whole mux, never inside an individual handler, per
// 02-ARCHITECTURE.md's "REST is the single enforcement point" rule.
//
// Wiring this into cmd/cdampd/main.go is explicitly out of scope for this
// task (not in its file list per 04-BUILD-PLAN.md/STATUS.md) — that's a
// later task.
func NewLocalMux(store domain.InboxStore, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", handleSend(store, cfg))
	mux.HandleFunc("GET /messages", handleListMessages(store))
	mux.HandleFunc("GET /messages/{id}", handleGetMessage(store))
	mux.HandleFunc("GET /threads", handleSearchThreads(store))
	mux.HandleFunc("GET /threads/{id}", handleGetThread(store))
	mux.HandleFunc("GET /agents/me", handleAgentsMe(cfg))

	// sizeLimitMiddleware runs outermost (caps the body before anything
	// else touches it), bearerAuthMiddleware next (resolves/rejects the
	// caller before any route's handler body runs), then the mux/handlers.
	return sizeLimitMiddleware(bearerAuthMiddleware(store, mux))
}

// mapDomainError maps an error returned by an app use case or a direct
// store call to the HTTP status and 03-API.md error.code it corresponds
// to, per STATUS.md's task 2 error-mapping table. Built as one table/switch
// — per the go-hexagonal-style skill's "HTTP adapter is the only place
// that maps domain errors to status codes" rule — rather than scattered
// per-handler if chains. Ownership-check 404s (a resource that exists but
// isn't the bearer-resolved agent's own) are a separate, deliberate
// decision handled inline at each call site, not through this function,
// since they're not errors returned by the use case/store at all.
func mapDomainError(err error) (status int, code string) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		// Covers every app.SendMessage validation failure (missing field,
		// malformed `to`, oversized `body`, unresolvable `in_reply_to`) —
		// already decided in task 1's own spec as a single undifferentiated
		// sentinel, deliberately not split into more specific codes here.
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, domain.ErrDuplicateIdempotencyKey):
		// A reused idempotency_key across two distinct /send calls. Not
		// fully pinned by any doc (03-API.md's /send line only documents
		// bad_request for missing-field/oversized-body, and /deliver's
		// dedupe-to-200 behavior is a different, Phase-4-only code path per
		// errors.go's own doc comment) — bad_request is the defensible
		// default since 03-API.md's codes list has nothing more specific
		// for this local-API case. A builder judgment call, not a blocking
		// decision, per STATUS.md's task 2 spec.
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not_found"
	default:
		// Not one of the documented sentinel errors — an unexpected
		// failure (e.g. a store I/O error). 03-API.md's codes list has no
		// generic "something went wrong" entry, so this falls back to a
		// plain 500 with a clearly-labeled, non-spec code rather than
		// mis-mapping it to one of the documented codes above, none of
		// which fit an unexpected failure.
		return http.StatusInternalServerError, "internal_error"
	}
}

// sendRequest is the JSON body shape for POST /send, per 03-API.md:
// {to, subject, body, priority?, in_reply_to?, idempotency_key?}.
type sendRequest struct {
	To             string `json:"to"`
	Subject        string `json:"subject"`
	Body           string `json:"body"`
	Priority       string `json:"priority,omitempty"`
	InReplyTo      string `json:"in_reply_to,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// handleSend implements POST /send.
func handleSend(store domain.InboxStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := agentFromContext(r.Context())

		var req sendRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			return // decodeJSONBody already wrote the error response
		}

		result, err := app.SendMessage(r.Context(), store, app.SendMessageRequest{
			AgentID:        agent.ID,
			From:           agent.Name + "@" + cfg.Domain,
			To:             req.To,
			Subject:        req.Subject,
			Body:           req.Body,
			Priority:       req.Priority,
			InReplyTo:      req.InReplyTo,
			IdempotencyKey: req.IdempotencyKey,
		})
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		writeJSON(w, http.StatusAccepted, map[string]any{
			"id":     result.ID,
			"status": result.Status,
		})
	}
}

// decodeJSONBody decodes r.Body's JSON into dst, writing the appropriate
// error response and returning a non-nil error if decoding fails. A
// *http.MaxBytesError (r.Body was wrapped by sizeLimitMiddleware) maps to
// `400 body_too_large`; any other decode failure (malformed JSON) maps to
// the generic `400 bad_request`, since 03-API.md's /send line documents
// bad_request as the catch-all for a malformed request.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest, "body_too_large", "request body exceeds the 256KB limit")
			return err
		}
		writeError(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return err
	}
	return nil
}

// handleListMessages implements GET /messages.
//
// The `agent` query param (03-API.md: `GET
// /messages?agent=&q=&from=&thread=&status=&unread=&after=&limit=`) is
// read but deliberately never used to set MessageFilter.AgentID — per
// STATUS.md's task 2 spec, an agent may only ever read its own inbox, so
// AgentID always comes from the bearer-resolved agent, never from the
// query string (which would let a caller impersonate another agent's
// filter simply by passing a different id).
func handleListMessages(store domain.InboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := agentFromContext(r.Context())
		q := r.URL.Query()

		filter := domain.MessageFilter{
			AgentID: agent.ID,
			Query:   q.Get("q"),
			From:    q.Get("from"),
			Thread:  q.Get("thread"),
			Status:  q.Get("status"),
			Unread:  parseUnread(q),
			After:   q.Get("after"),
			Limit:   parseLimit(q.Get("limit")),
		}

		result, err := app.ListMessages(r.Context(), store, filter)
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		messages := make([]messageResponse, 0, len(result.Messages))
		for _, m := range result.Messages {
			messages = append(messages, newMessageResponse(m))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"messages":    messages,
			"next_cursor": result.NextCursor,
		})
	}
}

// handleGetMessage implements GET /messages/{id}. There is no dedicated
// app-layer use case for a single-message fetch (per 04-BUILD-PLAN.md's
// Phase 3 file list — only send_message.go, list_messages.go,
// get_thread.go, search_threads.go exist under internal/app), so this
// calls store.GetMessage directly, per STATUS.md's task 2 spec.
func handleGetMessage(store domain.InboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := agentFromContext(r.Context())
		id := r.PathValue("id")

		msg, err := store.GetMessage(r.Context(), id)
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		// Ownership check (human-resolved, see STATUS.md's task 2 spec):
		// a message that exists but isn't this agent's own is reported as
		// 404, never 403, so as not to leak that the message exists.
		if msg.AgentID != agent.ID {
			writeError(w, http.StatusNotFound, "not_found", "message not found")
			return
		}

		writeJSON(w, http.StatusOK, newMessageResponse(msg))
	}
}

// handleSearchThreads implements GET /threads.
//
// As with handleListMessages, the `agent` query param is read but never
// used to set the agentID passed to app.SearchThreads — that always comes
// from the bearer-resolved agent, for the same ownership-scoping reason.
func handleSearchThreads(store domain.InboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := agentFromContext(r.Context())
		q := r.URL.Query()

		filter := domain.ThreadFilter{
			After: q.Get("after"),
			Limit: parseLimit(q.Get("limit")),
		}

		result, err := app.SearchThreads(r.Context(), store, agent.ID, q.Get("q"), filter)
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		threads := make([]threadListItem, 0, len(result.Threads))
		for _, item := range result.Threads {
			threads = append(threads, threadListItem{
				ID:           item.Thread.ID,
				Subject:      item.Thread.Subject,
				CreatedAt:    item.Thread.CreatedAt,
				MessageCount: item.MessageCount,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"threads":     threads,
			"next_cursor": result.NextCursor,
		})
	}
}

// handleGetThread implements GET /threads/{id}.
func handleGetThread(store domain.InboxStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := agentFromContext(r.Context())
		id := r.PathValue("id")

		result, err := app.GetThread(r.Context(), store, id)
		if err != nil {
			status, code := mapDomainError(err)
			writeError(w, status, code, err.Error())
			return
		}

		// Ownership check (human-resolved 2026-09-12, per STATUS.md's task
		// 2 spec): domain.InboxStore.GetThread takes no agentID parameter
		// and 03-API.md documents no ownership error for this endpoint
		// either, so this is enforced in the HTTP layer only: the resolved
		// agent must own at least one message in the thread, or the whole
		// thread is reported as 404 (never 403, so as not to confirm the
		// thread exists at all).
		owns := false
		for _, m := range result.Messages {
			if m.AgentID == agent.ID {
				owns = true
				break
			}
		}
		if !owns {
			writeError(w, http.StatusNotFound, "not_found", "thread not found")
			return
		}

		messages := make([]messageResponse, 0, len(result.Messages))
		for _, m := range result.Messages {
			messages = append(messages, newMessageResponse(m))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"thread":   threadResponse{ID: result.Thread.ID, Subject: result.Thread.Subject, CreatedAt: result.Thread.CreatedAt},
			"messages": messages,
		})
	}
}

// handleAgentsMe implements GET /agents/me. There is no dedicated
// app-layer use case for it (same reasoning as handleGetMessage) — it
// simply reads the already-resolved *domain.Agent off the request context
// (set by bearerAuthMiddleware) and formats it per 03-API.md's
// `{address, created_at}` shape.
func handleAgentsMe(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := agentFromContext(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"address":    agent.Name + "@" + cfg.Domain,
			"created_at": agent.CreatedAt,
		})
	}
}

// messageResponse is the JSON shape local.go returns for a domain.Message,
// across every endpoint that returns one (GET /messages, GET
// /messages/{id}, GET /threads/{id}). domain.Message deliberately carries
// no JSON tags of its own — the domain layer has zero HTTP/JSON knowledge,
// per the go-hexagonal-style skill — and it doesn't carry the signed wire
// envelope's payload/signature/kid fields either, since those don't exist
// until Phase 4's signing lands. This is therefore the HTTP adapter's own
// "full message" shape (03-API.md's GET /messages/{id} says only "..full
// message incl. payload.." without pinning an exact field list): every
// domain.Message field, snake_case, matching 01-PROTOCOL.md's envelope
// naming wherever a field overlaps with the wire envelope (id, from, to,
// subject, priority, in_reply_to, thread_id, idempotency_key, expires_at),
// plus the local-API-only bookkeeping fields the envelope itself doesn't
// carry (body, sender_domain, trust, status, read, attempts,
// next_attempt, agent_id).
type messageResponse struct {
	ID             string     `json:"id"`
	ThreadID       string     `json:"thread_id"`
	AgentID        int64      `json:"agent_id"`
	Direction      string     `json:"direction"`
	From           string     `json:"from"`
	To             string     `json:"to"`
	SenderDomain   string     `json:"sender_domain"`
	Subject        string     `json:"subject"`
	Body           string     `json:"body"`
	Priority       string     `json:"priority"`
	InReplyTo      string     `json:"in_reply_to,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	SentAt         time.Time  `json:"sent_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Trust          string     `json:"trust"`
	Status         string     `json:"status"`
	Read           bool       `json:"read"`
	Attempts       int        `json:"attempts"`
	NextAttempt    *time.Time `json:"next_attempt,omitempty"`
}

func newMessageResponse(m *domain.Message) messageResponse {
	return messageResponse{
		ID:             m.ID,
		ThreadID:       m.ThreadID,
		AgentID:        m.AgentID,
		Direction:      m.Direction,
		From:           m.From,
		To:             m.To,
		SenderDomain:   m.SenderDomain,
		Subject:        m.Subject,
		Body:           m.Body,
		Priority:       m.Priority,
		InReplyTo:      m.InReplyTo,
		IdempotencyKey: m.IdempotencyKey,
		SentAt:         m.SentAt,
		ExpiresAt:      m.ExpiresAt,
		Trust:          m.Trust,
		Status:         m.Status,
		Read:           m.Read,
		Attempts:       m.Attempts,
		NextAttempt:    m.NextAttempt,
	}
}

// threadResponse is the JSON shape of a thread on its own (GET
// /threads/{id}'s "thread" field), per 03-API.md.
type threadResponse struct {
	ID        string    `json:"id"`
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"created_at"`
}

// threadListItem is the JSON shape of one entry in GET /threads'
// "threads" array, per 03-API.md: {id, subject, created_at,
// message_count}.
type threadListItem struct {
	ID           string    `json:"id"`
	Subject      string    `json:"subject"`
	CreatedAt    time.Time `json:"created_at"`
	MessageCount int       `json:"message_count"`
}

// parseUnread implements the `unread` query flag's "presence/true means
// unread only" semantics (domain.MessageFilter's own doc comment): absent
// entirely, or explicitly "false"/"0", means "no filter"; any other
// present value (including the empty string from a bare `?unread`) means
// unread-only.
func parseUnread(q url.Values) bool {
	if !q.Has("unread") {
		return false
	}
	v := q.Get("unread")
	return v != "false" && v != "0"
}

// parseLimit parses a `limit` query param as a non-negative int, returning
// 0 (meaning "unset" to both the app layer's and the store's own
// default-50/cap-200 logic) for anything empty or unparsable rather than
// erroring the whole request over it — 03-API.md doesn't document a
// bad_request case for a malformed limit param.
func parseLimit(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
