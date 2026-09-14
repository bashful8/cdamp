package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestSession spins up a fake local agent API at apiHandler, wires a
// cdampd MCP server (via NewServer) to it with the given token, connects
// an in-process client/server pair over mcp.NewInMemoryTransports() (no
// real stdio process), and returns a connected *mcp.ClientSession ready
// to call tools against, plus a cleanup func that shuts everything down.
func newTestSession(t *testing.T, apiHandler http.Handler, token string) *mcp.ClientSession {
	t.Helper()

	apiServer := httptest.NewServer(apiHandler)
	t.Cleanup(apiServer.Close)

	client := NewClient(apiServer.URL, token)
	server := NewServer(client)

	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting server session: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting client session: %v", err)
	}
	t.Cleanup(func() { clientSession.Close() })

	return clientSession
}

func TestSendMailTool_CallsCorrectEndpoint(t *testing.T) {
	const token = "test-bearer-token"

	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
		}
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(sendMailOut{ID: "msg-123", Status: "queued"})
	})

	session := newTestSession(t, handler, token)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "send_mail",
		Arguments: map[string]any{
			"to":      "bob@example.com",
			"subject": "hello",
			"body":    "hi there",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned an error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/send" {
		t.Errorf("path = %q, want /send", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+token)
	}
	if gotBody.To != "bob@example.com" || gotBody.Subject != "hello" || gotBody.Body != "hi there" {
		t.Errorf("decoded request body = %+v, want to=bob@example.com subject=hello body=%q", gotBody, "hi there")
	}

	if res.IsError {
		t.Fatalf("res.IsError = true, want false (content: %+v)", res.Content)
	}

	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map[string]any: %+v", res.StructuredContent, res.StructuredContent)
	}
	if structured["id"] != "msg-123" {
		t.Errorf("StructuredContent[id] = %v, want msg-123", structured["id"])
	}
	if structured["status"] != "queued" {
		t.Errorf("StructuredContent[status] = %v, want queued", structured["status"])
	}
}

func TestSendMailTool_RESTErrorBecomesToolError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "bad_request",
				"message": "recipient address is invalid",
			},
		})
	})

	session := newTestSession(t, handler, "test-bearer-token")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "send_mail",
		Arguments: map[string]any{
			"to":      "not-an-address",
			"subject": "hello",
			"body":    "hi there",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error, want err == nil with res.IsError instead: %v", err)
	}

	if !res.IsError {
		t.Fatalf("res.IsError = false, want true (content: %+v)", res.Content)
	}
}

func TestReadMessageTool_CallsCorrectEndpoint(t *testing.T) {
	const token = "test-bearer-token"

	var (
		gotMethod string
		gotPath   string
		gotAuth   string
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(readMessageOut{
			ID:        "msg-123",
			ThreadID:  "thread-1",
			Direction: "inbound",
			From:      "alice@example.com",
			To:        "bob@example.com",
			Subject:   "hello",
			Body:      "hi there",
			Priority:  "normal",
			SentAt:    "2026-09-13T00:00:00Z",
			Trust:     "verified",
			Status:    "delivered",
		})
	})

	session := newTestSession(t, handler, token)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_message",
		Arguments: map[string]any{
			"id": "msg-123",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned an error: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/messages/msg-123" {
		t.Errorf("path = %q, want /messages/msg-123", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+token)
	}

	if res.IsError {
		t.Fatalf("res.IsError = true, want false (content: %+v)", res.Content)
	}

	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map[string]any: %+v", res.StructuredContent, res.StructuredContent)
	}
	if structured["id"] != "msg-123" {
		t.Errorf("StructuredContent[id] = %v, want msg-123", structured["id"])
	}
	if structured["subject"] != "hello" {
		t.Errorf("StructuredContent[subject] = %v, want hello", structured["subject"])
	}
	if structured["body"] != "hi there" {
		t.Errorf("StructuredContent[body] = %v, want %q", structured["body"], "hi there")
	}
	if structured["status"] != "delivered" {
		t.Errorf("StructuredContent[status] = %v, want delivered", structured["status"])
	}
}

