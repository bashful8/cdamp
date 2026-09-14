package main

// This file implements STATUS.md's "Task 4 spec — two-instance loopback
// integration test": a real, in-process, two-instance (A + B) federated
// delivery test built directly against this package's own newMux and
// loggingMiddleware (not run() itself — see startTestInstance's doc
// comment for why), using a loopbackRewriteTransport to make each
// instance's real https://<fake-domain> HTTP calls (Directory.Resolve,
// Client.Deliver) actually land on the other instance's real loopback
// listener. No mocking of domain.Signer/domain.Verifier/domain.Delivery —
// every adapter here is the real, already-verified production adapter.
//
// Every scenario polls across at least one real ~10s worker tick
// (Worker.Run's tickInterval, unexported/not injectable) — see "Known
// integration-test cost" in STATUS.md's Task 4 spec. The three scenario
// tests run as parallel subtests (t.Parallel()) purely to bound this
// file's total real wall-clock cost to roughly the slowest single
// scenario rather than their sum; each builds its own fully independent
// pair of instances (own ports, own temp SQLite files), so running them
// concurrently is safe.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cdamp/internal/adapters/delivery"
	"cdamp/internal/adapters/directory"
	"cdamp/internal/adapters/signing"
	"cdamp/internal/adapters/storage/sqlite"
	"cdamp/internal/config"
	"cdamp/internal/domain"

	httpadapter "cdamp/internal/adapters/http"
)

// shortTestRetrySchedule is the short, test-only retry schedule required
// for instance A, per STATUS.md's Task 4 spec ("What the test sets up"):
// 01-PROTOCOL.md's real 1m/5m/30m/2h/12h production schedule would make
// Scenario 2's retry unclaimable for a full real minute, blowing past this
// test's generous ~20-30s tick-bound assumptions. Every step here is a few
// seconds, so each scenario's real-time cost stays anchored to the
// worker's 10s tick alone, exactly as those bounds assume. Exact values
// are a builder judgment call per the spec ("as long as every step is a
// few seconds, not minutes/hours").
func shortTestRetrySchedule() []time.Duration {
	return []time.Duration{2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second, 6 * time.Second}
}

// productionRetrySchedule mirrors 01-PROTOCOL.md's real backoff schedule.
// Used only for instance B, which never originates outbound mail in any
// scenario here, so its own retry schedule is irrelevant — kept at the
// real production values rather than the short test schedule, per the
// spec ("B's own cfg.RetrySchedule is irrelevant to every scenario here
// ... and can be left at any value, including the real production
// schedule").
func productionRetrySchedule() []time.Duration {
	return []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}
}

// hashTestBearerToken duplicates internal/adapters/http/middleware.go's
// unexported hashBearerToken exactly (plain SHA-256, hex-encoded) — the
// only way this test (a different package) can mint a token_hash that
// bearerAuthMiddleware's own FindAgentByTokenHash lookup will actually
// match, per STATUS.md's Task 4 spec's explicit instruction to duplicate
// this scheme rather than import it.
func hashTestBearerToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// seedAgentRaw inserts a minimal agents row directly against sqlitePath's
// on-disk SQLite file, mirroring the seedAgent-direct-SQL precedent
// already established in internal/adapters/storage/sqlite/store_test.go
// (there's still no CreateAgent use case — Phase 6). That precedent
// reaches s.db directly because store_test.go lives in the same package
// as *sqlite.Store; this package (main) cannot reach *sqlite.Store's
// unexported db field from the outside, so this instead opens its own
// short-lived *sql.DB against the same file (safe to do here: by the time
// this is called, startTestInstance's own sqlite.Open has already run
// every migration, so the agents table already exists).
func seedAgentRaw(t *testing.T, sqlitePath string, id int64, name, tokenHash string) {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", sqlitePath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("opening sqlite to seed agent %q: %v", name, err)
	}
	defer db.Close()

	if _, err := db.Exec(
		"INSERT INTO agents (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)",
		id, name, tokenHash, time.Now().Unix(),
	); err != nil {
		t.Fatalf("seeding agent %q: %v", name, err)
	}
}

// onRequestFunc is loopbackRewriteTransport's optional per-request hook.
// It may short-circuit a request — returning short=true along with the
// response/error to hand back instead of ever forwarding the request —
// or let it proceed untouched (short=false, response/error ignored).
// Scenario 2 uses this to force exactly the first POST /deliver to fail.
type onRequestFunc func(req *http.Request) (resp *http.Response, err error, short bool)

