package domain

import "time"

// Thread groups related messages under a common subject.
type Thread struct {
	ID        string
	Subject   string
	CreatedAt time.Time
}