func TestReadMessageTool_NotFoundBecomesToolError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "not_found",
				"message": "message not found",
			},
		})
	})

	session := newTestSession(t, handler, "test-bearer-token")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_message",
		Arguments: map[string]any{
			"id": "does-not-exist",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error, want err == nil with res.IsError instead: %v", err)
	}

	if !res.IsError {
		t.Fatalf("res.IsError = false, want true (content: %+v)", res.Content)
	}
}

func TestGetThreadTool_CallsCorrectEndpoint(t *testing.T) {
	const token = "test-bearer-token"

	var (
		gotMethod string
		gotPath   string
		gotAuth   string
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"thread": map[string]any{
				"id":         "thread-1",
				"subject":    "hello",
				"created_at": "2026-09-13T00:00:00Z",
			},
			"messages": []map[string]any{
				{
					"id":        "msg-1",
					"thread_id": "thread-1",
					"agent_id":  int64(42),
					"direction": "inbound",
					"from":      "alice@example.com",
					"to":        "bob@example.com",
					"subject":   "hello",
					"body":      "hi there",
					"priority":  "normal",
					"sent_at":   "2026-09-13T00:00:00Z",
					"trust":     "verified",
					"status":    "delivered",
				},
				{
					"id":        "msg-2",
					"thread_id": "thread-1",
					"agent_id":  int64(42),
					"direction": "outbound",
					"from":      "bob@example.com",
					"to":        "alice@example.com",
					"subject":   "re: hello",
					"body":      "hi back",
					"priority":  "normal",
					"sent_at":   "2026-09-13T00:01:00Z",
					"trust":     "verified",
					"status":    "sent",
				},
			},
		})
	})

	session := newTestSession(t, handler, token)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_thread",
		Arguments: map[string]any{
			"id": "thread-1",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned an error: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/threads/thread-1" {
		t.Errorf("path = %q, want /threads/thread-1", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+token)
	}

	if res.IsError {
		t.Fatalf("res.IsError = true, want false (content: %+v)", res.Content)
	}

	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map[string]any: %+v", res.StructuredContent, res.StructuredContent)
	}

	thread, ok := structured["thread"].(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent[thread] is %T, want map[string]any: %+v", structured["thread"], structured["thread"])
	}
	if thread["id"] != "thread-1" {
		t.Errorf("thread[id] = %v, want thread-1", thread["id"])
	}
	if thread["subject"] != "hello" {
		t.Errorf("thread[subject] = %v, want hello", thread["subject"])
	}

	messages, ok := structured["messages"].([]any)
	if !ok {
		t.Fatalf("StructuredContent[messages] is %T, want []any: %+v", structured["messages"], structured["messages"])
	}
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(messages))
	}

	first, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("messages[0] is %T, want map[string]any: %+v", messages[0], messages[0])
	}
	if first["id"] != "msg-1" {
		t.Errorf("messages[0][id] = %v, want msg-1", first["id"])
	}
	if first["subject"] != "hello" {
		t.Errorf("messages[0][subject] = %v, want hello", first["subject"])
	}
	if first["body"] != "hi there" {
		t.Errorf("messages[0][body] = %v, want %q", first["body"], "hi there")
	}

	for i, m := range messages {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("messages[%d] is %T, want map[string]any: %+v", i, m, m)
		}
		if _, present := mm["agent_id"]; present {
			t.Errorf("messages[%d] carries an agent_id key, want it dropped", i)
		}
	}
}

