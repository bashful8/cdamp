package domain

import "time"

// AdminCredential is the single bootstrap credential that authenticates
// dashboard/admin-API sessions (02-ARCHITECTURE.md's Auth table: "Admin
// session, printed once on first boot ... Session cookie, not a bearer
// token — distinct code path from agent auth"). Exactly one
// instance-wide credential exists — there is no multi-admin concept.
// Per the Gap 2 human decision (STATUS.md, 2026-09-13), the admin
// session cookie's value *is* this credential's plaintext: a request's
// Cookie header is hashed and compared against TokenHash, the same
// hash-and-compare shape as agent bearer auth (a later task,
// adminAuthMiddleware, does the comparing — not this type).
type AdminCredential struct {
	TokenHash string
	CreatedAt time.Time
}
