package delivery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cdamp/internal/adapters/signing"
	"cdamp/internal/app"
	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// testMessage returns a stable outbound domain.Message exercising every
// field client.go's envelope-reconstruction table maps, with a fixed
// (non-monotonic) SentAt/ExpiresAt so JSON round-trips compare exactly.
func testMessage() *domain.Message {
	sentAt := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	return &domain.Message{
		ID:             "msg_1757683200_a1b2c3",
		ThreadID:       "msg_1757683200_a1b2c3",
		From:           "researcher@example.dev",
		To:             "reviewer@other.dev",
		Subject:        "Question about the API",
		Body:           "plain text or Markdown body",
		Priority:       "normal",
		InReplyTo:      "",
		IdempotencyKey: "idk_9f3c1e2a-aaaa-bbbb-cccc-dddddddddddd",
		SentAt:         sentAt,
		ExpiresAt:      &expiresAt,
	}
}

// captureServer returns an httptest.Server that decodes every request body
// into a clientEnvelope and records both it and the raw JSON body, plus the
// inbox URL to hand to Deliver.
func captureServer(t *testing.T, status int) (srv *httptest.Server, got *clientEnvelope, rawBody *[]byte) {
	t.Helper()
	got = &clientEnvelope{}
	rawBody = &[]byte{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		*rawBody = body
		if err := json.Unmarshal(body, got); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.WriteHeader(status)
	}))
	return srv, got, rawBody
}

func TestDeliver_HappyPath_FieldRoundTrip(t *testing.T) {
	srv, got, _ := captureServer(t, http.StatusOK)
	defer srv.Close()

	signer := fakes.NewSignerFake("k1")
	c := NewClient(signer, nil)
	m := testMessage()

	if err := c.Deliver(context.Background(), m, srv.URL+"/deliver"); err != nil {
		t.Fatalf("Deliver: unexpected error: %v", err)
	}

	if got.Version != protocolVersion {
		t.Errorf("version = %q, want %q", got.Version, protocolVersion)
	}
	if got.ID != m.ID {
		t.Errorf("id = %q, want %q", got.ID, m.ID)
	}
	if got.From != m.From {
		t.Errorf("from = %q, want %q", got.From, m.From)
	}
	if got.To != m.To {
		t.Errorf("to = %q, want %q", got.To, m.To)
	}
	if got.Subject != m.Subject {
		t.Errorf("subject = %q, want %q", got.Subject, m.Subject)
	}
	if got.Priority != m.Priority {
		t.Errorf("priority = %q, want %q", got.Priority, m.Priority)
	}
	if !got.Timestamp.Equal(m.SentAt) {
		t.Errorf("timestamp = %v, want %v", got.Timestamp, m.SentAt)
	}
	if got.ThreadID != m.ThreadID {
		t.Errorf("thread_id = %q, want %q", got.ThreadID, m.ThreadID)
	}
	if got.InReplyTo != m.InReplyTo {
		t.Errorf("in_reply_to = %q, want %q", got.InReplyTo, m.InReplyTo)
	}
	if got.IdempotencyKey != m.IdempotencyKey {
		t.Errorf("idempotency_key = %q, want %q", got.IdempotencyKey, m.IdempotencyKey)
	}
	if got.Payload.Message != m.Body {
		t.Errorf("payload.message = %q, want %q", got.Payload.Message, m.Body)
	}
	if got.Payload.Type != "request" {
		t.Errorf("payload.type = %q, want %q", got.Payload.Type, "request")
	}
	if got.KID != "k1" {
		t.Errorf("kid = %q, want %q", got.KID, "k1")
	}
	if got.Signature == "" {
		t.Error("signature is empty, want non-empty")
	}
}

func TestDeliver_SignatureIsValid(t *testing.T) {
	srv, got, _ := captureServer(t, http.StatusOK)
	defer srv.Close()

	signer := fakes.NewSignerFake("k1")
	c := NewClient(signer, nil)
	m := testMessage()

	if err := c.Deliver(context.Background(), m, srv.URL+"/deliver"); err != nil {
		t.Fatalf("Deliver: unexpected error: %v", err)
	}

	sig, err := base64.StdEncoding.DecodeString(got.Signature)
	if err != nil {
		t.Fatalf("decoding signature base64: %v", err)
	}

	payload := map[string]any{
		"type":    got.Payload.Type,
		"message": got.Payload.Message,
		"context": got.Payload.Context,
	}
	canonical, err := app.CanonicalString(got.From, got.To, got.Subject, got.Priority, got.InReplyTo, payload)
	if err != nil {
		t.Fatalf("building canonical string: %v", err)
	}

	verifier := &signing.Ed25519Verifier{}
	if !verifier.Verify(canonical, sig, signer.PublicKey()) {
		t.Error("Verify() = false, want true for a correctly signed envelope")
	}

	// Tampering sanity check: a canonical string built from a mutated
	// field must no longer verify against the same signature.
	tamperedCanonical, err := app.CanonicalString(got.From, got.To, "tampered subject", got.Priority, got.InReplyTo, payload)
	if err != nil {
		t.Fatalf("building tampered canonical string: %v", err)
	}
	if verifier.Verify(tamperedCanonical, sig, signer.PublicKey()) {
		t.Error("Verify() = true for a tampered canonical string, want false")
	}
}

