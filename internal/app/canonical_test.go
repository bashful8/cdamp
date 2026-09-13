package app

import (
	"strings"
	"testing"
)

// TestPayloadHashProtocolExample reproduces 01-PROTOCOL.md's Signing
// section illustrative example (values shortened for readability there,
// but the payload itself — {"type":"request","message":"hi","context":{}}
// canonicalizing to {"context":{},"message":"hi","type":"request"} — is
// exact) to confirm canonical_json's key-sorting and PayloadHash's
// base64(SHA256(...)) composition.
func TestPayloadHashProtocolExample(t *testing.T) {
	payload := map[string]any{
		"type":    "request",
		"message": "hi",
		"context": map[string]any{},
	}

	got, err := PayloadHash(payload)
	if err != nil {
		t.Fatalf("PayloadHash: %v", err)
	}

	// Independently computed: sha256("{\"context\":{},\"message\":\"hi\",\"type\":\"request\"}"),
	// base64-encoded.
	want := "9+upP+wQOI9sQZb5JBq9cXE0HK0cVXmjVor+Tn2vBi8="
	if got != want {
		t.Fatalf("PayloadHash = %q, want %q", got, want)
	}
}

// TestCanonicalStringProtocolExample confirms CanonicalString's join order
// and the empty-in_reply_to-not-"null" rule against 01-PROTOCOL.md's
// illustrative example.
func TestCanonicalStringProtocolExample(t *testing.T) {
	payload := map[string]any{
		"type":    "request",
		"message": "hi",
		"context": map[string]any{},
	}

	got, err := CanonicalString(
		"researcher@example.dev", "reviewer@other.dev", "Question about the API",
		"normal", "", payload,
	)
	if err != nil {
		t.Fatalf("CanonicalString: %v", err)
	}

	want := "researcher@example.dev|reviewer@other.dev|Question about the API|normal||9+upP+wQOI9sQZb5JBq9cXE0HK0cVXmjVor+Tn2vBi8="
	if string(got) != want {
		t.Fatalf("CanonicalString = %q, want %q", got, want)
	}
}

// TestCanonicalStringInReplyToPresent confirms a non-empty in_reply_to is
// joined in verbatim (not specified precisely by 01-PROTOCOL.md's shortened
// example, but the join order is explicit: the field sits between priority
// and payload_hash).
func TestCanonicalStringInReplyToPresent(t *testing.T) {
	payload := map[string]any{"type": "response", "message": "ack", "context": map[string]any{}}

	got, err := CanonicalString("a@x.dev", "b@y.dev", "re: hi", "low", "msg_1_abcdef", payload)
	if err != nil {
		t.Fatalf("CanonicalString: %v", err)
	}

	// Confirm in_reply_to appears in the correct join position rather than
	// hand-computing the hash again: split on "|" and check field 4
	// (0-indexed: from, to, subject, priority, in_reply_to, payload_hash).
	fields := strings.Split(string(got), "|")
	const wantFields = 6
	if len(fields) != wantFields {
		t.Fatalf("CanonicalString has %d fields, want %d: %q", len(fields), wantFields, got)
	}
	if fields[4] != "msg_1_abcdef" {
		t.Fatalf("in_reply_to field = %q, want msg_1_abcdef", fields[4])
	}
}

// TestCanonicalJSONSortsNestedKeys confirms canonical_json sorts object
// keys at every nesting level, not just the top level — exercised with a
// nested payload.context object per the testing list's explicit ask.
func TestCanonicalJSONSortsNestedKeys(t *testing.T) {
	payload := map[string]any{
		"type":    "request",
		"message": "hi",
		"context": map[string]any{
			"zebra": 1,
			"alpha": map[string]any{
				"delta": true,
				"bravo": "x",
			},
		},
	}

	got, err := canonicalJSON(payload)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}

	want := `{"context":{"alpha":{"bravo":"x","delta":true},"zebra":1},"message":"hi","type":"request"}`
	if string(got) != want {
		t.Fatalf("canonicalJSON = %s, want %s", got, want)
	}
}

// TestCanonicalJSONNoWhitespace confirms there is no insignificant
// whitespace anywhere in the output.
func TestCanonicalJSONNoWhitespace(t *testing.T) {
	got, err := canonicalJSON(map[string]any{"b": 1, "a": "x"})
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	want := `{"a":"x","b":1}`
	if string(got) != want {
		t.Fatalf("canonicalJSON = %s, want %s", got, want)
	}
}
