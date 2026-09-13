package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cdamp/internal/app"
	"cdamp/internal/config"
	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

// newTestAdminMux builds a fresh NewAdminMux over fake stores, with the
// admin bootstrap credential already saved as adminPlaintext (so
// adminCookie below authenticates against it).
const adminPlaintext = "admin-bootstrap-secret"

func newTestAdminMux(t *testing.T) (http.Handler, *fakes.InboxStoreFake, *fakes.AdminStoreFake, *fakes.BlocklistStoreFake, *config.Config) {
	t.Helper()
	inbox := fakes.NewInboxStoreFake()
	admin := fakes.NewAdminStoreFake()
	blocklist := fakes.NewBlocklistStoreFake()
	if err := admin.SaveAdminCredential(context.Background(), &domain.AdminCredential{
		TokenHash: hashBearerToken(adminPlaintext),
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seeding admin credential: %v", err)
	}
	cfg := &config.Config{Domain: "example.dev"}
	return NewAdminMux(inbox, admin, blocklist, cfg), inbox, admin, blocklist, cfg
}

// doAdminRequest issues a request against mux, attaching the admin cookie
// (adminCookieName=cookieValue) unless cookieValue is empty.
func doAdminRequest(t *testing.T, mux http.Handler, method, target, cookieValue string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if cookieValue != "" {
		r.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookieValue})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

// TestAdminMuxCreateAgentHappyPath covers POST /admin/agents' 201 response
// shape and confirms the returned token's independently-recomputed
// SHA-256/hex digest matches the fake's newly stored TokenHash — the same
// independent-recomputation discipline as every prior token-handling test
// in this codebase (never calling hashBearerToken itself to check its own
// output).
func TestAdminMuxCreateAgentHappyPath(t *testing.T) {
	mux, inbox, _, _, _ := newTestAdminMux(t)

	body, _ := json.Marshal(map[string]string{"name": "alice"})
	rec := doAdminRequest(t, mux, http.MethodPost, "/admin/agents", adminPlaintext, body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Address != "alice@example.dev" {
		t.Errorf("address = %q, want alice@example.dev", resp.Address)
	}
	if resp.Token == "" {
		t.Fatalf("token is empty")
	}

	sum := sha256.Sum256([]byte(resp.Token))
	wantHash := hex.EncodeToString(sum[:])

	stored, err := inbox.FindAgentByName(context.Background(), "alice")
	if err != nil {
		t.Fatalf("FindAgentByName: %v", err)
	}
	if stored.TokenHash != wantHash {
		t.Fatalf("stored TokenHash = %q, want %q (independently recomputed SHA-256 of returned token)", stored.TokenHash, wantHash)
	}
}

func TestAdminMuxCreateAgentDuplicateNameConflict(t *testing.T) {
	mux, inbox, _, _, _ := newTestAdminMux(t)
	inbox.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "existing-hash"})

	body, _ := json.Marshal(map[string]string{"name": "alice"})
	rec := doAdminRequest(t, mux, http.MethodPost, "/admin/agents", adminPlaintext, body)
	assertErrorResponse(t, rec, http.StatusConflict, "conflict")
}

func TestAdminMuxListAgents(t *testing.T) {
	mux, inbox, _, _, _ := newTestAdminMux(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inbox.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1", CreatedAt: createdAt})
	inbox.AddAgent(&domain.Agent{ID: 2, Name: "bob", TokenHash: "hash-2", CreatedAt: createdAt})

	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/agents", adminPlaintext, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Agents []struct {
			Address   string    `json:"address"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("agents = %+v, want 2 entries", resp.Agents)
	}
	wantAddrs := map[string]bool{"alice@example.dev": false, "bob@example.dev": false}
	for _, a := range resp.Agents {
		if _, ok := wantAddrs[a.Address]; !ok {
			t.Errorf("unexpected address %q", a.Address)
			continue
		}
		wantAddrs[a.Address] = true
		if !a.CreatedAt.Equal(createdAt) {
			t.Errorf("CreatedAt for %q = %v, want %v", a.Address, a.CreatedAt, createdAt)
		}
	}
	for addr, seen := range wantAddrs {
		if !seen {
			t.Errorf("expected address %q not present in response", addr)
		}
	}
}

func TestAdminMuxListAgentsEmpty(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)

	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/agents", adminPlaintext, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Agents []any `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Agents == nil {
		t.Fatalf(`agents = null, want [] (body: %s)`, rec.Body.String())
	}
	if len(resp.Agents) != 0 {
		t.Fatalf("agents = %v, want empty", resp.Agents)
	}
}

func TestAdminMuxMissingCookieRejected(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)
	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/agents", "", nil)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestAdminMuxWrongCookieRejected(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)
	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/agents", "not-the-right-value", nil)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestAdminMuxNoCredentialBootstrappedRejected(t *testing.T) {
	inbox := fakes.NewInboxStoreFake()
	admin := fakes.NewAdminStoreFake() // no SaveAdminCredential call: GetAdminCredential returns ErrNotFound
	blocklist := fakes.NewBlocklistStoreFake()
	cfg := &config.Config{Domain: "example.dev"}
	mux := NewAdminMux(inbox, admin, blocklist, cfg)

	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/agents", "anything", nil)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestAdminMuxCreateAgentOversizedBodyRejected(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)

	oversized := []byte(`{"name":"` + strings.Repeat("a", app.MaxBodyBytes+1) + `"}`)
	rec := doAdminRequest(t, mux, http.MethodPost, "/admin/agents", adminPlaintext, oversized)
	assertErrorResponse(t, rec, http.StatusBadRequest, "body_too_large")
}

