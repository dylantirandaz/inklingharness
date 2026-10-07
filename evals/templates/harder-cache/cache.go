package cache

import (
	"context"
	"sync"
)

// Cache retains successful values and shares active loads.
type Cache struct {
	mu sync.Mutex
	entries map[string]*entry
	load Loader
}

func New(load Loader) *Cache {
	return &Cache{entries: make(map[string]*entry), load: load}
}

// Get returns the value for key or the caller's cancellation error.
func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		e = &entry{done: make(chan struct{})}
		c.entries[key] = e
		c.mu.Unlock()
		e.value, e.err = c.load(ctx, key)
		close(e.done)
	} else {
		c.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-e.done:
		return e.value, e.err
	}
}
