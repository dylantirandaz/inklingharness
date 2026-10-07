package catalog

import (
	"context"
	"fmt"
)

type Store struct {
	values map[string]string
	closed bool
}

func New(values map[string]string) *Store {
	copy := make(map[string]string, len(values))
	for key, value := range values { copy[key] = value }
	return &Store{values: copy}
}

func (s *Store) Close() { s.closed = true }

// GetValue reads a catalog value by name.
func (s *Store) GetValue(name string) (string, error) {
	if s.closed { return "", fmt.Errorf("read %q: %v", name, ErrClosed) }
	value, ok := s.values[name]
	if !ok { return "", fmt.Errorf("read %q: %v", name, ErrMissing) }
	return value, nil
}

// Lookup is the context-aware catalog API.
func (s *Store) Lookup(ctx context.Context, name string) (string, error) {
	return s.GetValue(name)
}
