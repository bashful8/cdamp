package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
