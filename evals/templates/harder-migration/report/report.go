package report

import (
	"context"
	"errors"
	"fmt"

	"fixture/catalog"
	"fixture/service"
)

func Line(ctx context.Context, store *catalog.Store, name string) (string, error) {
	value, err := service.Resolve(ctx, store, name)
	if err != nil { return "", fmt.Errorf("report: %v", err) }
	return name + "=" + value + "\n", nil
}

// Optional uses fallback only when the catalog does not contain name.
func Optional(ctx context.Context, store *catalog.Store, name, fallback string) (string, error) {
	value, err := store.GetValue(name)
	if errors.Is(err, catalog.ErrMissing) { return fallback, nil }
	if err != nil { return "", fmt.Errorf("optional %q: %v", name, err) }
	return value, nil
}
