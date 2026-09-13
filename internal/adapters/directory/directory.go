// Package directory implements internal/domain's Directory port: it
// resolves an agent address to its public key, key id, and inbox URL via
// well-known lookup, caching results per 01-PROTOCOL.md's TTL.
package directory

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cdamp/internal/domain"
)

// defaultTimeout is the adapter's own *http.Client fallback Timeout,
// bounding a single well-known lookup end-to-end (connect + TLS + read).
// Not pinned by any doc — a judgment call, same category as keys.go's
// scrypt cost parameters — chosen generously enough to tolerate a slow
// remote instance while still failing well before an operator would
// suspect the daemon has hung. The caller's own context
// (http.NewRequestWithContext) still governs cancellation/deadline on top
// of this.
const defaultTimeout = 10 * time.Second

// wellKnownResponse mirrors the JSON body documented in 01-PROTOCOL.md's
// Discovery section and 03-API.md's federation surface table:
// {"public_key": "<base64>", "kid": "k1", "inbox_url": "https://..."}.
type wellKnownResponse struct {
	PublicKey string `json:"public_key"`
	KID       string `json:"kid"`
	InboxURL  string `json:"inbox_url"`
}

// cacheEntry is one address's cached successful resolution, plus the time
// it was fetched (used to compute expiry against the injectable now).
type cacheEntry struct {
	pubkey    ed25519.PublicKey
	kid       string
	inboxURL  string
	fetchedAt time.Time
}

// HTTPDirectory implements domain.Directory via HTTP well-known lookups
// against a remote instance's /.well-known/cdamp/{agent} endpoint, with an
// in-memory TTL cache so a delivery doesn't do a discovery round-trip per
// message (01-PROTOCOL.md's Discovery section).
type HTTPDirectory struct {
	client *http.Client
	ttl    time.Duration

	mu      sync.Mutex
	entries map[string]cacheEntry

	// now is an injectable time source, overridable only from this
	// package's own tests, so TTL expiry can be exercised without a real
	// sleep. Defaults to time.Now in NewHTTPDirectory.
	now func() time.Time
}

// NewHTTPDirectory returns an HTTPDirectory that issues lookups with
// client and caches successful resolutions for ttl. If client is nil, a
// new *http.Client with defaultTimeout is used; a non-nil client is used
// as given (its Timeout, if any, is the caller's responsibility — this
// lets tests point the client at an httptest.Server via a custom
// Transport while keeping Resolve's own request construction unchanged).
func NewHTTPDirectory(client *http.Client, ttl time.Duration) *HTTPDirectory {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &HTTPDirectory{
		client:  client,
		ttl:     ttl,
		entries: map[string]cacheEntry{},
		now:     time.Now,
	}
}

// Resolve implements domain.Directory. It splits address into an
// agent-name and domain, checks the TTL cache, and otherwise performs a
// single GET https://<domain>/.well-known/cdamp/<agent-name> request.
//
// Error handling (per STATUS.md's Task 2 spec, matching the existing
// domain/fakes.DirectoryFake contract): a 404 response returns a wrapped
// domain.ErrNotFound. Any other failure — non-2xx/non-404 status,
// transport/network error, or a malformed response body — returns a plain
// wrapped error, never domain.ErrNotFound, since only a confirmed-absent
// lookup (404) has established that the address doesn't exist.
func (d *HTTPDirectory) Resolve(ctx context.Context, address string) (ed25519.PublicKey, string, string, error) {
	agentName, domainName, err := splitAddress(address)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve %q: %w", address, err)
	}

	if entry, ok := d.cached(address); ok {
		return entry.pubkey, entry.kid, entry.inboxURL, nil
	}

	pubkey, kid, inboxURL, err := d.fetch(ctx, agentName, domainName)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve %q: %w", address, err)
	}

	d.store(address, pubkey, kid, inboxURL)
	return pubkey, kid, inboxURL, nil
}

// splitAddress parses address into its agent-name and domain per
// 01-PROTOCOL.md's Addressing section (exactly one "@", no tenant
// segment). This is a defensive adapter-boundary guard, not new
// validation logic — SendMessage already validates `to` shape before a
// message is queued — so it just keeps Resolve from building a
// nonsensical URL if it's ever called with something malformed.
func splitAddress(address string) (agentName, domainName string, err error) {
	parts := strings.Split(address, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("address must have exactly one \"@\" separating agent-name and domain")
	}
	return parts[0], parts[1], nil
}

// cached returns the cache entry for address, if present and not expired
// per d.ttl and d.now.
func (d *HTTPDirectory) cached(address string) (cacheEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entry, ok := d.entries[address]
	if !ok {
		return cacheEntry{}, false
	}
	if d.now().Sub(entry.fetchedAt) >= d.ttl {
		return cacheEntry{}, false
	}
	return entry, true
}

// store records a successful resolution for address in the cache.
func (d *HTTPDirectory) store(address string, pubkey ed25519.PublicKey, kid, inboxURL string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[address] = cacheEntry{
		pubkey:    pubkey,
		kid:       kid,
		inboxURL:  inboxURL,
		fetchedAt: d.now(),
	}
}

// fetch performs the single HTTP well-known lookup and parses its
// response. It never caches — Resolve does that, and only for the success
// path — so a 404 or any other failure here is never remembered: an agent
// that doesn't exist yet may exist a minute later, and caching a negative
// result for a full hour would make that needlessly slow to recover from
// (a builder judgment call per STATUS.md's Task 2 spec: "caches this
// response" in 01-PROTOCOL.md reads most naturally as caching the thing
// that was successfully resolved).
func (d *HTTPDirectory) fetch(ctx context.Context, agentName, domainName string) (ed25519.PublicKey, string, string, error) {
	reqURL := fmt.Sprintf("https://%s/.well-known/cdamp/%s", domainName, url.PathEscape(agentName))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("building request: %w", err)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", "", fmt.Errorf("%w", domain.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var body wellKnownResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", "", fmt.Errorf("decoding response body: %w", err)
	}

	rawKey, err := base64.StdEncoding.DecodeString(body.PublicKey)
	if err != nil {
		return nil, "", "", fmt.Errorf("decoding public_key: %w", err)
	}

	return ed25519.PublicKey(rawKey), body.KID, body.InboxURL, nil
}
