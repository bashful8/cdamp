// Package delivery implements internal/domain's Delivery port: an
// outbound HTTP client that POSTs signed messages to a remote instance's
// /deliver endpoint, plus the background worker loop that claims pending
// outbound messages and retries them on a backoff schedule.
package delivery
