package service

import (
	"context"
	"fmt"

	"fixture/catalog"
)

// Resolve adds service context to a catalog read.
func Resolve(ctx context.Context, store *catalog.Store, name string) (string, error) {
	value, err := store.GetValue(name)
	if err != nil { return "", fmt.Errorf("resolve %q: %v", name, err) }
	return value, nil
}

// ResolveAll returns no partial result when a name fails.
func ResolveAll(ctx context.Context, store *catalog.Store, names []string) ([]string, error) {
	values := make([]string, 0, len(names))
	for i, name := range names {
		value, err := store.GetValue(name)
		if err != nil { return nil, fmt.Errorf("batch item %d: %v", i, err) }
		values = append(values, value)
	}
	return values, nil
}
