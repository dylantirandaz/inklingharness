package fixture

import "time"

// Event is one entry of the audit log API.
type Event struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Tags      []string  `json:"tags"`
	Note      string    `json:"note"`
}
