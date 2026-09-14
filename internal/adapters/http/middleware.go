package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"cdamp/internal/app"
	"cdamp/internal/domain"
)

// contextKey is an unexported type for context.WithValue keys used by this
// package, per Go's standard advice to avoid collisions with keys defined
// in other packages — no framework, just a small private type.
type contextKey int

// agentContextKey is the key bearerAuthMiddleware stores the resolved
// *domain.Agent under, for handlers to read via agentFromContext.
const agentContextKey contextKey = iota

// agentFromContext returns the *domain.Agent bearerAuthMiddleware resolved
// for this request, or nil if none was ever set (which should never happen
// for a handler reached through NewLocalMux, since bearerAuthMiddleware
// runs before every route and refuses the request otherwise).
func agentFromContext(ctx context.Context) *domain.Agent {
	agent, _ := ctx.Value(agentContextKey).(*domain.Agent)
	return agent
}

// bearerPrefix is the only Authorization scheme this API accepts, per
// 03-API.md's "Authorization: Bearer <token>" local-agent-API convention.
// Matched case-sensitively, per the scheme's conventional spelling — a
// differently-cased or different scheme is treated as malformed, not
// normalized.
const bearerPrefix = "Bearer "

// bearerAuthMiddleware implements 03-API.md's local-agent-API auth and
// 02-ARCHITECTURE.md's "Enforced in middleware before the request reaches
// any use case" rule: it reads the Authorization header, requires exactly
// the "Bearer <token>" scheme, hashes the raw token (crypto/sha256,
// hex-encoded — see hashBearerToken) and resolves it to a domain.Agent via
// store.FindAgentByTokenHash (added per STATUS.md's "Agent-lookup decision
// (human-resolved, 2026-09-12)"). A missing header, a wrong/malformed
// scheme, or a token that doesn't resolve to any agent all map to the same
// `401 {"error":{"code":"unauthorized",...}}` shape, per the error-mapping
// table in STATUS.md's task 2 spec — deliberately not distinguishing these
// cases in the response, so a caller can't probe which tokens exist.
//
// On success, the resolved *domain.Agent is stored on the request context
// (agentContextKey) for downstream handlers, and the request proceeds.
func bearerAuthMiddleware(store domain.InboxStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if header == "" || !strings.HasPrefix(header, bearerPrefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed Authorization header")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}

		agent, err := store.FindAgentByTokenHash(r.Context(), hashBearerToken(token))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}

		ctx := context.WithValue(r.Context(), agentContextKey, agent)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// hashBearerToken hashes a raw bearer token into the deterministic,
// exact-match-lookupable form domain.InboxStore.FindAgentByTokenHash (and,
// eventually, Phase 6's CreateAgent, which must write agents.token_hash the
// same way) expect: plain SHA-256, hex-encoded. See STATUS.md's
// "Agent-lookup decision (human-resolved, 2026-09-12)" for why a
// deterministic hash was chosen over a salted KDF here — bearer tokens are
// high-entropy random strings, not passwords, so a fast hash isn't a
// brute-force risk, and FindAgentByTokenHash needs an exact-match SQL
// lookup that a per-call salt would make impossible.
func hashBearerToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sizeLimitMiddleware caps every request body handled by next at
// app.MaxBodyBytes (256KB), per STATUS.md's task 2 spec, by wrapping the
// body in http.MaxBytesReader. This only makes a Read past the limit fail
// (with a *http.MaxBytesError) — it does not itself write a response, so
// callers reading the body (local.go's handlers) are responsible for
// checking for that error and mapping it to
// `400 {"error":{"code":"body_too_large",...}}` per the error-mapping
// table.
//
// Overlap with app.SendMessage's own body-size check (deliberate, not
// dead code): app.SendMessage independently rejects a `body` field over
// MaxBodyBytes with domain.ErrValidation (mapped to a *different* code,
// `bad_request` — see local.go's mapDomainError). Since this middleware
// already caps the entire request body — envelope and all — at
// MaxBodyBytes before JSON decoding ever runs, a real HTTP client can never
// actually trigger SendMessage's own body-size branch (the `body` JSON
// field alone can't exceed 256KB once the whole request already couldn't).
// SendMessage's check remains valuable defense-in-depth for any non-HTTP
// caller of the use case directly (e.g. a future in-process caller that
// bypasses this middleware entirely), which is why it wasn't removed when
// this middleware was added.
func sizeLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, app.MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// errorBody is the JSON shape of every error response, per 03-API.md's
// "Error shape (all endpoints)" section:
// {"error": {"code": "...", "message": "..."}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError writes an errorBody with the given HTTP status, code, and
// human-readable message.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Code: code, Message: message}})
}

