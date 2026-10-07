package fixture

import "time"

// Backoff computes retry delays that double after each attempt.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay returns the wait before retry number attempt. Attempt 0 waits Base.
func (b Backoff) Delay(attempt int) time.Duration {
	delay := b.Base << attempt
	if delay > b.Max {
		return b.Max
	}
	return delay
}
