package inventory

import "errors"

var ErrInvalid = errors.New("invalid inventory change")
var ErrMissing = errors.New("missing inventory key")

type Item struct {
	Count int64
	Labels []string
}

// Change uses set, add, or delete. Only set uses Item; only add uses Delta.
type Change struct {
	Kind string
	Key string
	Item Item
	Delta int64
}

type Store struct { items map[string]Item }
