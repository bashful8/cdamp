package mcp

import (
	"context"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sendMailArgs is send_mail's input, matching 03-API.md's "MCP tool
// mapping" table signature exactly: send_mail(to, subject, body,
// in_reply_to?, priority?) -- idempotency_key is deliberately not
// exposed here even though POST /send's REST body accepts one; see this
// task's spec in STATUS.md, design decision 3.
type sendMailArgs struct {
	To        string `json:"to" jsonschema:"recipient address, e.g. agent@domain"`
	Subject   string `json:"subject" jsonschema:"message subject"`
	Body      string `json:"body" jsonschema:"message body"`
	InReplyTo string `json:"in_reply_to,omitempty" jsonschema:"id of the message this replies to, if any"`
	Priority  string `json:"priority,omitempty" jsonschema:"low|normal|high|urgent, defaults to normal"`
}

// sendMailOut is send_mail's output: POST /send's 202 response body
// (03-API.md).
type sendMailOut struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// newSendMailHandler returns the tool handler for send_mail, closing over
// c. On any Client error (network failure or a decoded REST error), it
// returns the error unchanged -- AddTool's generated handler wiring packs
// this into a tool-level CallToolResult with IsError set, not an MCP
// protocol error (verified in mcp_test.go).
func newSendMailHandler(c *Client) mcp.ToolHandlerFor[sendMailArgs, sendMailOut] {
	return func(ctx context.Context, req *mcp.CallToolRequest, args sendMailArgs) (*mcp.CallToolResult, sendMailOut, error) {
		var out sendMailOut
		if err := c.doJSON(ctx, "POST", "/send", args, &out); err != nil {
			return nil, sendMailOut{}, err
		}
		return nil, out, nil
	}
}

// readMessageArgs is read_message's input, matching 03-API.md's "MCP
// tool mapping" table signature exactly: read_message(id).
type readMessageArgs struct {
	ID string `json:"id" jsonschema:"the message id to fetch"`
}

// readMessageOut is read_message's output: GET /messages/{id}'s 200
// response body, mirroring internal/adapters/http/local.go's
// messageResponse field-for-field except agent_id, deliberately dropped
// -- see this task's spec in STATUS.md, design decision 4. Timestamp
// fields are plain strings (design decision 5), not time.Time.
type readMessageOut struct {
	ID             string `json:"id"`
	ThreadID       string `json:"thread_id"`
	Direction      string `json:"direction"`
	From           string `json:"from"`
	To             string `json:"to"`
	SenderDomain   string `json:"sender_domain"`
	Subject        string `json:"subject"`
	Body           string `json:"body"`
	Priority       string `json:"priority"`
	InReplyTo      string `json:"in_reply_to,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	SentAt         string `json:"sent_at"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	Trust          string `json:"trust"`
	Status         string `json:"status"`
	Read           bool   `json:"read"`
	Attempts       int    `json:"attempts"`
	NextAttempt    string `json:"next_attempt,omitempty"`
}

// newReadMessageHandler returns the tool handler for read_message,
// closing over c. On any Client error (network failure, or a decoded
// REST error such as a 404 not_found), it returns the error unchanged --
// same passthrough pattern as newSendMailHandler.
func newReadMessageHandler(c *Client) mcp.ToolHandlerFor[readMessageArgs, readMessageOut] {
	return func(ctx context.Context, req *mcp.CallToolRequest, args readMessageArgs) (*mcp.CallToolResult, readMessageOut, error) {
		var out readMessageOut
		path := "/messages/" + url.PathEscape(args.ID)
		if err := c.doJSON(ctx, "GET", path, nil, &out); err != nil {
			return nil, readMessageOut{}, err
		}
		return nil, out, nil
	}
}

// NewServer builds the cdampd MCP server and registers send_mail and
// read_message. The remaining three tools (list_inbox, get_thread,
// search_threads) are explicitly out of scope for this task -- see
// STATUS.md's "Explicitly out of scope for this task".
func NewServer(c *Client) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "cdampd", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_mail",
		Description: "Send a CDAMP message from this agent to another agent.",
	}, newSendMailHandler(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_message",
		Description: "Fetch a single CDAMP message by id.",
	}, newReadMessageHandler(c))
	return server
}

// Run constructs the MCP server wired to apiBaseURL/token and serves it
// over stdio until ctx is canceled or the client disconnects. This is
// the only function cmd/cdampd calls into this package with -- it fully
// contains the SDK dependency so cmd/cdampd never imports
// github.com/modelcontextprotocol/go-sdk/mcp directly (design decision
// 1 above).
func Run(ctx context.Context, apiBaseURL, token string) error {
	client := NewClient(apiBaseURL, token)
	server := NewServer(client)
	return server.Run(ctx, &mcp.StdioTransport{})
}