// loopbackRewriteTransport is a custom http.RoundTripper that makes an
// adapter's own real https://<fake-domain>/... request construction
// (directory.HTTPDirectory.fetch, delivery.Client.Deliver — neither
// modified nor aware of being under test) actually land on a real
// loopback listener. See STATUS.md's Task 4 spec, "Gap history" and "The
// redirect table": since cdampd never serves TLS at all, rewriting the
// request's scheme and host before the transport ever attempts a
// handshake is the only way to make this work without changing either
// adapter's production code path.
//
// targets is guarded by a mutex (a small addition beyond the spec's plain
// map field, not new scope) rather than being populated at construction
// time, because the two instances' real listener addresses aren't known
// until *after* both instances are already started — each instance's
// Directory/Client need a *http.Client at construction time, before the
// other instance's address exists yet. setTarget lets setupFederationPair
// populate the map once both real addresses are known, without racing the
// worker goroutine's later reads (under -race).
type loopbackRewriteTransport struct {
	mu      sync.Mutex
	targets map[string]string // fake domain (host:port form) -> real 127.0.0.1:port

	// base is the underlying transport that actually performs the
	// (possibly rewritten) request. Defaults to http.DefaultTransport.
	base http.RoundTripper

	// onRequest, if set, is consulted before every request. See
	// onRequestFunc's doc comment.
	onRequest onRequestFunc

	// requests counts every RoundTrip call, regardless of outcome —
	// Scenario 3 uses this to prove zero delivery-side requests were ever
	// attempted for an already-expired message.
	requests atomic.Int32
}

func newLoopbackRewriteTransport(onRequest onRequestFunc) *loopbackRewriteTransport {
	return &loopbackRewriteTransport{targets: map[string]string{}, onRequest: onRequest}
}

// setTarget records that requests to fakeHost should be rewritten to
// realAddr (a real 127.0.0.1:<port> loopback address).
func (t *loopbackRewriteTransport) setTarget(fakeHost, realAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.targets[fakeHost] = realAddr
}

// RoundTrip implements http.RoundTripper. It clones req, and if
// req.URL.Host matches a key in targets, rewrites the scheme to "http"
// and the host to the corresponding real loopback address before
// delegating to base.
func (t *loopbackRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests.Add(1)

	if t.onRequest != nil {
		if resp, err, short := t.onRequest(req); short {
			return resp, err
		}
	}

	req = req.Clone(req.Context())

	t.mu.Lock()
	real, ok := t.targets[req.URL.Host]
	t.mu.Unlock()
	if ok {
		req.URL.Scheme = "http"
		req.URL.Host = real
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// startTestInstance builds one fully-wired cdampd instance in-process,
// using the exact same construction sequence run() uses (sqlite.Open,
// signing.NewSigner, signing.NewVerifier, directory.NewHTTPDirectory,
// delivery.NewClient, delivery.NewWorker, newMux) against a fresh
// cfg.SQLitePath, then serves it on a real ephemeral loopback port.
//
// This deliberately does not call run() itself, for two reasons specific
// to this test (per STATUS.md's Task 4 spec): (a) run() hardcodes
// OS-signal-based shutdown (signal.NotifyContext), which a test can't
// cleanly trigger per-instance without sending real OS signals to its own
// test process; (b) run() calls server.ListenAndServe() against
// cfg.ListenAddr as a bare string, which can't hand back the OS-assigned
// port when cfg.ListenAddr is "127.0.0.1:0" — this test needs each
// instance's real port *before* it can build the other instance's
// redirect table.
func startTestInstance(t *testing.T, cfg *config.Config, dirClient, deliveryHTTPClient *http.Client) (addr string, store *sqlite.Store) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for test instance %s: %v", cfg.Domain, err)
	}

	store, err = sqlite.Open(cfg.SQLitePath)
	if err != nil {
		t.Fatalf("opening sqlite store for %s: %v", cfg.Domain, err)
	}

	// context.Background(), not a cancelable context: this is one-time
	// startup work, mirroring run()'s own use of context.Background() for
	// signer bootstrap.
	signer, err := signing.NewSigner(context.Background(), store, cfg.SigningKeyPassphrase)
	if err != nil {
		t.Fatalf("initializing signer for %s: %v", cfg.Domain, err)
	}
	verifier := signing.NewVerifier()

	dir := directory.NewHTTPDirectory(dirClient, cfg.DirectoryCacheTTL)
	deliveryClient := delivery.NewClient(signer, deliveryHTTPClient)
	worker := delivery.NewWorker(store, dir, deliveryClient, cfg.RetrySchedule)

	// Generous rate/burst: this test's real end-to-end federation traffic
	// must never be rejected by rate limiting, which is unrelated to what
	// this file tests.
	limiters := httpadapter.NewDomainLimiters(1000, 1000)
	mux := newMux(store, store, dir, verifier, store, limiters, cfg)

	// io.Discard: this test asserts on message/store state, not log
	// output, and letting every instance log to the test's real stdout
	// would just be noise across three parallel scenarios.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())

	go worker.Run(ctx)
	go func() {
		_ = http.Serve(listener, loggingMiddleware(logger, mux))
	}()

	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		_ = store.Close()
	})

	return listener.Addr().String(), store
}

