package fixture

import "container/list"

// Cache keeps at most capacity entries and evicts the least recently used.
type Cache struct {
	capacity int
	order    *list.List // front is the most recently used
	elements map[string]*list.Element
}

type entry struct {
	key   string
	value int
}

// New returns an empty cache. capacity must be at least 1.
func New(capacity int) *Cache {
	return &Cache{capacity: capacity, order: list.New(), elements: make(map[string]*list.Element)}
}

// Get returns the value for key.
func (c *Cache) Get(key string) (int, bool) {
	element, found := c.elements[key]
	if !found {
		return 0, false
	}
	return element.Value.(*entry).value, true
}

// Put stores value for key.
func (c *Cache) Put(key string, value int) {
	element := c.order.PushFront(&entry{key: key, value: value})
	c.elements[key] = element
	if c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.elements, oldest.Value.(*entry).key)
	}
}

// Len returns the number of entries.
func (c *Cache) Len() int {
	return c.order.Len()
}
