package mcp

import (
	"context"
	"net/url"
	"strconv"

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

// getThreadArgs is get_thread's input, matching 03-API.md's "MCP tool
// mapping" table signature exactly: get_thread(id).
type getThreadArgs struct {
	ID string `json:"id" jsonschema:"the thread id to fetch"`
}

// threadInfo is the JSON shape of GET /threads/{id}'s "thread" field,
// mirroring internal/adapters/http/local.go's threadResponse
// field-for-field (id, subject, created_at) -- see this task's spec in
// STATUS.md, design decision 4. created_at is a plain string (design
// decision 4), not time.Time.
type threadInfo struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	CreatedAt string `json:"created_at"`
}

// getThreadOut is get_thread's output: GET /threads/{id}'s 200 response
// body. Messages reuses readMessageOut verbatim -- its shape (every
// messageResponse field except agent_id, plain-string timestamps) is
// exactly what this task wants too, and encoding/json silently drops
// the wire's extra agent_id field when decoding into a struct that
// lacks it. See this task's spec in STATUS.md, design decision 4.
type getThreadOut struct {
	Thread   threadInfo       `json:"thread"`
	Messages []readMessageOut `json:"messages"`
}

// newGetThreadHandler returns the tool handler for get_thread, closing
// over c. On any Client error (network failure, or a decoded REST error
// such as a 404 not_found -- covering both "no such thread" and "thread
// exists but caller owns none of its messages", per local.go's
// handleGetThread), it returns the error unchanged -- same passthrough
// pattern as newSendMailHandler/newReadMessageHandler.
func newGetThreadHandler(c *Client) mcp.ToolHandlerFor[getThreadArgs, getThreadOut] {
	return func(ctx context.Context, req *mcp.CallToolRequest, args getThreadArgs) (*mcp.CallToolResult, getThreadOut, error) {
		var out getThreadOut
		path := "/threads/" + url.PathEscape(args.ID)
		if err := c.doJSON(ctx, "GET", path, nil, &out); err != nil {
			return nil, getThreadOut{}, err
		}
		return nil, out, nil
	}
}

// searchThreadsArgs is search_threads's input: 03-API.md's "MCP tool
// mapping" table signature search_threads(query, limit?), plus a
// human-resolved optional cursor field for page 2+ (see STATUS.md's
// "MCP pagination gap" decision). See this task's spec in STATUS.md,
// design decisions 2, 3, and 6 -- an empty Query is a well-defined "all
// of the caller's threads" REST call, not an error, and cursor maps
// directly to REST's existing after query param.
type searchThreadsArgs struct {
	Query  string `json:"query" jsonschema:"the search query -- matches any message in a thread (FTS5); empty returns all of the caller's threads"`
	Limit  int    `json:"limit,omitempty" jsonschema:"optional page size, default 50, max 200 (enforced server-side)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"optional page cursor from a previous call's next_cursor, to fetch the next page"`
}

// threadSummary is the JSON shape of one entry in GET /threads' "threads"
// array, mirroring internal/adapters/http/local.go's threadListItem
// field-for-field -- see this task's spec in STATUS.md, design decision
// 4. created_at is a plain string, not time.Time, same reasoning as
// threadInfo/readMessageOut.
type threadSummary struct {
	ID           string `json:"id"`
	Subject      string `json:"subject"`
	CreatedAt    string `json:"created_at"`
	MessageCount int    `json:"message_count"`
}

// searchThreadsOut is search_threads's output: GET /threads?q=...'s 200
// response body (03-API.md: {threads: [...], next_cursor}). NextCursor
// has no ",omitempty" -- the wire always carries the key, as a string or
// JSON null -- see this task's spec in STATUS.md, design decision 5.
type searchThreadsOut struct {
	Threads    []threadSummary `json:"threads"`
	NextCursor *string         `json:"next_cursor"`
}

// newSearchThreadsHandler returns the tool handler for search_threads,
// closing over c. The query string is built with net/url.Values (not
// path escaping -- args.Query is free text, unlike task 2/3's
// path-segment ids) so args.Query and, if positive, args.Limit are
// correctly form-encoded. On any Client error it returns the error
// unchanged, same passthrough pattern as every other handler here.
func newSearchThreadsHandler(c *Client) mcp.ToolHandlerFor[searchThreadsArgs, searchThreadsOut] {
	return func(ctx context.Context, req *mcp.CallToolRequest, args searchThreadsArgs) (*mcp.CallToolResult, searchThreadsOut, error) {
		var out searchThreadsOut
		q := url.Values{}
		q.Set("q", args.Query)
		if args.Limit > 0 {
			q.Set("limit", strconv.Itoa(args.Limit))
		}
		if args.Cursor != "" {
			q.Set("after", args.Cursor)
		}
		path := "/threads?" + q.Encode()
		if err := c.doJSON(ctx, "GET", path, nil, &out); err != nil {
			return nil, searchThreadsOut{}, err
		}
		return nil, out, nil
	}
}

