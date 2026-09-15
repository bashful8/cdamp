package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// countingDirectory wraps fakes.DirectoryFake to count Resolve calls, so
// tests can assert Directory.Resolve is never called for the "external"
// trust case (per the testing list's explicit ask).
type countingDirectory struct {
	*fakes.DirectoryFake
	calls         int
	previousCalls int
}

func newCountingDirectory() *countingDirectory {
	return &countingDirectory{DirectoryFake: fakes.NewDirectoryFake()}
}

func (d *countingDirectory) Resolve(ctx context.Context, address string) (ed25519.PublicKey, string, string, error) {
	d.calls++
	return d.DirectoryFake.Resolve(ctx, address)
}

func (d *countingDirectory) ResolvePrevious(ctx context.Context, address string) (ed25519.PublicKey, string, error) {
	d.previousCalls++
	return d.DirectoryFake.ResolvePrevious(ctx, address)
}

// countingStore wraps fakes.InboxStoreFake to count SaveMessage calls.
// This matters specifically for the "non-duplicate despite same id"
// test: the fake's underlying map is keyed by message ID, so two accepted
// messages that happen to share an ID collapse to one map entry — the
// call count is what actually distinguishes "both calls were accepted and
// attempted to save" from "the second call was rejected as a duplicate
// before ever reaching SaveMessage."
type countingStore struct {
	*fakes.InboxStoreFake
	saveCalls int
}

func newCountingStore() *countingStore {
	return &countingStore{InboxStoreFake: fakes.NewInboxStoreFake()}
}

func (s *countingStore) SaveMessage(ctx context.Context, m *domain.Message) error {
	s.saveCalls++
	return s.InboxStoreFake.SaveMessage(ctx, m)
}

// baseReceiveMessageRequest returns a well-formed, unsigned (trust=
// "external") envelope addressed to a recipient the caller is expected to
// seed into the store via AddAgent(&domain.Agent{Name: "reviewer", ...}).
func baseReceiveMessageRequest() ReceiveMessageRequest {
	return ReceiveMessageRequest{
		Version:        protocolVersion,
		ID:             "msg_1757683200_a1b2c3",
		From:           "researcher@example.dev",
		To:             "reviewer@other.dev",
		Subject:        "Question about the API",
		Priority:       "normal",
		Timestamp:      time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC),
		ThreadID:       "msg_1757683200_a1b2c3",
		PayloadType:    "request",
		PayloadMessage: "hi",
		PayloadContext: map[string]any{},
	}
}

func seedRecipient(store *fakes.InboxStoreFake) *domain.Agent {
	a := &domain.Agent{ID: 1, Name: "reviewer", TokenHash: "irrelevant"}
	store.AddAgent(a)
	return a
}

// signRequest signs req's canonical string with priv (test-local, per the
// testing list: "do not go through domain.Signer for this") and attaches
// the resulting base64 Signature and kid to a copy of req, which it
// returns.
func signRequest(t *testing.T, req ReceiveMessageRequest, priv ed25519.PrivateKey, kid string) ReceiveMessageRequest {
	t.Helper()
	payload := map[string]any{
		"type":    req.PayloadType,
		"message": req.PayloadMessage,
		"context": req.PayloadContext,
	}
	canonical, err := CanonicalString(req.From, req.To, req.Subject, req.Priority, req.InReplyTo, payload)
	if err != nil {
		t.Fatalf("CanonicalString: %v", err)
	}
	sig := ed25519.Sign(priv, canonical)
	req.Signature = base64.StdEncoding.EncodeToString(sig)
	req.KID = kid
	return req
}

func TestReceiveMessageTrustExternal(t *testing.T) {
	store := newCountingStore()
	seedRecipient(store.InboxStoreFake)
	directory := newCountingDirectory()
	verifier := fakes.NewVerifierFake()

	req := baseReceiveMessageRequest()
	req.Signature = ""
	req.KID = ""

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Trust != "external" {
		t.Fatalf("Trust = %q, want external", res.Trust)
	}
	if directory.calls != 0 {
		t.Fatalf("Directory.Resolve called %d times, want 0 (no signature to verify)", directory.calls)
	}
}

func TestReceiveMessageTrustVerified(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedRecipient(store)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}
	directory.Add("researcher@example.dev", pub, "k1", "https://example.dev/deliver")

	req := signRequest(t, baseReceiveMessageRequest(), priv, "k1")

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Trust != "verified" {
		t.Fatalf("Trust = %q, want verified", res.Trust)
	}
}

func TestReceiveMessageTrustUntrustedBadSignature(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedRecipient(store)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}
	directory.Add("researcher@example.dev", pub, "k1", "https://example.dev/deliver")

	req := signRequest(t, baseReceiveMessageRequest(), priv, "k1")
	// Mutate a signed field after signing so the signature no longer
	// matches the (re-derived) canonical string.
	req.Subject = "a different subject entirely"

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Trust != "untrusted" {
		t.Fatalf("Trust = %q, want untrusted", res.Trust)
	}
}

