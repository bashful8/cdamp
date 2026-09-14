package http

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// okHandler is a stand-in "next" handler that reports 200 and, if an agent
// was resolved onto the request context, echoes its ID back so tests can
// confirm bearerAuthMiddleware actually stored it.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent := agentFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
		if agent != nil {
			_, _ = w.Write([]byte(agent.Name))
		}
	})
}

func seedTokenAgent(t *testing.T, store *fakes.InboxStoreFake, id int64, name, rawToken string) {
	t.Helper()
	store.AddAgent(&domain.Agent{ID: id, Name: name, TokenHash: hashBearerToken(rawToken)})
}

func TestBearerAuthMiddlewareMissingHeader(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	handler := bearerAuthMiddleware(store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/agents/me", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestBearerAuthMiddlewareMalformedScheme(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedTokenAgent(t, store, 1, "alice", "good-token")
	handler := bearerAuthMiddleware(store, okHandler())

	tests := []string{
		"Basic good-token",
		"bearer good-token", // wrong case
		"BearerXgood-token", // no separating space
		"Bearer",            // no token at all
		"Bearer ",           // empty token
	}
	for _, authHeader := range tests {
		t.Run(authHeader, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/agents/me", nil)
			req.Header.Set("Authorization", authHeader)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
		})
	}
}

func TestBearerAuthMiddlewareUnknownToken(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedTokenAgent(t, store, 1, "alice", "good-token")
	handler := bearerAuthMiddleware(store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/agents/me", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestBearerAuthMiddlewareValidTokenResolvesAgent(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	seedTokenAgent(t, store, 1, "alice", "good-token")
	handler := bearerAuthMiddleware(store, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/agents/me", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "alice" {
		t.Fatalf("next handler saw agent name %q, want alice (context propagation failed)", got)
	}
}

func TestHashBearerTokenIsSHA256Hex(t *testing.T) {
	sum := sha256.Sum256([]byte("some-token"))
	want := hex.EncodeToString(sum[:])
	if got := hashBearerToken("some-token"); got != want {
		t.Fatalf("hashBearerToken = %q, want %q", got, want)
	}
}

func TestSizeLimitMiddlewareRejectsOversizedBody(t *testing.T) {
	// sizeLimitMiddleware itself only wraps the body reader — the actual
	// 400 body_too_large mapping happens where the body is read (in
	// decodeJSONBody, exercised via handleSend in local_test.go). This
	// test confirms the wrapped reader does fail past app.MaxBodyBytes.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			t.Fatalf("expected a MaxBytesReader error reading an oversized body, got nil")
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := sizeLimitMiddleware(next)

	oversized := strings.Repeat("a", app.MaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(oversized))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
}

func TestSizeLimitMiddlewareAllowsBodyAtLimit(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading body at the limit: %v", err)
		}
		if len(b) != app.MaxBodyBytes {
			t.Fatalf("read %d bytes, want %d", len(b), app.MaxBodyBytes)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := sizeLimitMiddleware(next)

	atLimit := strings.Repeat("a", app.MaxBodyBytes)
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(atLimit))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestDomainLimiters_AllowsUnderLimit(t *testing.T) {
	d := NewDomainLimiters(1000, 1000)
	if !d.Allow("a.dev") {
		t.Fatalf("Allow(%q) = false, want true under a generous rate/burst", "a.dev")
	}
}

func TestDomainLimiters_BlocksOverLimit(t *testing.T) {
	// rps=0, burst=1: no refill at all, so exactly one token ever exists
	// -- deterministic without any time.Sleep/real-clock wait.
	d := NewDomainLimiters(0, 1)
	if !d.Allow("a.dev") {
		t.Fatalf("first Allow(%q) = false, want true", "a.dev")
	}
	if d.Allow("a.dev") {
		t.Fatalf("second Allow(%q) = true, want false (bucket should be exhausted)", "a.dev")
	}
}

func TestDomainLimiters_DomainsAreIndependent(t *testing.T) {
	d := NewDomainLimiters(0, 1)
	if !d.Allow("a.dev") {
		t.Fatalf("Allow(%q) = false, want true", "a.dev")
	}
	if d.Allow("a.dev") {
		t.Fatalf("second Allow(%q) = true, want false (bucket should be exhausted)", "a.dev")
	}
	if !d.Allow("b.dev") {
		t.Fatalf("Allow(%q) = false, want true (independent bucket from a.dev)", "b.dev")
	}
}

func TestDomainLimiters_SweepEvictsOnlyIdleEntries(t *testing.T) {
	d := NewDomainLimiters(1000, 1000)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	d.now = func() time.Time { return now }

	d.Allow("idle.dev")
	d.Allow("active.dev")

	// Refresh active.dev's lastSeen partway through the TTL window.
	now = t0.Add(domainLimiterTTL / 2)
	d.Allow("active.dev")

	// Advance past the TTL for idle.dev (whose lastSeen is still t0), but
	// still within the TTL of active.dev's refreshed lastSeen.
	now = t0.Add(domainLimiterTTL + time.Second)
	d.sweep()

	d.mu.Lock()
	_, idleStillPresent := d.limiters["idle.dev"]
	_, activeStillPresent := d.limiters["active.dev"]
	remaining := len(d.limiters)
	d.mu.Unlock()

	if idleStillPresent {
		t.Errorf("idle.dev entry still present after sweep, want evicted")
	}
	if !activeStillPresent {
		t.Errorf("active.dev entry evicted after sweep, want retained (refreshed lastSeen)")
	}
	if remaining != 1 {
		t.Errorf("limiters map has %d entries after sweep, want 1", remaining)
	}
}

// assertErrorResponse checks rec's status code and errorBody.Error.Code
// against wantStatus/wantCode, per 03-API.md's error shape.
func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error response: %v (body: %s)", err, rec.Body.String())
	}
	if body.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q", body.Error.Code, wantCode)
	}
}
