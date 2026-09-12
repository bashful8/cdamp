// Package http implements CDAMP's HTTP surface: the local bearer-token
// agent API (/send, /messages, /threads, /agents/me), the federation
// endpoints (/deliver, /.well-known/cdamp/*), the admin API
// (/admin/agents, /admin/blocklist), and the shared middleware (bearer
// auth, admin session, rate limiting, size limits). It is the single
// enforcement point for auth, rate-limiting, and audit per
// 02-ARCHITECTURE.md.
package http