func TestReceiveMessageTrustUntrustedResolveFails(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedRecipient(store)
	directory := fakes.NewDirectoryFake() // no entry registered for the sender
	verifier := fakes.NewVerifierFake()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}
	req := signRequest(t, baseReceiveMessageRequest(), priv, "k1")

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("expected no hard error when directory resolve fails, got: %v", err)
	}
	if res.Trust != "untrusted" {
		t.Fatalf("Trust = %q, want untrusted", res.Trust)
	}
}

func TestReceiveMessageTrustVerifiedViaPreviousKey(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedRecipient(store)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()

	// Current key: some unrelated keypair the sender has already rotated
	// away from signing with (simulates 01-PROTOCOL.md's post-rotation
	// state: Resolve now returns the *new* active key, not the one that
	// actually signed this message).
	currentPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating current keypair: %v", err)
	}
	directory.Add("researcher@example.dev", currentPub, "k2", "https://example.dev/deliver")

	// Previous (grace-period) key: what actually signed the message.
	prevPub, prevPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating previous keypair: %v", err)
	}
	directory.AddPrevious("researcher@example.dev", prevPub, "k1")

	req := signRequest(t, baseReceiveMessageRequest(), prevPriv, "k1")

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Trust != "verified" {
		t.Fatalf("Trust = %q, want verified (signed with the sender's previous, still-in-grace key)", res.Trust)
	}
}

func TestReceiveMessageTrustCurrentKeySucceedsWithoutConsultingPreviousKey(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedRecipient(store)
	directory := newCountingDirectory()
	verifier := fakes.NewVerifierFake()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}
	directory.Add("researcher@example.dev", pub, "k1", "https://example.dev/deliver")

	req := signRequest(t, baseReceiveMessageRequest(), priv, "k1")

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Trust != "verified" {
		t.Fatalf("Trust = %q, want verified", res.Trust)
	}
	if directory.previousCalls != 0 {
		t.Fatalf("ResolvePrevious called %d times, want 0 (current-key verification already succeeded)", directory.previousCalls)
	}
}

func TestReceiveMessageTrustUntrustedWhenNeitherCurrentNorPreviousKeyMatches(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedRecipient(store)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()

	currentPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating current keypair: %v", err)
	}
	directory.Add("researcher@example.dev", currentPub, "k2", "https://example.dev/deliver")

	prevPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating previous keypair: %v", err)
	}
	directory.AddPrevious("researcher@example.dev", prevPub, "k1")

	// Signed with neither of the two registered keys.
	_, unknownPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating unknown keypair: %v", err)
	}
	req := signRequest(t, baseReceiveMessageRequest(), unknownPriv, "k1")

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Trust != "untrusted" {
		t.Fatalf("Trust = %q, want untrusted", res.Trust)
	}
}

// TestReceiveMessageTrustUntrustedNoPreviousKeyRegistered mirrors an
// expired/absent previous key at the fake layer: no AddPrevious call at
// all, so ResolvePrevious reports ErrNotFound -- per
// domain.Directory.ResolvePrevious's own doc comment, indistinguishable
// from "grace period elapsed," exactly the outcome 04-BUILD-PLAN.md's
// scenario 5 names. The genuinely end-to-end version of this (a real
// elapsed grace period against a real HTTPDirectory) is
// cmd/cdampd/integration_test.go's
// TestIntegrationScenario5_ExpiredPreviousKeyUntrusted.
func TestReceiveMessageTrustUntrustedNoPreviousKeyRegistered(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedRecipient(store)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()

	currentPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating current keypair: %v", err)
	}
	directory.Add("researcher@example.dev", currentPub, "k2", "https://example.dev/deliver")

	_, unknownPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating unknown keypair: %v", err)
	}
	req := signRequest(t, baseReceiveMessageRequest(), unknownPriv, "k1")

	res, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Trust != "untrusted" {
		t.Fatalf("Trust = %q, want untrusted", res.Trust)
	}
}

func TestReceiveMessageIdempotentRedeliveryByKey(t *testing.T) {
	store := newCountingStore()
	seedRecipient(store.InboxStoreFake)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()
	ctx := context.Background()

	req := baseReceiveMessageRequest()
	req.IdempotencyKey = "idk_9f3c1e2a-0000-0000-0000-000000000000"

	first, err := ReceiveMessage(ctx, store, directory, verifier, req)
	if err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first call: Duplicate = true, want false")
	}

	second, err := ReceiveMessage(ctx, store, directory, verifier, req)
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("second call: Duplicate = false, want true")
	}
	if second.ID != first.ID {
		t.Fatalf("second call: ID = %q, want %q", second.ID, first.ID)
	}
	if second.Trust != "" {
		t.Fatalf("second call: Trust = %q, want \"\" for a duplicate", second.Trust)
	}
	if store.saveCalls != 1 {
		t.Fatalf("SaveMessage called %d times, want exactly 1", store.saveCalls)
	}
}

