package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CanonicalString builds 01-PROTOCOL.md's canonical signing string:
//
//	from|to|subject|priority|in_reply_to|payload_hash
//
// inReplyTo must already be "" (never the literal "null") when absent —
// callers must not pass a Go nil-turned-string. payload is the full
// {type, message, context} object — canonical_json hashes all of it, not
// just the message text. thread_id and timestamp are deliberately excluded
// per 01-PROTOCOL.md's Signing section.
//
// Used by both Signer.Sign at send time (Phase 5's DeliveryWorker) and
// Verifier.Verify at receive time (ReceiveMessage, this phase) — building
// it in exactly one place keeps signing and verification from drifting
// apart, per the cdamp-signing skill.
func CanonicalString(from, to, subject, priority, inReplyTo string, payload map[string]any) ([]byte, error) {
	hash, err := PayloadHash(payload)
	if err != nil {
		return nil, fmt.Errorf("building canonical string: %w", err)
	}
	s := strings.Join([]string{from, to, subject, priority, inReplyTo, hash}, "|")
	return []byte(s), nil
}

// PayloadHash returns base64(SHA256(canonical_json(payload))), per
// 01-PROTOCOL.md's Signing section.
func PayloadHash(payload map[string]any) (string, error) {
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("computing payload hash: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// canonicalJSON re-encodes v with object keys sorted lexicographically at
// every nesting level, no insignificant whitespace, UTF-8, no trailing
// newline — 01-PROTOCOL.md's deliberately-small JCS subset (RFC 8785),
// sufficient because CDAMP payloads are simple JSON: strings and small
// objects, no floats needing canonical number formatting. Per the
// cdamp-signing skill, this is a hand-rolled canonicalizer, not a
// third-party JCS dependency.
func canonicalJSON(v any) ([]byte, error) {
	var buf strings.Builder
	if err := writeCanonicalJSON(&buf, v); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// writeCanonicalJSON recursively writes v's canonical JSON encoding to buf.
// A nil v (an absent/empty payload) encodes as "null", matching
// encoding/json's own behavior for a nil interface — 01-PROTOCOL.md doesn't
// special-case this, and payloads are always a concrete
// {type,message,context} object in practice.
func writeCanonicalJSON(buf *strings.Builder, v any) error {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyJSON, err := json.Marshal(k)
			if err != nil {
				return fmt.Errorf("marshaling object key %q: %w", k, err)
			}
			buf.Write(keyJSON)
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')

	case []any:
		buf.WriteByte('[')
		for i, elem := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')

	default:
		// Strings, numbers, bools, and nil all have no nested keys to sort
		// and no insignificant whitespace to strip once marshaled: stdlib
		// json.Marshal already produces a minimal, whitespace-free encoding
		// for these leaf types.
		leaf, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("marshaling value: %w", err)
		}
		buf.Write(leaf)
	}
	return nil
}
