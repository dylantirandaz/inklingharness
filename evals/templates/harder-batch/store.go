package inventory

import "fmt"

func clone(items map[string]Item) map[string]Item {
	out := make(map[string]Item, len(items))
	for key, item := range items { out[key] = item }
	return out
}

func New(items map[string]Item) (*Store, error) {
	for key, item := range items {
		if key == "" || item.Count < 0 { return nil, fmt.Errorf("seed %q: %w", key, ErrInvalid) }
	}
	return &Store{items: clone(items)}, nil
}

// Snapshot returns a caller-owned copy of the current inventory.
func (s *Store) Snapshot() map[string]Item { return clone(s.items) }
