package fixture

import "errors"

// ErrNotFound means that the store has no value for the key.
var ErrNotFound = errors.New("not found")

// Store is an in-memory key-value store.
type Store struct {
	values map[string]string
}

// NewStore returns a store with the given values.
func NewStore(values map[string]string) *Store {
	return &Store{values: values}
}

// Get returns the value for key, or ErrNotFound.
func (s *Store) Get(key string) (string, error) {
	value, found := s.values[key]
	if !found {
		return "", ErrNotFound
	}
	return value, nil
}