func TestReceiveMessageIdempotentRedeliveryBySenderDomainAndID(t *testing.T) {
	store := newCountingStore()
	seedRecipient(store.InboxStoreFake)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()
	ctx := context.Background()

	req := baseReceiveMessageRequest() // no IdempotencyKey set

	first, err := ReceiveMessage(ctx, store, directory, verifier, req)
	if err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first call: Duplicate = true, want false")
	}

	second, err := ReceiveMessage(ctx, store, directory, verifier, req)
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("second call: Duplicate = false, want true")
	}
	if store.saveCalls != 1 {
		t.Fatalf("SaveMessage called %d times, want exactly 1", store.saveCalls)
	}
}

func TestReceiveMessageNonDuplicateDespiteSameIDDifferentDomains(t *testing.T) {
	store := newCountingStore()
	seedRecipient(store.InboxStoreFake)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()
	ctx := context.Background()

	req1 := baseReceiveMessageRequest()
	req1.From = "researcher@example.dev"

	req2 := baseReceiveMessageRequest() // same ID
	req2.From = "researcher@another.dev"

	first, err := ReceiveMessage(ctx, store, directory, verifier, req1)
	if err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first call: Duplicate = true, want false")
	}

	second, err := ReceiveMessage(ctx, store, directory, verifier, req2)
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if second.Duplicate {
		t.Fatalf("second call: Duplicate = true, want false (different sender domain, same id)")
	}
	if store.saveCalls != 2 {
		t.Fatalf("SaveMessage called %d times, want exactly 2 (both accepted)", store.saveCalls)
	}
}

func TestReceiveMessageValidationFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReceiveMessageRequest)
	}{
		{name: "unrecognized version", mutate: func(r *ReceiveMessageRequest) { r.Version = "cdamp/9.9" }},
		{name: "missing id", mutate: func(r *ReceiveMessageRequest) { r.ID = "" }},
		{name: "missing from", mutate: func(r *ReceiveMessageRequest) { r.From = "" }},
		{name: "missing to", mutate: func(r *ReceiveMessageRequest) { r.To = "" }},
		{name: "missing subject", mutate: func(r *ReceiveMessageRequest) { r.Subject = "" }},
		{name: "to missing @", mutate: func(r *ReceiveMessageRequest) { r.To = "revieweror.dev" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newCountingStore()
			seedRecipient(store.InboxStoreFake)
			directory := fakes.NewDirectoryFake()
			verifier := fakes.NewVerifierFake()

			req := baseReceiveMessageRequest()
			tc.mutate(&req)

			_, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected domain.ErrValidation, got %v", err)
			}
			if store.saveCalls != 0 {
				t.Fatalf("SaveMessage called %d times, want 0", store.saveCalls)
			}
		})
	}
}

func TestReceiveMessageUnknownLocalRecipient(t *testing.T) {
	store := newCountingStore() // no recipient seeded
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()

	req := baseReceiveMessageRequest()

	_, err := ReceiveMessage(context.Background(), store, directory, verifier, req)
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected domain.ErrValidation, got %v", err)
	}
	if store.saveCalls != 0 {
		t.Fatalf("SaveMessage called %d times, want 0", store.saveCalls)
	}
}

func TestReceiveMessageSavedRowFields(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	recipient := seedRecipient(store)
	directory := fakes.NewDirectoryFake()
	verifier := fakes.NewVerifierFake()
	ctx := context.Background()

	req := baseReceiveMessageRequest()

	res, err := ReceiveMessage(ctx, store, directory, verifier, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Duplicate {
		t.Fatalf("Duplicate = true, want false")
	}

	saved, err := store.GetMessage(ctx, res.ID)
	if err != nil {
		t.Fatalf("saved message not found: %v", err)
	}
	if saved.Direction != "in" {
		t.Errorf("Direction = %q, want in", saved.Direction)
	}
	if saved.Status != "received" {
		t.Errorf("Status = %q, want received", saved.Status)
	}
	if saved.SenderDomain != "example.dev" {
		t.Errorf("SenderDomain = %q, want example.dev", saved.SenderDomain)
	}
	if saved.AgentID != recipient.ID {
		t.Errorf("AgentID = %d, want %d", saved.AgentID, recipient.ID)
	}
	if saved.Body != req.PayloadMessage {
		t.Errorf("Body = %q, want %q", saved.Body, req.PayloadMessage)
	}
}
