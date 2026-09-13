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

func TestDashboardHomeLinksToAgentInbox(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/dashboard/agents/1"`) {
		t.Errorf("body missing link to agent inbox:\n%s", body)
	}
}

func TestDashboardAgentInboxListsMessages(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, From: "bob@example.dev", Subject: "hello", Read: false}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m2", AgentID: 1, From: "carol@example.dev", Subject: "re: hello", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/agents/1", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice@example.dev") {
		t.Errorf("body missing alice@example.dev heading:\n%s", body)
	}
	if !strings.Contains(body, "bob@example.dev") || !strings.Contains(body, "hello") {
		t.Errorf("body missing m1's From/Subject:\n%s", body)
	}
	if !strings.Contains(body, "carol@example.dev") || !strings.Contains(body, "re: hello") {
		t.Errorf("body missing m2's From/Subject:\n%s", body)
	}
}

func TestDashboardAgentInboxUnknownIDReturns404(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/agents/999", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestDashboardAgentInboxMalformedIDReturns404(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/agents/not-a-number", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestDashboardAgentInboxListMessagesErrorReturns500(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})
	store.SetListMessagesErr(errors.New("boom"))

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/agents/1", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestDashboardAgentInboxLinksToThread(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, ThreadID: "t1", From: "bob@example.dev", Subject: "hello", Read: false}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/agents/1", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/dashboard/threads/t1"`) {
		t.Errorf("body missing link to thread:\n%s", body)
	}
}

func TestDashboardThreadShowsMessages(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, ThreadID: "t1", From: "bob@example.dev", To: "alice@example.dev", Subject: "hello", Body: "first message body", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m2", AgentID: 1, ThreadID: "t1", From: "alice@example.dev", To: "bob@example.dev", Subject: "re: hello", Body: "second message body", Read: false}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/threads/t1", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello") {
		t.Errorf("body missing thread subject in heading:\n%s", body)
	}
	if !strings.Contains(body, "bob@example.dev") || !strings.Contains(body, "alice@example.dev") {
		t.Errorf("body missing From/To addresses:\n%s", body)
	}
	if !strings.Contains(body, "first message body") || !strings.Contains(body, "second message body") {
		t.Errorf("body missing message bodies:\n%s", body)
	}
}

func TestDashboardThreadUnknownIDReturns404(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/threads/does-not-exist", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestDashboardHomeSearchShowsMatchingThreads(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, ThreadID: "t1", Subject: "budget report", Body: "the numbers", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m2", AgentID: 1, ThreadID: "t2", Subject: "lunch plans", Body: "pizza", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/?q=budget", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/dashboard/threads/t1"`) || !strings.Contains(body, "budget report") {
		t.Errorf("body missing matching thread t1:\n%s", body)
	}
	if strings.Contains(body, "lunch plans") {
		t.Errorf("body should not contain non-matching thread's subject:\n%s", body)
	}
}

func TestDashboardHomeSearchMergesAcrossAgents(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})
	store.AddAgent(&domain.Agent{ID: 2, Name: "bob", TokenHash: "hash-2"})

	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, ThreadID: "t1", Subject: "project apollo kickoff", Body: "let's begin", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m2", AgentID: 2, ThreadID: "t2", Subject: "apollo status update", Body: "on track", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/?q=apollo", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/dashboard/threads/t1"`) {
		t.Errorf("body missing agent 1's matching thread t1:\n%s", body)
	}
	if !strings.Contains(body, `href="/dashboard/threads/t2"`) {
		t.Errorf("body missing agent 2's matching thread t2:\n%s", body)
	}
}

func TestDashboardHomeSearchEmptyResultsNoMatch(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, ThreadID: "t1", Subject: "budget report", Body: "the numbers", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/?q=nonexistentquery", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `href="/dashboard/threads/t1"`) {
		t.Errorf("body should not contain any thread link:\n%s", body)
	}
	if !strings.Contains(body, "Search results for") {
		t.Errorf("body missing search results heading:\n%s", body)
	}
}

func TestDashboardHomeNoQueryShowsAgentsList(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

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
	if !strings.Contains(body, `href="/dashboard/agents/1"`) {
		t.Errorf("body missing link to agent inbox:\n%s", body)
	}
	if strings.Contains(body, "Search results for") {
		t.Errorf("body should not show search results heading:\n%s", body)
	}
}

func TestDashboardHomeSearchListAgentsErrorReturns500(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.SetListAgentsErr(errors.New("boom"))

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/?q=budget", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestDashboardHomeSearchGetThreadErrorReturns500(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	store.AddAgent(&domain.Agent{ID: 1, Name: "alice", TokenHash: "hash-1"})

	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, ThreadID: "t1", Subject: "budget report", Body: "the numbers", Read: true}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	store.SetGetThreadErr(errors.New("boom"))

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/?q=budget", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestDashboardThreadGetThreadErrorReturns500(t *testing.T) {
	store := fakes.NewInboxStoreFake()
	ctx := context.Background()
	if err := store.SaveMessage(ctx, &domain.Message{ID: "m1", AgentID: 1, ThreadID: "t1", Subject: "hello"}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	store.SetGetThreadErr(errors.New("boom"))

	cfg := &config.Config{Domain: "example.dev"}
	handler := NewDashboardHandler(store, cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/threads/t1", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}