func TestDeliver_SignError(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK)
	defer srv.Close()

	signer := fakes.NewSignerFake("k1")
	signer.SetSignError(context.DeadlineExceeded)
	c := NewClient(signer, nil)

	if err := c.Deliver(context.Background(), testMessage(), srv.URL+"/deliver"); err == nil {
		t.Fatal("Deliver: expected error from signer, got nil")
	}
}

func TestDeliver_ExpiresAtOmittedWhenNil(t *testing.T) {
	srv, _, rawBody := captureServer(t, http.StatusOK)
	defer srv.Close()

	c := NewClient(fakes.NewSignerFake("k1"), nil)
	m := testMessage()
	m.ExpiresAt = nil

	if err := c.Deliver(context.Background(), m, srv.URL+"/deliver"); err != nil {
		t.Fatalf("Deliver: unexpected error: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*rawBody, &raw); err != nil {
		t.Fatalf("decoding raw body: %v", err)
	}
	if _, ok := raw["expires_at"]; ok {
		t.Errorf("expected \"expires_at\" to be absent, got %s", *rawBody)
	}
}

func TestDeliver_ExpiresAtIncludedWhenSet(t *testing.T) {
	srv, _, rawBody := captureServer(t, http.StatusOK)
	defer srv.Close()

	c := NewClient(fakes.NewSignerFake("k1"), nil)
	m := testMessage() // has a non-nil ExpiresAt

	if err := c.Deliver(context.Background(), m, srv.URL+"/deliver"); err != nil {
		t.Fatalf("Deliver: unexpected error: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*rawBody, &raw); err != nil {
		t.Fatalf("decoding raw body: %v", err)
	}
	if _, ok := raw["expires_at"]; !ok {
		t.Errorf("expected \"expires_at\" to be present, got %s", *rawBody)
	}
}

func TestDeliver_NonOKStatus(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusBadRequest)
	defer srv.Close()

	c := NewClient(fakes.NewSignerFake("k1"), nil)
	if err := c.Deliver(context.Background(), testMessage(), srv.URL+"/deliver"); err == nil {
		t.Fatal("Deliver: expected error for a 400 response, got nil")
	}
}

func TestDeliver_TransportFailure_ServerClosed(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK)
	inboxURL := srv.URL + "/deliver"
	srv.Close() // closed before Deliver is ever called

	c := NewClient(fakes.NewSignerFake("k1"), nil)
	if err := c.Deliver(context.Background(), testMessage(), inboxURL); err == nil {
		t.Fatal("Deliver: expected a transport error against a closed server, got nil")
	}
}

func TestDeliver_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(fakes.NewSignerFake("k1"), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := c.Deliver(ctx, testMessage(), srv.URL+"/deliver"); err == nil {
		t.Fatal("Deliver: expected a timeout error, got nil")
	}
}

func TestDeliver_Success_ReturnsNil(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK)
	defer srv.Close()

	c := NewClient(fakes.NewSignerFake("k1"), nil)
	if err := c.Deliver(context.Background(), testMessage(), srv.URL+"/deliver"); err != nil {
		t.Fatalf("Deliver: unexpected error: %v", err)
	}
}

// redirectTransport rewrites every outgoing request's scheme/host to target
// (an httptest.Server's URL) before delegating to the default transport —
// the same technique internal/adapters/directory/directory_test.go already
// established for HTTPDirectory, mirrored here to prove NewClient's new
// httpClient parameter is actually used, not just accepted.
type redirectTransport struct {
	target string
}

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	targetURL, err := http.NewRequest(req.Method, t.target+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	targetURL = targetURL.WithContext(req.Context())
	targetURL.Header = req.Header
	return http.DefaultTransport.RoundTrip(targetURL)
}

func TestDeliver_UsesInjectedHTTPClient(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK)
	defer srv.Close()

	customClient := &http.Client{
		Transport: redirectTransport{target: srv.URL},
	}
	c := NewClient(fakes.NewSignerFake("k1"), customClient)

	// An arbitrary, unroutable-in-reality host/scheme: only reaches srv at
	// all because customClient's Transport rewrites it there. If Deliver
	// used a default-constructed *http.Client instead of the injected one,
	// this request would fail with a DNS/transport error, not land on srv.
	if err := c.Deliver(context.Background(), testMessage(), "https://fake.invalid/deliver"); err != nil {
		t.Fatalf("Deliver: unexpected error: %v", err)
	}
}
