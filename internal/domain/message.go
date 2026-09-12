package domain

import "time"

// Message is a single CDAMP message, inbound or outbound.
type Message struct {
	ID             string
	ThreadID       string
	AgentID        int64  // local owner: sender (out) or recipient (in)
	Direction      string // "in" | "out"
	From, To       string
	SenderDomain   string
	Subject, Body  string
	Priority       string // low|normal|high|urgent
	InReplyTo      string
	IdempotencyKey string
	SentAt         time.Time
	ExpiresAt      *time.Time
	Trust          string // "verified" | "external" | "untrusted"
	Status         string // "pending" | "delivered" | "failed" | "received"
	Read           bool
	Attempts       int
	NextAttempt    *time.Time
}
