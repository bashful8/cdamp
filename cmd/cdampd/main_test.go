package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cdamp/internal/config"
)

func TestHealthzHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewMuxServesHealthz(t *testing.T) {
	mux := newMux()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewMuxUnknownRouteNotFound(t *testing.T) {
	mux := newMux()

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope status = %d, want %d", rec.Code, http.StatusNotFound)
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
	cfg := &config.Config{ListenAddr: "127.0.0.1:0"}

	srv := newServer(cfg, logger)
	if srv.Addr != "127.0.0.1:0" {
		t.Errorf("Addr = %q, want %q", srv.Addr, "127.0.0.1:0")
	}
	if srv.Handler == nil {
		t.Error("Handler is nil")
	}
}