// setupFederationPair builds two full, independently-keyed cdampd
// instances — A at domain "a.loopback.test", B at domain
// "b.loopback.test" — each with its own listener/port, its own temp
// SQLite file, and its own signing key bootstrapped fresh by
// signing.NewSigner (no shared key material between them, matching two
// genuinely independent domains). Each instance's Directory and
// delivery.Client share one loopbackRewriteTransport-backed *http.Client,
// wired so their https://*.loopback.test requests land on the other
// instance's real loopback address, per STATUS.md's Task 4 spec ("The
// redirect table"). Seeds a sender agent on A (name "sender", bearer
// token senderToken) and a recipient agent on B (name "recipient", token
// irrelevant — B's inbox is asserted via direct store access, per the
// spec's own preference).
//
// aOnRequest, if non-nil, becomes A's delivery-side transport's request
// hook (Scenario 2 uses this to force the first POST /deliver to fail).
func setupFederationPair(t *testing.T, aRetrySchedule []time.Duration, aOnRequest onRequestFunc, senderToken string) (aAddr, bAddr string, aStore, bStore *sqlite.Store, aTransport *loopbackRewriteTransport) {
	t.Helper()

	aTransport = newLoopbackRewriteTransport(aOnRequest)
	bTransport := newLoopbackRewriteTransport(nil)

	aClient := &http.Client{Transport: aTransport}
	bClient := &http.Client{Transport: bTransport}

	aPath := filepath.Join(t.TempDir(), "a.db")
	bPath := filepath.Join(t.TempDir(), "b.db")

	aCfg := &config.Config{
		Domain:               "a.loopback.test",
		ListenAddr:           "127.0.0.1:0",
		SQLitePath:           aPath,
		SigningKeyPassphrase: "test-passphrase-a",
		RetrySchedule:        aRetrySchedule,
		DirectoryCacheTTL:    5 * time.Minute,
	}
	bCfg := &config.Config{
		Domain:               "b.loopback.test",
		ListenAddr:           "127.0.0.1:0",
		SQLitePath:           bPath,
		SigningKeyPassphrase: "test-passphrase-b",
		RetrySchedule:        productionRetrySchedule(),
		DirectoryCacheTTL:    5 * time.Minute,
	}

	// Each instance's Directory and delivery.Client share the same
	// *http.Client, per the spec: "used for both that instance's
	// Directory and its delivery.Client."
	aAddr, aStore = startTestInstance(t, aCfg, aClient, aClient)
	bAddr, bStore = startTestInstance(t, bCfg, bClient, bClient)

	// The redirect table, built once both real addresses are known.
	aTransport.setTarget("b.loopback.test", bAddr)
	bTransport.setTarget("a.loopback.test", aAddr)

	seedAgentRaw(t, aPath, 1, "sender", hashTestBearerToken(senderToken))
	seedAgentRaw(t, bPath, 1, "recipient", hashTestBearerToken("unused-recipient-token"))

	return aAddr, bAddr, aStore, bStore, aTransport
}

