package fixture

import "sort"

// Counter counts words. The zero value is ready to use.
type Counter struct {
	counts map[string]int
}

// Add counts one more use of word.
func (c *Counter) Add(word string) {
	c.counts[word]++
}

// Count returns how many times word was added.
func (c *Counter) Count(word string) int {
	return c.counts[word]
}

// Words returns the counted words in sorted order.
func (c *Counter) Words() []string {
	words := make([]string, 0, len(c.counts))
	for word := range c.counts {
		words = append(words, word)
	}
	sort.Strings(words)
	return words
}
