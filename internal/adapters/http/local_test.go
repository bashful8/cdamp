package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/config"
	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// messageIDRE matches SendMessage's server-assigned ID shape, mirroring
// internal/app/send_message_test.go's own assertion (never assert an exact
// random suffix, only the shape).
var messageIDRE = regexp.MustCompile(`^msg_[0-9]+_[0-9a-z]{6}$`)

func newTestMux(t *testing.T) (http.Handler, *fakes.InboxStoreFake, *config.Config) {
	t.Helper()
	store := fakes.NewInboxStoreFake()
	cfg := &config.Config{Domain: "example.dev"}
	return NewLocalMux(store, cfg), store, cfg
}

func doRequest(t *testing.T, mux http.Handler, method, target, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func TestLocalMuxMissingAuthHeaderRejected(t *testing.T) {
	mux, _, _ := newTestMux(t)
	rec := doRequest(t, mux, http.MethodGet, "/agents/me", "", nil)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestLocalMuxMalformedSchemeRejected(t *testing.T) {
	mux, _, _ := newTestMux(t)
	r := httptest.NewRequest(http.MethodGet, "/agents/me", nil)
	r.Header.Set("Authorization", "Basic somevalue")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestLocalMuxUnknownTokenRejected(t *testing.T) {
	mux, _, _ := newTestMux(t)
	rec := doRequest(t, mux, http.MethodGet, "/agents/me", "no-such-token", nil)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestLocalMuxSendOversizedBodyRejected(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})

	oversized := []byte(`{"to":"bob@other.dev","subject":"hi","body":"` + strings.Repeat("a", app.MaxBodyBytes+1) + `"}`)
	rec := doRequest(t, mux, http.MethodPost, "/send", "tok-alice", oversized)
	assertErrorResponse(t, rec, http.StatusBadRequest, "body_too_large")
}

func TestLocalMuxSendMissingToRejected(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})

	body, _ := json.Marshal(map[string]string{"subject": "hi", "body": "hello"})
	rec := doRequest(t, mux, http.MethodPost, "/send", "tok-alice", body)
	assertErrorResponse(t, rec, http.StatusBadRequest, "bad_request")
}

func TestLocalMuxSendHappyPath(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})

	body, _ := json.Marshal(map[string]string{
		"to":      "bob@other.dev",
		"subject": "hi",
		"body":    "hello",
	})
	rec := doRequest(t, mux, http.MethodPost, "/send", "tok-alice", body)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !messageIDRE.MatchString(resp.ID) {
		t.Fatalf("id %q doesn't match %s", resp.ID, messageIDRE)
	}
	if resp.Status != "pending" {
		t.Fatalf("status = %q, want pending", resp.Status)
	}
}

func TestLocalMuxAgentsMe(t *testing.T) {
	mux, store, _ := newTestMux(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice"), CreatedAt: createdAt})

	rec := doRequest(t, mux, http.MethodGet, "/agents/me", "tok-alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Address   string    `json:"address"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Address != "alice@example.dev" {
		t.Fatalf("address = %q, want alice@example.dev", resp.Address)
	}
	if !resp.CreatedAt.Equal(createdAt) {
		t.Fatalf("created_at = %v, want %v", resp.CreatedAt, createdAt)
	}
}

// TestLocalMuxMessageOwnershipReturns404NotForbidden covers STATUS.md's
// human-resolved ownership decision: a message that exists but belongs to
// a different agent must read back as 404 not_found, never 403.
func TestLocalMuxMessageOwnershipReturns404NotForbidden(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})
	store.AddAgent(&domain.Agent{ID: 2, Name: "bob", TokenHash: hashBearerToken("tok-bob")})

	sendBody, _ := json.Marshal(map[string]string{"to": "carol@other.dev", "subject": "hi", "body": "hello"})
	sendRec := doRequest(t, mux, http.MethodPost, "/send", "tok-alice", sendBody)
	if sendRec.Code != http.StatusAccepted {
		t.Fatalf("seeding message: status = %d, body = %s", sendRec.Code, sendRec.Body.String())
	}
	var sendResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sendRec.Body.Bytes(), &sendResp); err != nil {
		t.Fatalf("decoding send response: %v", err)
	}

	// The owner (alice) can read it back.
	ownerRec := doRequest(t, mux, http.MethodGet, "/messages/"+sendResp.ID, "tok-alice", nil)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("owner GET /messages/%s: status = %d, body = %s", sendResp.ID, ownerRec.Code, ownerRec.Body.String())
	}

	// A different agent (bob) gets 404, never 403.
	otherRec := doRequest(t, mux, http.MethodGet, "/messages/"+sendResp.ID, "tok-bob", nil)
	assertErrorResponse(t, otherRec, http.StatusNotFound, "not_found")
}