// sentMessage is POST /send's response shape (03-API.md: `202 {id,
// status: "pending"}`).
type sentMessage struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// sendMessage performs a real HTTP POST /send against addr (exercising
// A's real local API, not a direct app.SendMessage call — the point of
// this test is the full stack), asserts the 202/"pending" response shape
// 03-API.md documents, and returns the decoded response.
func sendMessage(t *testing.T, addr, token, to, subject, body string) sentMessage {
	t.Helper()

	payload, err := json.Marshal(map[string]string{
		"to":      to,
		"subject": subject,
		"body":    body,
	})
	if err != nil {
		t.Fatalf("marshaling /send request body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/send", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("building /send request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /send: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /send status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	var result sentMessage
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding /send response: %v", err)
	}
	if result.Status != "pending" {
		t.Fatalf(`/send response status = %q, want "pending"`, result.Status)
	}
	return result
}

// waitForMessage polls store for id until it reaches wantStatus (and, if
// wantAttempts >= 0, also reports exactly wantAttempts attempts), bounded
// by timeout — never a fixed sleep, per STATUS.md's Task 4 spec. Fails
// the test on timeout.
func waitForMessage(t *testing.T, store *sqlite.Store, id, wantStatus string, wantAttempts int, timeout time.Duration) *domain.Message {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last *domain.Message
	for time.Now().Before(deadline) {
		m, err := store.GetMessage(context.Background(), id)
		if err == nil {
			last = m
			if m.Status == wantStatus && (wantAttempts < 0 || m.Attempts == wantAttempts) {
				return m
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	if last != nil {
		t.Fatalf("message %s: status=%q attempts=%d after %s, want status=%q attempts=%d",
			id, last.Status, last.Attempts, timeout, wantStatus, wantAttempts)
	}
	t.Fatalf("message %s: not found within %s", id, timeout)
	return nil
}

// TestIntegrationScenario1_SuccessfulFederatedDelivery implements
// STATUS.md's Task 4 spec, Scenario 1: a normal message sent through A's
// real local HTTP API is delivered, via a real loopback HTTP POST, to B's
// real federation HTTP API, and observed on B's side as trust ==
// "verified" — a genuine, non-mocked end-to-end Ed25519 round trip across
// two independent per-domain signing keys.
func TestIntegrationScenario1_SuccessfulFederatedDelivery(t *testing.T) {
	t.Parallel()

	const senderToken = "scenario1-sender-token"
	aAddr, _, aStore, bStore, _ := setupFederationPair(t, shortTestRetrySchedule(), nil, senderToken)

	sent := sendMessage(t, aAddr, senderToken, "recipient@b.loopback.test", "hello", "hi there")

	// Correction reflected here per the spec: this does not land on the
	// worker's immediate first pass (that pass already ran, and found
	// nothing, during startTestInstance's setup, before this message ever
	// existed) — it waits for the worker's next real ~10s tick, same as
	// every other scenario. See "Known integration-test cost".
	waitForMessage(t, aStore, sent.ID, "delivered", -1, 20*time.Second)

	got, err := bStore.GetMessage(context.Background(), sent.ID)
	if err != nil {
		t.Fatalf("GetMessage on B: %v", err)
	}
	if got.ID != sent.ID {
		t.Errorf("B's copy id = %q, want %q (the same envelope id, round-tripped end to end)", got.ID, sent.ID)
	}
	if got.AgentID != 1 {
		t.Errorf("B's copy agent_id = %d, want 1 (the seeded recipient)", got.AgentID)
	}
	if got.To != "recipient@b.loopback.test" {
		t.Errorf("B's copy to = %q, want %q", got.To, "recipient@b.loopback.test")
	}
	if got.Subject != "hello" {
		t.Errorf("B's copy subject = %q, want %q", got.Subject, "hello")
	}
	if got.Body != "hi there" {
		t.Errorf("B's copy body = %q, want %q", got.Body, "hi there")
	}
	if got.Trust != "verified" {
		t.Errorf(`B's copy trust = %q, want "verified" (signature+kid present, B's own Directory.Resolve fetched A's real key, and Verify succeeded)`, got.Trust)
	}
}

// TestIntegrationScenario2_ForcedFailureRetry implements STATUS.md's Task
// 4 spec, Scenario 2: the first POST /deliver A's worker attempts is
// forced to fail (a synthetic 503, never forwarded to B at all); the
// worker's backoff logic (already verified in isolation by task 2's
// worker_test.go) retries on the next real tick and succeeds.
func TestIntegrationScenario2_ForcedFailureRetry(t *testing.T) {
	t.Parallel()

	var failOnce sync.Once
	onRequest := func(req *http.Request) (*http.Response, error, bool) {
		if req.URL.Path != "/deliver" {
			return nil, nil, false
		}
		fired := false
		failOnce.Do(func() { fired = true })
		if !fired {
			return nil, nil, false
		}
		return &http.Response{
			Status:     "503 Service Unavailable",
			StatusCode: http.StatusServiceUnavailable,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil, true
	}

	const senderToken = "scenario2-sender-token"
	aAddr, _, aStore, bStore, _ := setupFederationPair(t, shortTestRetrySchedule(), onRequest, senderToken)

	sent := sendMessage(t, aAddr, senderToken, "recipient@b.loopback.test", "hello2", "hi there 2")

	// First real tick after the message is sent: runOnce attempts
	// delivery, gets the injected 503, and MarkFailed schedules a retry —
	// status stays "pending" (not "failed": only schedule-exhaustion or
	// expiry produce a terminal "failed", per task 2's spec), attempts
	// becomes 1.
	waitForMessage(t, aStore, sent.ID, "pending", 1, 20*time.Second)

	if _, err := bStore.GetMessage(context.Background(), sent.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("message reached B before the forced failure ever happened (err=%v) — the forced 503 wasn't actually forced", err)
	}

	// Second real tick (after the short retry backoff elapses, so this
	// bound comfortably covers two ticks plus that backoff): the hook now
	// lets the request through, so delivery succeeds exactly as in
	// Scenario 1. attempts stays 1, not 2: store.go's MarkDelivered only
	// ever sets status='delivered' — attempts is incremented solely by
	// MarkFailed (once, on the first, forced failure), confirmed by
	// reading both methods directly. An earlier draft of this test
	// expected attempts==2 here, which store.go's already-verified
	// contract (task 2) makes impossible; corrected to match the real,
	// verified behavior rather than the earlier draft's assumption.
	waitForMessage(t, aStore, sent.ID, "delivered", 1, 30*time.Second)

	got, err := bStore.GetMessage(context.Background(), sent.ID)
	if err != nil {
		t.Fatalf("GetMessage on B: %v", err)
	}
	if got.Trust != "verified" {
		t.Errorf(`B's copy trust = %q, want "verified"`, got.Trust)
	}
}

// TestIntegrationScenario3_ExpiredMessage implements STATUS.md's Task 4
// spec, Scenario 3: an already-expired outbound message is claimed on a
// later runOnce pass, the expiry pre-check fires, and MarkFailed(...,
// "expired") is called with no Directory.Resolve/Client.Deliver attempt
// at all.
func TestIntegrationScenario3_ExpiredMessage(t *testing.T) {
	t.Parallel()

	const senderToken = "scenario3-sender-token"
	_, _, aStore, bStore, aTransport := setupFederationPair(t, shortTestRetrySchedule(), nil, senderToken)

	// 03-API.md's /send body has no expires_at input field today
	// (internal/app/send_message.go's SendMessageRequest deliberately
	// omits it — "expires_at is deliberately absent"), so this seeds the
	// outbound message directly via A's own store.SaveMessage, mirroring
	// the seedAgent-direct-store precedent, not a workaround HTTP call.
	sentAt := time.Now()
	expiresAt := sentAt.Add(2 * time.Second) // past well before the next real tick fires
	msg := &domain.Message{
		ID:           "msg_scenario3_expired",
		ThreadID:     "msg_scenario3_expired",
		AgentID:      1,
		Direction:    "out",
		From:         "sender@a.loopback.test",
		To:           "recipient@b.loopback.test",
		SenderDomain: "a.loopback.test",
		Subject:      "expiring",
		Body:         "this should expire before delivery",
		Priority:     "normal",
		SentAt:       sentAt,
		ExpiresAt:    &expiresAt,
		Trust:        "verified",
		Status:       "pending",
		Attempts:     0,
		NextAttempt:  &sentAt,
	}
	if err := aStore.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("seeding expired outbound message: %v", err)
	}

	// The worker's immediate first pass already ran (and found nothing)
	// before this message ever existed, so this waits for the next real
	// tick — same as every other scenario.
	waitForMessage(t, aStore, msg.ID, "failed", -1, 20*time.Second)

	if _, err := bStore.GetMessage(context.Background(), msg.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired message unexpectedly reached B (err=%v)", err)
	}
	if got := aTransport.requests.Load(); got != 0 {
		t.Errorf("A's delivery-side transport recorded %d request(s), want 0 (an already-expired message must never attempt Directory.Resolve or Client.Deliver)", got)
	}
}
