package audit

import "errors"

var ErrQuery = errors.New("invalid audit query")

type Event struct {
	ID string
	Time int64
	Kind string
	Message string
}

// Query selects a time window and optional exact kind, then a page.
// Since is included; Until is excluded. Limit zero means no limit.
type Query struct {
	Since int64
	Until int64
	Kind string
	Offset int
	Limit int
}

type Summary struct {
	Total int
	ByKind map[string]int
	First int64
	Last int64
}