// TestLocalMuxThreadOwnershipReturns404NotForbidden mirrors the message
// ownership test above for GET /threads/{id}.
func TestLocalMuxThreadOwnershipReturns404NotForbidden(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})
	store.AddAgent(&domain.Agent{ID: 2, Name: "bob", TokenHash: hashBearerToken("tok-bob")})

	sendBody, _ := json.Marshal(map[string]string{"to": "carol@other.dev", "subject": "hi", "body": "hello"})
	sendRec := doRequest(t, mux, http.MethodPost, "/send", "tok-alice", sendBody)
	var sendResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sendRec.Body.Bytes(), &sendResp); err != nil {
		t.Fatalf("decoding send response: %v", err)
	}
	// A brand-new (non-reply) message's thread_id equals its own id.
	threadID := sendResp.ID

	ownerRec := doRequest(t, mux, http.MethodGet, "/threads/"+threadID, "tok-alice", nil)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("owner GET /threads/%s: status = %d, body = %s", threadID, ownerRec.Code, ownerRec.Body.String())
	}

	otherRec := doRequest(t, mux, http.MethodGet, "/threads/"+threadID, "tok-bob", nil)
	assertErrorResponse(t, otherRec, http.StatusNotFound, "not_found")
}

func TestLocalMuxMessageNotFound(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})

	rec := doRequest(t, mux, http.MethodGet, "/messages/msg_does_not_exist", "tok-alice", nil)
	assertErrorResponse(t, rec, http.StatusNotFound, "not_found")
}

func TestLocalMuxThreadNotFound(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})

	rec := doRequest(t, mux, http.MethodGet, "/threads/does-not-exist", "tok-alice", nil)
	assertErrorResponse(t, rec, http.StatusNotFound, "not_found")
}

func TestLocalMuxListMessagesScopedToBearerAgent(t *testing.T) {
	mux, store, _ := newTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: hashBearerToken("tok-alice")})
	store.AddAgent(&domain.Agent{ID: 2, Name: "bob", TokenHash: hashBearerToken("tok-bob")})

	aliceSend, _ := json.Marshal(map[string]string{"to": "carol@other.dev", "subject": "from alice", "body": "hi"})
	if rec := doRequest(t, mux, http.MethodPost, "/send", "tok-alice", aliceSend); rec.Code != http.StatusAccepted {
		t.Fatalf("alice send: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	bobSend, _ := json.Marshal(map[string]string{"to": "carol@other.dev", "subject": "from bob", "body": "hi"})
	if rec := doRequest(t, mux, http.MethodPost, "/send", "tok-bob", bobSend); rec.Code != http.StatusAccepted {
		t.Fatalf("bob send: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Even if bob tries to pass agent=1 in the query string, the bearer
	// token (bob's own) must win — never trust the query param.
	rec := doRequest(t, mux, http.MethodGet, "/messages?agent=1", "tok-bob", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Messages []messageResponse `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Subject != "from bob" {
		t.Fatalf("messages = %+v, want exactly bob's own message", resp.Messages)
	}
}
