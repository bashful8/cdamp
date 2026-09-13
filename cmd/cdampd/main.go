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

	"cdamp/internal/adapters/delivery"
	"cdamp/internal/adapters/directory"
	"cdamp/internal/adapters/signing"
	"cdamp/internal/adapters/storage/sqlite"
	"cdamp/internal/config"
	"cdamp/internal/domain"

	httpadapter "cdamp/internal/adapters/http"
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

// run is the composition root: it opens the SQLite store, bootstraps
// signing, wires the directory/delivery adapters and the outbound worker,
// builds the combined local-API + federation HTTP mux on cfg.ListenAddr
// (03-API.md's Config section defines exactly one listen_addr — there is
// no separate federation port), and blocks until a shutdown signal
// (SIGINT/SIGTERM) is received, then drains connections via a bounded
// graceful shutdown before closing the store. It is split out from main
// so it's callable/testable without invoking os.Exit.
func run(cfg *config.Config, logger *slog.Logger) error {
	store, err := sqlite.Open(cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("opening sqlite store: %w", err)
	}

	// context.Background() here (not the shutdown ctx below) since this is
	// one-time startup work, same category as config.Load itself never
	// taking a context.
	signer, err := signing.NewSigner(context.Background(), store, cfg.SigningKeyPassphrase)
	if err != nil {
		return fmt.Errorf("initializing signer: %w", err)
	}

	verifier := signing.NewVerifier()

	// nil client -> NewHTTPDirectory's own documented default
	// (&http.Client{Timeout: defaultTimeout}).
	dir := directory.NewHTTPDirectory(nil, cfg.DirectoryCacheTTL)

	deliveryClient := delivery.NewClient(signer)

	worker := delivery.NewWorker(store, dir, deliveryClient, cfg.RetrySchedule)

	// store satisfies both domain.InboxStore and domain.SigningKeyStore
	// simultaneously - the same *sqlite.Store value is passed twice below,
	// no second store instance.
	mux := newMux(store, store, dir, verifier, cfg)
	server := newServer(mux, cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go worker.Run(ctx) // stops when ctx is canceled by the same shutdown signal

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
		// Still attempt store.Close() below even on a Shutdown error, so
		// the database is never left open on a failed-shutdown path.
		closeErr := store.Close()
		return fmt.Errorf("shutting down server: %w (store close: %v)", err, closeErr)
	}

	if err := store.Close(); err != nil {
		return fmt.Errorf("closing sqlite store: %w", err)
	}

	logger.Info("server shut down cleanly")
	return nil
}

// newServer builds the HTTP server for cfg, serving mux under the
// request-logging middleware.
func newServer(mux *http.ServeMux, cfg *config.Config, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: loggingMiddleware(logger, mux),
	}
}

// newMux returns cdampd's combined top-level HTTP mux: /healthz, the local
// agent API (local.go's six bearer-auth-gated routes, mounted at the
// catch-all "/"), and the federation surface (federation.go's three
// unauthenticated routes, mounted at their own exact path roots so Go
// 1.22+'s longest-pattern-wins rule routes them correctly ahead of the
// catch-all). Both httpadapter.NewLocalMux and httpadapter.NewFederationMux
// return fully self-contained, middleware-wrapped http.Handlers that
// re-match the full request path internally, so no http.StripPrefix is
// needed here.
func newMux(store domain.InboxStore, keys domain.SigningKeyStore, dir domain.Directory, verifier domain.Verifier, cfg *config.Config) *http.ServeMux {
	localHandler := httpadapter.NewLocalMux(store, cfg)
	federationHandler := httpadapter.NewFederationMux(store, keys, dir, verifier, cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.Handle("/.well-known/", federationHandler) // catches /.well-known/cdamp/{agent} and /.well-known/cdamp/keys
	mux.Handle("/deliver", federationHandler)
	mux.Handle("/", localHandler) // catch-all: /send, /messages, /messages/{id}, /threads, /threads/{id}, /agents/me
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
