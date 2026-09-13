package http

import (
	"errors"
	"net/http"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/config"
	"cdamp/internal/domain"
)

// NewFederationMux returns the HTTP handler for CDAMP's federation
// surface (03-API.md's "Federation surface" section): GET
// /.well-known/cdamp/{agent}, GET /.well-known/cdamp/keys, POST /deliver.
// None of these routes require bearer auth — TLS validity on the
// well-known endpoints is itself the domain-ownership proof, per
// 01-PROTOCOL.md's Discovery section — but /deliver is still
// size-limited by the same sizeLimitMiddleware local.go uses (already a
// package-level helper in this package, not local.go-specific).
//
// Wiring this into cmd/cdampd/main.go is explicitly out of scope, same
// as every prior task's file.
func NewFederationMux(
	store domain.InboxStore,
	keys domain.SigningKeyStore,
	directory domain.Directory,
	verifier domain.Verifier,
	cfg *config.Config,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/cdamp/{agent}", handleWellKnownAgent(store, keys, cfg))
	mux.HandleFunc("GET /.well-known/cdamp/keys", handleWellKnownKeys(keys, cfg))
	mux.HandleFunc("POST /deliver", handleDeliver(store, directory, verifier))

	return sizeLimitMiddleware(mux)
}

// wellKnownAgentResponse is GET /.well-known/cdamp/{agent}'s response
// shape, per 01-PROTOCOL.md's Discovery section and 03-API.md:
// {public_key, kid, inbox_url}. PublicKey is ed25519.PublicKey ([]byte)
// with a json:"public_key" tag, so encoding/json auto-base64-encodes it
// (std alphabet) — no manual base64 call needed.
type wellKnownAgentResponse struct {
	PublicKey []byte `json:"public_key"`
	KID       string `json:"kid"`
	InboxURL  string `json:"inbox_url"`
}

// handleWellKnownAgent implements GET /.well-known/cdamp/{agent}. The
// path segment names a local agent (existence gates the response), but
// the returned key material is always the domain's shared signing key,
// never a per-agent key (01-PROTOCOL.md Signing: "One Ed25519 keypair per
// domain... not per-agent") — the resolved *domain.Agent itself is
// otherwise unused, this call is purely the existence gate.
func handleWellKnownAgent(store domain.InboxStore, keys domain.SigningKeyStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if _, err := store.FindAgentByName(ctx, r.PathValue("agent")); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "agent not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		active, err := keys.GetActiveSigningKey(ctx, cfg.SigningKeyPassphrase)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, wellKnownAgentResponse{
			PublicKey: active.PublicKey,
			KID:       active.KID,
			InboxURL:  "https://" + cfg.Domain + "/deliver",
		})
	}
}

// wellKnownKeyEntry is one {kid, pubkey} entry in GET
// /.well-known/cdamp/keys' response — note the field name is "pubkey"
// here, not "public_key" as in handleWellKnownAgent's response;
// 03-API.md's two well-known responses deliberately use different field
// names for the same value and this task has no authority to unify them.
type wellKnownKeyEntry struct {
	KID    string `json:"kid"`
	PubKey []byte `json:"pubkey"`
}

// wellKnownKeysResponse is GET /.well-known/cdamp/keys' response shape,
// per 03-API.md: {current: {kid, pubkey}, previous?: {kid, pubkey}}.
// Previous is a pointer tagged omitempty so it is omitted from the JSON
// entirely (not null, not a zero-valued object) when there is no key in
// the rotation grace period.
type wellKnownKeysResponse struct {
	Current  wellKnownKeyEntry  `json:"current"`
	Previous *wellKnownKeyEntry `json:"previous,omitempty"`
}

// handleWellKnownKeys implements GET /.well-known/cdamp/keys. There is no
// agent-existence gate for this route at all.
func handleWellKnownKeys(keys domain.SigningKeyStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		current, err := keys.GetActiveSigningKey(ctx, cfg.SigningKeyPassphrase)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		resp := wellKnownKeysResponse{
			Current: wellKnownKeyEntry{KID: current.KID, PubKey: current.PublicKey},
		}

		// GetPreviousSigningKey already applies the grace-period filter
		// itself (active=0 AND retire_at > now, bound server-side) — a
		// domain.ErrNotFound here means either no previous key exists at
		// all, or one exists but its grace period has elapsed; both cases
		// omit "previous" from the response identically.
		previous, err := keys.GetPreviousSigningKey(ctx, cfg.SigningKeyPassphrase)
		if err == nil {
			resp.Previous = &wellKnownKeyEntry{KID: previous.KID, PubKey: previous.PublicKey}
		} else if !errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// deliverPayload mirrors 01-PROTOCOL.md's Envelope `payload` object:
// {type, message, context}.
type deliverPayload struct {
	Type    string         `json:"type"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

// deliverEnvelope mirrors 01-PROTOCOL.md's wire Envelope shape 1:1:
// version, id, from, to, subject, priority, timestamp, expires_at,
// signature, kid, in_reply_to, thread_id, idempotency_key, payload.
type deliverEnvelope struct {
	Version        string         `json:"version"`
	ID             string         `json:"id"`
	From           string         `json:"from"`
	To             string         `json:"to"`
	Subject        string         `json:"subject"`
	Priority       string         `json:"priority"`
	Timestamp      time.Time      `json:"timestamp"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	Signature      string         `json:"signature,omitempty"`
	KID            string         `json:"kid,omitempty"`
	InReplyTo      string         `json:"in_reply_to,omitempty"`
	ThreadID       string         `json:"thread_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Payload        deliverPayload `json:"payload"`
}

// handleDeliver implements POST /deliver. Rate limiting and blocklist
// enforcement (01-PROTOCOL.md's /deliver response codes 403/429) are not
// built here — Phase 8's job, per STATUS.md's task 4 spec — so this
// handler only decodes the envelope, calls app.ReceiveMessage, and maps
// its result/errors to bad_request/internal_error/200.
func handleDeliver(store domain.InboxStore, directory domain.Directory, verifier domain.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var env deliverEnvelope
		if err := decodeJSONBody(w, r, &env); err != nil {
			return // decodeJSONBody already wrote the error response
		}

		// id/timestamp are read straight off the wire, never re-derived:
		// unlike /send, /deliver receives an envelope some other instance
		// already finalized.
		req := app.ReceiveMessageRequest{
			Version:        env.Version,
			ID:             env.ID,
			From:           env.From,
			To:             env.To,
			Subject:        env.Subject,
			Priority:       env.Priority,
			Timestamp:      env.Timestamp,
			ExpiresAt:      env.ExpiresAt,
			InReplyTo:      env.InReplyTo,
			ThreadID:       env.ThreadID,
			IdempotencyKey: env.IdempotencyKey,
			Signature:      env.Signature,
			KID:            env.KID,
			PayloadType:    env.Payload.Type,
			PayloadMessage: env.Payload.Message,
			PayloadContext: env.Payload.Context,
		}

		result, err := app.ReceiveMessage(r.Context(), store, directory, verifier, req)
		if err != nil {
			if errors.Is(err, domain.ErrValidation) {
				writeError(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		// Always 200 for both a fresh accept and a Duplicate no-op, per
		// 03-API.md's explicit "includes idempotent no-op for a duplicate
		// idempotency_key" line.
		writeJSON(w, http.StatusOK, map[string]any{
			"id":     result.ID,
			"status": "accepted",
		})
	}
}
