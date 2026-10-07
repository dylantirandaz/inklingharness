package fixture

// Stats counts requests. Many goroutines call its methods at the same time.
type Stats struct {
	hits   int
	byPath map[string]int
}

// NewStats returns empty statistics.
func NewStats() *Stats {
	return &Stats{byPath: make(map[string]int)}
}

// Record counts one request for path.
func (s *Stats) Record(path string) {
	s.hits++
	s.byPath[path]++
}

// Hits returns the number of recorded requests.
func (s *Stats) Hits() int {
	return s.hits
}

// PathHits returns the number of recorded requests for path.
func (s *Stats) PathHits(path string) int {
	return s.byPath[path]
}
