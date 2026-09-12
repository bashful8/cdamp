// Package web implements the human-facing admin dashboard: HTTP handlers
// that render the go:embed'd templates/ (HTML + htmx) into views over
// every agent's inbox, unread counts, and thread contents. It is a thin
// client of the local/admin HTTP API — never a parallel code path with
// its own business logic, per 02-ARCHITECTURE.md.
package web