func TestAdminMuxCreateBlocklistEntryHappyPath(t *testing.T) {
	mux, _, _, blocklist, _ := newTestAdminMux(t)

	body, _ := json.Marshal(map[string]string{"domain": "spam.example", "reason": "spam"})
	rec := doAdminRequest(t, mux, http.MethodPost, "/admin/blocklist", adminPlaintext, body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Domain  string    `json:"domain"`
		Reason  string    `json:"reason"`
		AddedAt time.Time `json:"added_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Domain != "spam.example" {
		t.Errorf("domain = %q, want spam.example", resp.Domain)
	}
	if resp.Reason != "spam" {
		t.Errorf("reason = %q, want spam", resp.Reason)
	}
	if resp.AddedAt.IsZero() {
		t.Errorf("added_at is zero")
	}

	entries, err := blocklist.ListBlocklist(context.Background())
	if err != nil {
		t.Fatalf("ListBlocklist: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stored entries = %d, want 1", len(entries))
	}
	if entries[0].Domain != "spam.example" || entries[0].Reason != "spam" {
		t.Errorf("stored entry = %+v, want {Domain: spam.example, Reason: spam}", entries[0])
	}
}

func TestAdminMuxCreateBlocklistEntryDuplicateDomainConflict(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)

	body, _ := json.Marshal(map[string]string{"domain": "spam.example", "reason": "spam"})
	rec := doAdminRequest(t, mux, http.MethodPost, "/admin/blocklist", adminPlaintext, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first insert status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	rec = doAdminRequest(t, mux, http.MethodPost, "/admin/blocklist", adminPlaintext, body)
	assertErrorResponse(t, rec, http.StatusConflict, "conflict")
}

func TestAdminMuxListBlocklist(t *testing.T) {
	mux, _, _, blocklist, _ := newTestAdminMux(t)
	addedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := blocklist.SaveBlocklistEntry(context.Background(), &domain.BlocklistEntry{
		Domain: "a.example", Reason: "reason-a", AddedAt: addedAt,
	}); err != nil {
		t.Fatalf("SaveBlocklistEntry: %v", err)
	}
	if err := blocklist.SaveBlocklistEntry(context.Background(), &domain.BlocklistEntry{
		Domain: "b.example", Reason: "reason-b", AddedAt: addedAt,
	}); err != nil {
		t.Fatalf("SaveBlocklistEntry: %v", err)
	}

	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/blocklist", adminPlaintext, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Domains []struct {
			Domain  string    `json:"domain"`
			Reason  string    `json:"reason"`
			AddedAt time.Time `json:"added_at"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Domains) != 2 {
		t.Fatalf("domains = %+v, want 2 entries", resp.Domains)
	}
	wantDomains := map[string]string{"a.example": "reason-a", "b.example": "reason-b"}
	for _, d := range resp.Domains {
		wantReason, ok := wantDomains[d.Domain]
		if !ok {
			t.Errorf("unexpected domain %q", d.Domain)
			continue
		}
		if d.Reason != wantReason {
			t.Errorf("reason for %q = %q, want %q", d.Domain, d.Reason, wantReason)
		}
		if !d.AddedAt.Equal(addedAt) {
			t.Errorf("added_at for %q = %v, want %v", d.Domain, d.AddedAt, addedAt)
		}
		delete(wantDomains, d.Domain)
	}
	if len(wantDomains) != 0 {
		t.Errorf("domains missing from response: %v", wantDomains)
	}
}

func TestAdminMuxListBlocklistEmpty(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)

	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/blocklist", adminPlaintext, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Domains []any `json:"domains"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Domains == nil {
		t.Fatalf(`domains = null, want [] (body: %s)`, rec.Body.String())
	}
	if len(resp.Domains) != 0 {
		t.Fatalf("domains = %v, want empty", resp.Domains)
	}
}

func TestAdminMuxBlocklistMissingCookieRejected(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)
	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/blocklist", "", nil)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestAdminMuxBlocklistWrongCookieRejected(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)
	rec := doAdminRequest(t, mux, http.MethodGet, "/admin/blocklist", "not-the-right-value", nil)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestAdminMuxCreateBlocklistEntryOversizedBodyRejected(t *testing.T) {
	mux, _, _, _, _ := newTestAdminMux(t)

	oversized := []byte(`{"domain":"` + strings.Repeat("a", app.MaxBodyBytes+1) + `"}`)
	rec := doAdminRequest(t, mux, http.MethodPost, "/admin/blocklist", adminPlaintext, oversized)
	assertErrorResponse(t, rec, http.StatusBadRequest, "body_too_large")
}
