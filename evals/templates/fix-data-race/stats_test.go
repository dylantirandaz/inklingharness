package fixture

import (
	"sync"
	"testing"
)

func TestStatsConcurrent(t *testing.T) {
	s := NewStats()
	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			path := "/even"
			if worker%2 == 1 {
				path = "/odd"
			}
			for range 1000 {
				s.Record(path)
				_ = s.Hits()
			}
		}()
	}
	wait.Wait()
	if got := s.Hits(); got != 8000 {
		t.Errorf("Hits() = %d, want 8000", got)
	}
	if got := s.PathHits("/odd"); got != 4000 {
		t.Errorf("PathHits(/odd) = %d, want 4000", got)
	}
}