func TestSearchThreadsTool_CallsCorrectEndpoint(t *testing.T) {
	const token = "test-bearer-token"

	var (
		gotMethod string
		gotPath   string
		gotQuery  url.Values
		gotAuth   string
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"threads": []map[string]any{
				{
					"id":            "thread-1",
					"subject":       "hello",
					"created_at":    "2026-09-13T00:00:00Z",
					"message_count": 3,
				},
				{
					"id":            "thread-2",
					"subject":       "another",
					"created_at":    "2026-09-13T00:01:00Z",
					"message_count": 1,
				},
			},
			"next_cursor": "thread-2",
		})
	})

	session := newTestSession(t, handler, token)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_threads",
		Arguments: map[string]any{
			"query":  "hello",
			"limit":  10,
			"cursor": "thread-1",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned an error: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/threads" {
		t.Errorf("path = %q, want /threads", gotPath)
	}
	if gotQuery.Get("q") != "hello" {
		t.Errorf("query[q] = %q, want hello", gotQuery.Get("q"))
	}
	if gotQuery.Get("limit") != "10" {
		t.Errorf("query[limit] = %q, want 10", gotQuery.Get("limit"))
	}
	if gotQuery.Get("after") != "thread-1" {
		t.Errorf("query[after] = %q, want thread-1", gotQuery.Get("after"))
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+token)
	}

	if res.IsError {
		t.Fatalf("res.IsError = true, want false (content: %+v)", res.Content)
	}

	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map[string]any: %+v", res.StructuredContent, res.StructuredContent)
	}

	threads, ok := structured["threads"].([]any)
	if !ok {
		t.Fatalf("StructuredContent[threads] is %T, want []any: %+v", structured["threads"], structured["threads"])
	}
	if len(threads) != 2 {
		t.Fatalf("len(threads) = %d, want 2", len(threads))
	}

	first, ok := threads[0].(map[string]any)
	if !ok {
		t.Fatalf("threads[0] is %T, want map[string]any: %+v", threads[0], threads[0])
	}
	if first["id"] != "thread-1" {
		t.Errorf("threads[0][id] = %v, want thread-1", first["id"])
	}
	if first["subject"] != "hello" {
		t.Errorf("threads[0][subject] = %v, want hello", first["subject"])
	}
	if first["message_count"] != float64(3) {
		t.Errorf("threads[0][message_count] = %v, want 3", first["message_count"])
	}

	if structured["next_cursor"] != "thread-2" {
		t.Errorf("StructuredContent[next_cursor] = %v, want thread-2", structured["next_cursor"])
	}
}

func TestSearchThreadsTool_OmittedOptionalFieldsAreNotSent(t *testing.T) {
	var gotQuery url.Values

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"threads":     []map[string]any{},
			"next_cursor": nil,
		})
	})

	session := newTestSession(t, handler, "test-bearer-token")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_threads",
		Arguments: map[string]any{
			"query": "hello",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned an error: %v", err)
	}

	if gotQuery.Has("limit") {
		t.Errorf("query has limit = %q, want it absent entirely", gotQuery.Get("limit"))
	}
	if gotQuery.Has("after") {
		t.Errorf("query has after = %q, want it absent entirely", gotQuery.Get("after"))
	}
	if gotQuery.Get("q") != "hello" {
		t.Errorf("query[q] = %q, want hello", gotQuery.Get("q"))
	}

	if res.IsError {
		t.Fatalf("res.IsError = true, want false (content: %+v)", res.Content)
	}

	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map[string]any: %+v", res.StructuredContent, res.StructuredContent)
	}

	if structured["next_cursor"] != nil {
		t.Errorf("StructuredContent[next_cursor] = %v, want nil", structured["next_cursor"])
	}

	threads, ok := structured["threads"].([]any)
	if !ok {
		t.Fatalf("StructuredContent[threads] is %T, want []any: %+v", structured["threads"], structured["threads"])
	}
	if len(threads) != 0 {
		t.Errorf("len(threads) = %d, want 0", len(threads))
	}
}

func TestGetThreadTool_NotFoundBecomesToolError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "not_found",
				"message": "thread not found",
			},
		})
	})

	session := newTestSession(t, handler, "test-bearer-token")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_thread",
		Arguments: map[string]any{
			"id": "does-not-exist",
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error, want err == nil with res.IsError instead: %v", err)
	}

	if !res.IsError {
		t.Fatalf("res.IsError = false, want true (content: %+v)", res.Content)
	}
}
