package cache

import "context"

// Loader reads one value. Cache calls it once for each active key.
type Loader func(context.Context, string) (string, error)

type entry struct {
	done chan struct{}
	value string
	err error
}
