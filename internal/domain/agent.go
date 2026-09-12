package domain

import "time"

// Agent is a local agent registered with this instance.
type Agent struct {
	ID        int64
	Name      string
	TokenHash string
	CreatedAt time.Time
}
