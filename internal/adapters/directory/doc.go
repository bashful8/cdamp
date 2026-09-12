// Package directory implements internal/domain's Directory port: it
// resolves an agent address to its public key, key id, and inbox URL via
// well-known lookup, caching results per 01-PROTOCOL.md's TTL.
package directory