// listInboxArgs is list_inbox's input: 03-API.md's "MCP tool mapping"
// table signature list_inbox(unread_only?, from?, thread?, status?,
// limit?), plus two human-resolved optional additions: cursor (see
// STATUS.md's "MCP pagination gap" decision, already applied by
// search_threads) and query (see STATUS.md's "MCP list_inbox query gap"
// decision). Every field is optional -- an all-omitted call is a
// well-defined "give me my inbox, page one" request, per
// handleListMessages. UnreadOnly is a plain bool, not *bool -- see this
// task's spec in STATUS.md, "unread_only bool-semantics decision":
// domain.MessageFilter.Unread is itself only ever a two-state bool
// (there is no third domain state an omitted vs. explicit-false
// argument would need to distinguish), so nothing is lost by not using
// a pointer here.
type listInboxArgs struct {
	UnreadOnly bool   `json:"unread_only,omitempty" jsonschema:"true to return only unread messages; false or omitted returns all messages regardless of read state"`
	From       string `json:"from,omitempty" jsonschema:"optional sender address filter"`
	Thread     string `json:"thread,omitempty" jsonschema:"optional thread id filter"`
	Status     string `json:"status,omitempty" jsonschema:"optional delivery status filter"`
	Limit      int    `json:"limit,omitempty" jsonschema:"optional page size, default 50, max 200 (enforced server-side)"`
	Cursor     string `json:"cursor,omitempty" jsonschema:"optional page cursor from a previous call's next_cursor, to fetch the next page"`
	Query      string `json:"query,omitempty" jsonschema:"optional free-text match against message bodies (FTS5); omitted returns all matching messages regardless of content"`
}

// listInboxOut is list_inbox's output: GET /messages's 200 response body
// (03-API.md: {messages: [...], next_cursor}). Messages reuses
// readMessageOut verbatim, same reasoning as getThreadOut -- see this
// task's spec in STATUS.md, design decision 4. NextCursor has no
// ",omitempty" -- the wire always carries the key.
type listInboxOut struct {
	Messages   []readMessageOut `json:"messages"`
	NextCursor *string          `json:"next_cursor"`
}

// newListInboxHandler returns the tool handler for list_inbox, closing
// over c. Each of the seven possible query params is added only when it
// carries a non-default value -- see this task's spec in STATUS.md,
// design decision 5, for UnreadOnly's true-only encoding in particular.
// On any Client error it returns the error unchanged, same passthrough
// pattern as every other handler here.
func newListInboxHandler(c *Client) mcp.ToolHandlerFor[listInboxArgs, listInboxOut] {
	return func(ctx context.Context, req *mcp.CallToolRequest, args listInboxArgs) (*mcp.CallToolResult, listInboxOut, error) {
		var out listInboxOut
		q := url.Values{}
		if args.Query != "" {
			q.Set("q", args.Query)
		}
		if args.UnreadOnly {
			q.Set("unread", "true")
		}
		if args.From != "" {
			q.Set("from", args.From)
		}
		if args.Thread != "" {
			q.Set("thread", args.Thread)
		}
		if args.Status != "" {
			q.Set("status", args.Status)
		}
		if args.Limit > 0 {
			q.Set("limit", strconv.Itoa(args.Limit))
		}
		if args.Cursor != "" {
			q.Set("after", args.Cursor)
		}
		path := "/messages"
		if encoded := q.Encode(); encoded != "" {
			path += "?" + encoded
		}
		if err := c.doJSON(ctx, "GET", path, nil, &out); err != nil {
			return nil, listInboxOut{}, err
		}
		return nil, out, nil
	}
}

// NewServer builds the cdampd MCP server and registers send_mail,
// read_message, get_thread, search_threads, and list_inbox -- all five
// Phase 7 tools.
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
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_thread",
		Description: "Fetch a thread's full ordered message list by thread id.",
	}, newGetThreadHandler(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_threads",
		Description: "Search the caller's threads by free-text query; empty query returns all of the caller's threads, oldest first.",
	}, newSearchThreadsHandler(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_inbox",
		Description: "List the caller's own inbox messages, optionally filtered by query/unread/from/thread/status, oldest-page-first.",
	}, newListInboxHandler(c))
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
