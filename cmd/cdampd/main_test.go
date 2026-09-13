package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cdamp/internal/config"
	"cdamp/internal/domain/fakes"
)

// newTestMux builds a newMux with fresh fakes for every port, for tests
// that only care about the combined-mux routing behavior, not any
// particular adapter's real logic.
func newTestMux(cfg *config.Config) *http.ServeMux {
	return newMux(
		fakes.NewInboxStoreFake(),
		fakes.NewSigningKeyStoreFake(),
		fakes.NewDirectoryFake(),
		fakes.NewVerifierFake(),
		cfg,
	)
}

func TestHealthzHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewMuxServesHealthz(t *testing.T) {
	mux := newTestMux(&config.Config{Domain: "test.example"})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestNewMuxUnknownRouteNotFound confirms the federation mux is actually
// reachable through the combined top-level mux (not bypassed): an unknown
// agent name at the (unauthenticated) well-known route 404s, since a bare
// unmatched path elsewhere would instead hit the local catch-all's
// bearer-auth middleware first and 401 (see
// TestNewMuxUnauthenticatedLocalRouteReturns401 below), never reaching a
// routing-level 404.
func TestNewMuxUnknownRouteNotFound(t *testing.T) {
	mux := newTestMux(&config.Config{Domain: "test.example"})

	req := httptest.NewRequest(http.MethodGet, "/.well-known/cdamp/nonexistent", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /.well-known/cdamp/nonexistent status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestNewMuxUnauthenticatedLocalRouteReturns401 proves the local mux's
// bearer-auth middleware is actually reachable through the top-level mux
// (mounted at the "/" catch-all), not bypassed by the combination.
func TestNewMuxUnauthenticatedLocalRouteReturns401(t *testing.T) {
	mux := newTestMux(&config.Config{Domain: "test.example"})

	req := httptest.NewRequest(http.MethodGet, "/messages", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /messages status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestLoggingMiddlewareLogsMethodPathStatusDuration(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	handler := loggingMiddleware(logger, next)

	req := httptest.NewRequest(http.MethodGet, "/teapot", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("recorded status = %d, want %d", rec.Code, http.StatusTeapot)
	}

	logged := buf.String()
	for _, want := range []string{
		`"method":"GET"`,
		`"path":"/teapot"`,
		`"status":418`,
		`"duration"`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q; got: %s", want, logged)
		}
	}
}

func TestLoggingMiddlewareDefaultsToStatusOK(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	// Handler never calls WriteHeader explicitly, mirroring healthzHandler-
	// like handlers that only Write; net/http (and this middleware) should
	// treat that as an implicit 200.
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	handler := loggingMiddleware(logger, next)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("expected default status 200 logged; got: %s", buf.String())
	}
}

func TestNewServerUsesConfiguredAddr(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	cfg := &config.Config{ListenAddr: "127.0.0.1:0", Domain: "test.example"}
	mux := newTestMux(cfg)

	srv := newServer(mux, cfg, logger)
	if srv.Addr != "127.0.0.1:0" {
		t.Errorf("Addr = %q, want %q", srv.Addr, "127.0.0.1:0")
	}
	if srv.Handler == nil {
		t.Error("Handler is nil")
	}
}
