package http

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cdamp/internal/config"
	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

func newFederationTestMux(t *testing.T) (http.Handler, *fakes.InboxStoreFake, *fakes.SigningKeyStoreFake, *fakes.DirectoryFake, *config.Config) {
	t.Helper()
	store := fakes.NewInboxStoreFake()
	keys := fakes.NewSigningKeyStoreFake()
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()
	cfg := &config.Config{Domain: "example.dev"}
	mux := NewFederationMux(store, keys, directory, verifier, cfg)
	return mux, store, keys, directory, cfg
}

func seedActiveKey(keys *fakes.SigningKeyStoreFake, kid string) ed25519.PublicKey {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	keys.AddSigningKey(&domain.SigningKey{
		KID:        kid,
		PublicKey:  pub,
		PrivateKey: priv,
		Active:     true,
		CreatedAt:  time.Now(),
	})
	return pub
}

func doFederationGet(mux http.Handler, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func doFederationPost(mux http.Handler, target string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func TestWellKnownAgentSuccess(t *testing.T) {
	mux, store, keys, _, cfg := newFederationTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice"})
	pub := seedActiveKey(keys, "k1")

	rec := doFederationGet(mux, "/.well-known/cdamp/alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		PublicKey string `json:"public_key"`
		KID       string `json:"kid"`
		InboxURL  string `json:"inbox_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	gotPub, err := base64.StdEncoding.DecodeString(resp.PublicKey)
	if err != nil {
		t.Fatalf("decoding public_key base64: %v", err)
	}
	if !bytes.Equal(gotPub, pub) {
		t.Fatalf("public_key = %x, want %x", gotPub, pub)
	}
	if resp.KID != "k1" {
		t.Fatalf("kid = %q, want %q", resp.KID, "k1")
	}
	wantInboxURL := "https://" + cfg.Domain + "/deliver"
	if resp.InboxURL != wantInboxURL {
		t.Fatalf("inbox_url = %q, want %q", resp.InboxURL, wantInboxURL)
	}
}

func TestWellKnownAgentNotFound(t *testing.T) {
	mux, _, keys, _, _ := newFederationTestMux(t)
	seedActiveKey(keys, "k1")

	rec := doFederationGet(mux, "/.well-known/cdamp/nobody")
	assertErrorResponse(t, rec, http.StatusNotFound, "not_found")
}

func TestWellKnownKeysNoPrevious(t *testing.T) {
	mux, _, keys, _, _ := newFederationTestMux(t)
	seedActiveKey(keys, "k1")

	rec := doFederationGet(mux, "/.well-known/cdamp/keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := raw["current"]; !ok {
		t.Fatalf("expected \"current\" key in response, got %s", rec.Body.String())
	}
	if _, ok := raw["previous"]; ok {
		t.Fatalf("expected \"previous\" key to be absent, got %s", rec.Body.String())
	}
}

func TestWellKnownKeysWithPreviousInGracePeriod(t *testing.T) {
	mux, _, keys, _, _ := newFederationTestMux(t)
	seedActiveKey(keys, "k2")
	seedActiveKey(keys, "k1") // will retire below
	if err := keys.RetireSigningKey(context.Background(), "k1", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}

	rec := doFederationGet(mux, "/.well-known/cdamp/keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := raw["current"]; !ok {
		t.Fatalf("expected \"current\" key in response, got %s", rec.Body.String())
	}
	if _, ok := raw["previous"]; !ok {
		t.Fatalf("expected \"previous\" key in response, got %s", rec.Body.String())
	}
}

func TestWellKnownKeysWithExpiredPreviousOmitted(t *testing.T) {
	mux, _, keys, _, _ := newFederationTestMux(t)
	seedActiveKey(keys, "k2")
	seedActiveKey(keys, "k1")
	if err := keys.RetireSigningKey(context.Background(), "k1", time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("RetireSigningKey: %v", err)
	}

	rec := doFederationGet(mux, "/.well-known/cdamp/keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := raw["previous"]; ok {
		t.Fatalf("expected \"previous\" key to be absent for an expired grace period, got %s", rec.Body.String())
	}
}

func validEnvelope(overrides map[string]any) []byte {
	env := map[string]any{
		"version":         "cdamp/0.1",
		"id":              "msg_1757683200_a1b2c3",
		"from":            "researcher@other.dev",
		"to":              "bob@example.dev",
		"subject":         "hi",
		"priority":        "normal",
		"timestamp":       "2026-09-12T14:00:00Z",
		"idempotency_key": "",
		"payload": map[string]any{
			"type":    "request",
			"message": "hello",
			"context": map[string]any{},
		},
	}
	for k, v := range overrides {
		env[k] = v
	}
	b, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return b
}

func TestDeliverHappyPathUnsigned(t *testing.T) {
	mux, store, _, _, _ := newFederationTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "bob"})

	body := validEnvelope(nil)
	rec := doFederationPost(mux, "/deliver", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	msg, err := store.GetMessage(context.Background(), "msg_1757683200_a1b2c3")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Direction != "in" {
		t.Fatalf("Direction = %q, want %q", msg.Direction, "in")
	}
	if msg.Trust != "external" {
		t.Fatalf("Trust = %q, want %q", msg.Trust, "external")
	}
}

func TestDeliverMissingFieldRejected(t *testing.T) {
	mux, store, _, _, _ := newFederationTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "bob"})

	body := validEnvelope(map[string]any{"subject": ""})
	rec := doFederationPost(mux, "/deliver", body)
	assertErrorResponse(t, rec, http.StatusBadRequest, "bad_request")

	if _, err := store.GetMessage(context.Background(), "msg_1757683200_a1b2c3"); err == nil {
		t.Fatalf("expected message not to be saved")
	}
}

func TestDeliverBadVersionRejected(t *testing.T) {
	mux, store, _, _, _ := newFederationTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "bob"})

	body := validEnvelope(map[string]any{"version": "cdamp/9.9"})
	rec := doFederationPost(mux, "/deliver", body)
	assertErrorResponse(t, rec, http.StatusBadRequest, "bad_request")

	if _, err := store.GetMessage(context.Background(), "msg_1757683200_a1b2c3"); err == nil {
		t.Fatalf("expected message not to be saved")
	}
}

func TestDeliverUnresolvableRecipientRejected(t *testing.T) {
	mux, _, _, _, _ := newFederationTestMux(t)
	// no agent seeded at all

	body := validEnvelope(nil)
	rec := doFederationPost(mux, "/deliver", body)
	assertErrorResponse(t, rec, http.StatusBadRequest, "bad_request")
}

func TestDeliverIdempotentRedelivery(t *testing.T) {
	mux, store, _, _, _ := newFederationTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "bob"})

	body := validEnvelope(map[string]any{"idempotency_key": "idk_abc123"})

	rec1 := doFederationPost(mux, "/deliver", body)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want 200 (body: %s)", rec1.Code, rec1.Body.String())
	}
	rec2 := doFederationPost(mux, "/deliver", body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("redelivery status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}

	if _, err := store.GetMessage(context.Background(), "msg_1757683200_a1b2c3"); err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
}

func TestDeliverOversizedBodyRejected(t *testing.T) {
	mux, store, _, _, _ := newFederationTestMux(t)
	store.AddAgent(&domain.Agent{ID: 1, Name: "bob"})

	oversized := validEnvelope(map[string]any{
		"payload": map[string]any{
			"type":    "request",
			"message": strings.Repeat("a", 300000),
			"context": map[string]any{},
		},
	})
	rec := doFederationPost(mux, "/deliver", oversized)
	assertErrorResponse(t, rec, http.StatusBadRequest, "body_too_large")
}
