package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"cdamp/internal/config"
	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

func TestDashboardHomeListsAgentsWithUnreadCounts(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})
	store.AddAgent(&domain.Agent{ID: 2, Name: "bob", TokenHash: "hash-2"})

	ctx := context.Background()
	// alice: one unread, one read.
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, Read: false}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m2", AgentID: 1, Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	// bob: two unread.
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m3", AgentID: 2, Read: false}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m4", AgentID: 2, Read: false}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice@example.dev") {
		t.Errorf("body missing alice@example.dev:\n%s", body)
	}
	if !strings.Contains(body, "bob@example.dev") {
		t.Errorf("body missing bob@example.dev:\n%s", body)
	}
	// alice has 1 unread, bob has 2.
	if !strings.Contains(body, "<td>1</td>") {
		t.Errorf("body missing alice's unread count (1):\n%s", body)
	}
	if !strings.Contains(body, "<td>2</td>") {
		t.Errorf("body missing bob's unread count (2):\n%s", body)
	}
}

func TestDashboardHomeEmptyNoAgents(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Agents") {
		t.Errorf("body missing Agents heading:\n%s", body)
	}
	if strings.Contains(body, "@example.dev") {
		t.Errorf("body should contain no agent rows, got:\n%s", body)
	}
}

func TestDashboardHomeStoreErrorReturns500(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.SetListAgentsErr(errors.New("boom"))

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}
