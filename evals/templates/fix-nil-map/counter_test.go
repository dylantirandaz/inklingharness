package fixture

import (
	"slices"
	"testing"
)

func TestZeroCounter(t *testing.T) {
	var c Counter
	if got := c.Count("go"); got != 0 {
		t.Fatalf("Count on empty counter = %d, want 0", got)
	}
	if got := c.Words(); len(got) != 0 {
		t.Fatalf("Words on empty counter = %v, want none", got)
	}
	for _, word := range []string{"go", "test", "go"} {
		c.Add(word)
	}
	if got := c.Count("go"); got != 2 {
		t.Errorf("Count(go) = %d, want 2", got)
	}
	if got := c.Words(); !slices.Equal(got, []string{"go", "test"}) {
		t.Errorf("Words() = %v, want [go test]", got)
	}
}
