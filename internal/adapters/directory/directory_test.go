package directory

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cdamp/internal/domain"
)

// newTestDirectory returns an HTTPDirectory whose *http.Client routes
// every request to srv regardless of the request's own https://<domain>
// URL, via a custom RoundTripper — per the go-hexagonal-style skill, this
// keeps Resolve's own production code path exactly real https://, with no
// test-only insecure-http mode, while still letting the test hit a real
// httptest.Server.
func newTestDirectory(t *testing.T, srv *httptest.Server, ttl time.Duration) *HTTPDirectory {
	t.Helper()
	client := &http.Client{
		Transport: redirectTransport{target: srv.URL},
	}
	return NewHTTPDirectory(client, ttl)
}

// redirectTransport rewrites every outgoing request's scheme/host to
// target (an httptest.Server's URL) before delegating to the default
// transport, so tests can drive the adapter's real https:// request
// construction against a local server.
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

func mustPubkey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating test keypair: %v", err)
	}
	return pub
}

func TestResolve_Success(t *testing.T) {
	pub := mustPubkey(t)
	wantB64 := base64.StdEncoding.EncodeToString(pub)

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.URL.Path != "/.well-known/cdamp/alice" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wellKnownResponse{
			PublicKey: wantB64,
			KID:       "k1",
			InboxURL:  "https://example.dev/deliver",
		})
	}))
	defer srv.Close()

	d := newTestDirectory(t, srv, time.Hour)
	pubkey, kid, inboxURL, err := d.Resolve(context.Background(), "alice@example.dev")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if !pubkey.Equal(pub) {
		t.Errorf("pubkey = %x, want %x", pubkey, pub)
	}
	if kid != "k1" {
		t.Errorf("kid = %q, want %q", kid, "k1")
	}
	if inboxURL != "https://example.dev/deliver" {
		t.Errorf("inboxURL = %q, want %q", inboxURL, "https://example.dev/deliver")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestResolve_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d := newTestDirectory(t, srv, time.Hour)
	_, _, _, err := d.Resolve(context.Background(), "nobody@example.dev")
	if err == nil {
		t.Fatal("Resolve: expected error, got nil")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Resolve: error = %v, want errors.Is(err, domain.ErrNotFound)", err)
	}
}

func TestResolve_ServerError_NotErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := newTestDirectory(t, srv, time.Hour)
	_, _, _, err := d.Resolve(context.Background(), "alice@example.dev")
	if err == nil {
		t.Fatal("Resolve: expected error, got nil")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Resolve: error = %v, want NOT errors.Is(err, domain.ErrNotFound)", err)
	}
}

func TestResolve_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{not valid json"))
	}))
	defer srv.Close()

	d := newTestDirectory(t, srv, time.Hour)
	_, _, _, err := d.Resolve(context.Background(), "alice@example.dev")
	if err == nil {
		t.Fatal("Resolve: expected error, got nil")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Resolve: error = %v, want NOT errors.Is(err, domain.ErrNotFound)", err)
	}
}

func TestResolve_MalformedBase64Pubkey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wellKnownResponse{
			PublicKey: "not-valid-base64!!!",
			KID:       "k1",
			InboxURL:  "https://example.dev/deliver",
		})
	}))
	defer srv.Close()

	d := newTestDirectory(t, srv, time.Hour)
	_, _, _, err := d.Resolve(context.Background(), "alice@example.dev")
	if err == nil {
		t.Fatal("Resolve: expected error, got nil")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Resolve: error = %v, want NOT errors.Is(err, domain.ErrNotFound)", err)
	}
}

func TestResolve_MalformedAddress(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cases := []string{
		"no-at-sign.example.dev",
		"a@b@example.dev",
		"@example.dev",
		"alice@",
		"",
	}

	for _, address := range cases {
		t.Run(fmt.Sprintf("%q", address), func(t *testing.T) {
			d := newTestDirectory(t, srv, time.Hour)
			_, _, _, err := d.Resolve(context.Background(), address)
			if err == nil {
				t.Fatal("Resolve: expected error, got nil")
			}
		})
	}

	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Errorf("requests = %d, want 0 (guard must run before any HTTP call)", got)
	}
}

func TestResolve_CacheHit(t *testing.T) {
	pub := mustPubkey(t)
	wantB64 := base64.StdEncoding.EncodeToString(pub)

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wellKnownResponse{
			PublicKey: wantB64,
			KID:       "k1",
			InboxURL:  "https://example.dev/deliver",
		})
	}))
	defer srv.Close()

	d := newTestDirectory(t, srv, time.Hour)
	ctx := context.Background()

	if _, _, _, err := d.Resolve(ctx, "alice@example.dev"); err != nil {
		t.Fatalf("first Resolve: unexpected error: %v", err)
	}
	if _, _, _, err := d.Resolve(ctx, "alice@example.dev"); err != nil {
		t.Fatalf("second Resolve: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("requests = %d, want 1 (second call should hit cache)", got)
	}
}

func TestResolve_CacheExpiry(t *testing.T) {
	pub := mustPubkey(t)
	wantB64 := base64.StdEncoding.EncodeToString(pub)

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wellKnownResponse{
			PublicKey: wantB64,
			KID:       "k1",
			InboxURL:  "https://example.dev/deliver",
		})
	}))
	defer srv.Close()

	ttl := time.Hour
	d := newTestDirectory(t, srv, ttl)
	current := time.Now()
	d.now = func() time.Time { return current }

	ctx := context.Background()
	if _, _, _, err := d.Resolve(ctx, "alice@example.dev"); err != nil {
		t.Fatalf("first Resolve: unexpected error: %v", err)
	}
	if _, _, _, err := d.Resolve(ctx, "alice@example.dev"); err != nil {
		t.Fatalf("second Resolve: unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("requests after two calls within TTL = %d, want 1", got)
	}

	// Advance the injected clock past the TTL: a third call must hit the
	// server again rather than replaying the now-expired cache entry.
	current = current.Add(ttl + time.Second)
	if _, _, _, err := d.Resolve(ctx, "alice@example.dev"); err != nil {
		t.Fatalf("third Resolve: unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("requests after TTL expiry = %d, want 2", got)
	}
}

func TestResolve_NegativeResultsNotCached(t *testing.T) {
	pub := mustPubkey(t)
	wantB64 := base64.StdEncoding.EncodeToString(pub)

	var requests int32
	var found atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if !found.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wellKnownResponse{
			PublicKey: wantB64,
			KID:       "k1",
			InboxURL:  "https://example.dev/deliver",
		})
	}))
	defer srv.Close()

	d := newTestDirectory(t, srv, time.Hour)
	ctx := context.Background()

	_, _, _, err := d.Resolve(ctx, "alice@example.dev")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("first Resolve: error = %v, want errors.Is(err, domain.ErrNotFound)", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("requests after first (404) call = %d, want 1", got)
	}

	// Simulate the agent now existing: the 404 must not have been cached.
	found.Store(true)
	_, _, _, err = d.Resolve(ctx, "alice@example.dev")
	if err != nil {
		t.Fatalf("second Resolve: unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("requests after second call = %d, want 2 (404 must not be cached)", got)
	}
}
