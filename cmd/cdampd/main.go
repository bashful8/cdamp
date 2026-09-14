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
	"cdamp/internal/adapters/web"
	"cdamp/internal/app"
	"cdamp/internal/config"
	"cdamp/internal/domain"

	httpadapter "cdamp/internal/adapters/http"
	mcpadapter "cdamp/internal/adapters/mcp"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests to drain before giving up. No value is specified in the design
// docs, so this is a reasonable default, not a load-bearing constant.
const shutdownTimeout = 5 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := runMCPMode(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "cdampd mcp: %v\n", err)
			os.Exit(1)
		}
		return
	}

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

	adminToken, created, err := app.BootstrapAdminCredential(context.Background(), store)
	if err != nil {
		return fmt.Errorf("bootstrapping admin credential: %w", err)
	}
	if created {
		fmt.Println("cdampd: admin bootstrap credential (printed once, never shown again):", adminToken)
	}

	verifier := signing.NewVerifier()

	// nil client -> NewHTTPDirectory's own documented default
	// (&http.Client{Timeout: defaultTimeout}).
	dir := directory.NewHTTPDirectory(nil, cfg.DirectoryCacheTTL)

	deliveryClient := delivery.NewClient(signer, nil)

	worker := delivery.NewWorker(store, dir, deliveryClient, cfg.RetrySchedule)

	limiters := httpadapter.NewDomainLimiters(float64(cfg.RateLimit.PerDomainRPS), cfg.RateLimit.Burst)

	// store satisfies domain.InboxStore, domain.SigningKeyStore, and
	// domain.BlocklistStore simultaneously - the same *sqlite.Store value
	// is passed three times below, no second store instance (same "one
	// concrete store, many narrow ports" pattern as the admin mux wiring
	// below).
	mux := newMux(store, store, dir, verifier, store, limiters, cfg)
	server := newServer(mux, cfg, logger)

	// The admin surface (POST/GET /admin/agents, POST/GET
	// /admin/blocklist, /dashboard/) is served on its own,
	// independently-bound http.Server — not a route group on the mux
	// above — since 02-ARCHITECTURE.md's Auth table says it's "bound to
	// localhost by default", only meaningful if it's reachable at a
	// genuinely different address than cfg.ListenAddr (which may be
	// 0.0.0.0-bound in production for federation traffic). store
	// satisfies domain.InboxStore, domain.AdminStore, and
	// domain.BlocklistStore simultaneously - the same *sqlite.Store
	// value passed three times below, same "one concrete store, many
	// narrow ports" pattern as above. dashboard is constructed
	// separately (internal/adapters/web.NewDashboardHandler) and passed
	// in as NewAdminMux's fourth argument so it can be mounted behind
	// the same admin-session auth without internal/adapters/web
	// importing internal/adapters/http (see STATUS.md's Phase 6 task 5
	// spec "Design decisions").
	dashboard := web.NewDashboardHandler(store, cfg)
	adminMux := httpadapter.NewAdminMux(store, store, store, dashboard, cfg)
	adminServer := &http.Server{
		Addr:    cfg.AdminBindAddr,
		Handler: loggingMiddleware(logger, adminMux),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go worker.Run(ctx)   // stops when ctx is canceled by the same shutdown signal
	go limiters.Run(ctx) // same shutdown-signal-driven lifecycle as the worker

	go func() {
		logger.Info("http server listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
		}
	}()

	go func() {
		logger.Info("admin http server listening", "addr", cfg.AdminBindAddr)
		if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin http server error", "error", err)
		}
	}()

	<-ctx.Done()
	stop()
	logger.Info("shutdown signal received, draining connections")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Both servers are shut down before store.Close(), regardless of
	// whether either individually fails — a failure on one must not skip
	// attempting the other, since each is an independent listener that
	// should be given its own chance to drain in-flight requests. Errors
	// from both are joined (errors.Join) rather than only reporting the
	// first, so a caller/log line can see if both failed simultaneously,
	// not just whichever happened to be checked first — a small builder
	// judgment call, not pinned by any doc.
	shutdownErr := server.Shutdown(shutdownCtx)
	adminShutdownErr := adminServer.Shutdown(shutdownCtx)
	if shutdownErr != nil || adminShutdownErr != nil {
		// Still attempt store.Close() below even on a Shutdown error, so
		// the database is never left open on a failed-shutdown path.
		closeErr := store.Close()
		return fmt.Errorf("shutting down http servers: %w (store close: %v)",
			errors.Join(
				wrapNamedErr("main server", shutdownErr),
				wrapNamedErr("admin server", adminShutdownErr),
			), closeErr)
	}

	if err := store.Close(); err != nil {
		return fmt.Errorf("closing sqlite store: %w", err)
	}

	logger.Info("server shut down cleanly")
	return nil
}

// runMCPMode parses the "mcp" subcommand's own flags and runs cdampd as
// an MCP server over stdio (03-API.md's MCP tool mapping; binary-layout
// decision in STATUS.md's "MCP library decision"). It never opens the
// SQLite store or touches cfg.ListenAddr/AdminBindAddr -- it only speaks
// to an already-running cdampd's local agent API over HTTP, exactly like
// any other bearer-token caller of that API.
func runMCPMode(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	apiBaseURL := fs.String("api-base-url", "http://127.0.0.1:8443", "base URL of the running cdampd instance's local agent API")
	token := fs.String("token", "", "this agent's bearer token (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		return fmt.Errorf("-token is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return mcpadapter.Run(ctx, *apiBaseURL, *token)
}

// wrapNamedErr wraps err with a name identifying which server it came
// from, or returns nil unchanged if err is nil — used so errors.Join
// below only aggregates the servers that actually failed to shut down,
// each labeled with which one it was.
func wrapNamedErr(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", name, err)
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
func newMux(store domain.InboxStore, keys domain.SigningKeyStore, dir domain.Directory, verifier domain.Verifier, blocklist domain.BlocklistStore, limiters *httpadapter.DomainLimiters, cfg *config.Config) *http.ServeMux {
	localHandler := httpadapter.NewLocalMux(store, cfg)
	federationHandler := httpadapter.NewFederationMux(store, keys, dir, verifier, blocklist, limiters, cfg)

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
