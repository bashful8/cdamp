// Package mcp is CDAMP's MCP (Model Context Protocol) surface: thin tool
// wrappers over the local agent API, per 03-API.md's "MCP tool mapping"
// table and 02-ARCHITECTURE.md's "MCP is a thin client of REST, never a
// parallel code path" golden rule. Every tool here does exactly one HTTP
// call to the already-running cdampd's local agent API, authenticated
// with the calling agent's own bearer token, and shapes the JSON response
// into the tool's result -- no independent logic, per 04-BUILD-PLAN.md's
// Phase 7 instruction. This package fully contains the
// github.com/modelcontextprotocol/go-sdk dependency: cmd/cdampd imports
// only this package's exported Run function, never the SDK directly.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// apiError mirrors 03-API.md's error shape for every endpoint:
// {"error":{"code":"...","message":"..."}}.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Client is a thin REST client for cdampd's local agent API
// (03-API.md's "Local agent API" section), used by every tool handler in
// this package. It performs the HTTP call and JSON (de)serialization and
// nothing else -- no retries, no caching.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient returns a Client that calls baseURL (e.g.
// "http://127.0.0.1:8443") with an "Authorization: Bearer token" header
// on every request.
func NewClient(baseURL, token string) *Client {
	return &Client{baseURL: baseURL, token: token, http: &http.Client{}}
}

// doJSON performs method path against c.baseURL, JSON-encoding body (if
// non-nil) as the request payload and JSON-decoding a successful response
// into out (if non-nil). A non-2xx response is decoded per 03-API.md's
// error shape and returned as a wrapped Go error -- callers (tool
// handlers) return this error unchanged, which the SDK's AddTool-driven
// handler wiring packs into a tool-level CallToolResult automatically
// (verified in mcp_test.go).
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reqBody bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = *bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, &reqBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiError
		if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
			return fmt.Errorf("%s %s: status %d (undecodable error body: %w)", method, path, resp.StatusCode, err)
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, apiErr.Error.Code, apiErr.Error.Message)
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
	}
	return nil
}
