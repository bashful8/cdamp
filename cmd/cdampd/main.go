package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cdamp/internal/config"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests to drain before giving up. No value is specified in the design
// docs, so this is a reasonable default, not a load-bearing constant.
const shutdownTimeout = 5 * time.Second

func main() {
	configPath := flag.String("config", "cdampd.yaml", "path to the cdampd YAML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		// The logger isn't constructed yet at this point, so plain stderr
		// is correct here rather than inventing a fallback logger.
		fmt.Fprintf(os.Stderr, "cdampd: loading config: %v\n", err)
		os.Exit(1)
	}

	logger := config.NewLogger(os.Stdout, slog.LevelInfo)

	if err := run(cfg, logger); err != nil {
		logger.Error("cdampd exited with error", "error", err)
		os.Exit(1)
	}
}

// run wires the HTTP server for cfg and blocks until a shutdown signal
// (SIGINT/SIGTERM) is received, then drains connections via a bounded
// graceful shutdown. It is the composition root's main body, split out
// from main so it's callable/testable without invoking os.Exit.
func run(cfg *config.Config, logger *slog.Logger) error {
	server := newServer(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("http server listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
		}
	}()

	<-ctx.Done()
	stop()
	logger.Info("shutdown signal received, draining connections")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down server: %w", err)
	}

	logger.Info("server shut down cleanly")
	return nil
}

// newServer builds the HTTP server for cfg: a mux serving only /healthz,
// wrapped in the request-logging middleware.
func newServer(cfg *config.Config, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: loggingMiddleware(logger, newMux()),
	}
}

// newMux returns the HTTP routes cdampd currently serves. Only /healthz
// exists at this phase, per 04-BUILD-PLAN.md's Phase 0 scope.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	return mux
}

// healthzHandler always responds 200 with an empty body.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// statusRecorder wraps an http.ResponseWriter to capture the status code
// written, defaulting to 200 (net/http's own default when a handler never
// calls WriteHeader explicitly).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// loggingMiddleware logs method, path, status, and duration for every
// request handled by next, per the go-hexagonal-style skill's logging
// section and 04-BUILD-PLAN.md's Logging engineering practice.
func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	})
}