// writeJSON writes v as a JSON response body with the given HTTP status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// domainLimiterTTL bounds how long an idle per-domain rate limiter is
// kept before a cleanup sweep evicts it. See STATUS.md's Phase 8 task 2
// spec, design decision 6, for why this exists and why these two
// constants (not pinned by any doc) were chosen at these values.
const domainLimiterTTL = 10 * time.Minute

// domainLimiterSweepInterval is how often the background cleanup
// goroutine (Run) scans for idle entries to evict.
const domainLimiterSweepInterval = time.Minute

// DomainLimiters is a concurrency-safe registry of one
// golang.org/x/time/rate.Limiter per sender domain, implementing
// 01-PROTOCOL.md/03-API.md's per-domain token bucket on POST /deliver
// (429 rate_limited). Exported so cmd/cdampd/main.go can construct one
// at startup and run its cleanup loop (Run) the same way it already
// runs delivery.Worker.Run.
type DomainLimiters struct {
	mu       sync.Mutex
	rps      float64
	burst    int
	limiters map[string]*limiterEntry
	now      func() time.Time // overridable in this package's own tests; always time.Now outside tests
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewDomainLimiters returns a DomainLimiters using rps/burst already
// resolved from config (cfg.RateLimit.PerDomainRPS/.Burst) by the
// caller, matching every other adapter constructor in this codebase
// that takes specific resolved fields rather than *config.Config
// itself (e.g. delivery.NewWorker's cfg.RetrySchedule parameter).
func NewDomainLimiters(rps float64, burst int) *DomainLimiters {
	return &DomainLimiters{
		rps:      rps,
		burst:    burst,
		limiters: make(map[string]*limiterEntry),
		now:      time.Now,
	}
}

// Allow reports whether a request from domainName is within its
// per-domain token bucket, lazily creating a fresh limiter (starting
// full, at burst capacity) the first time a domain is seen.
func (d *DomainLimiters) Allow(domainName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.limiters[domainName]
	if !ok {
		e = &limiterEntry{limiter: rate.NewLimiter(rate.Limit(d.rps), d.burst)}
		d.limiters[domainName] = e
	}
	e.lastSeen = d.now()
	return e.limiter.Allow()
}

// sweep evicts every tracked domain whose lastSeen is older than
// domainLimiterTTL. Unexported, but this package's own tests call it
// directly with d.now stubbed, so eviction is tested deterministically
// without waiting on Run's real ticker.
func (d *DomainLimiters) sweep() {
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := d.now().Add(-domainLimiterTTL)
	for k, e := range d.limiters {
		if e.lastSeen.Before(cutoff) {
			delete(d.limiters, k)
		}
	}
}

// Run ticks every domainLimiterSweepInterval calling sweep, until ctx
// is canceled -- same lifecycle shape as delivery.Worker.Run; main.go
// starts it the same way (go limiters.Run(ctx)) alongside the existing
// worker, stopping on the same shutdown signal.
func (d *DomainLimiters) Run(ctx context.Context) {
	ticker := time.NewTicker(domainLimiterSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.sweep()
		}
	}
}
