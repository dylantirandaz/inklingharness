package fixture

import (
	"testing"
	"time"
)

func TestDelay(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: 5 * time.Second}
	cases := map[int]time.Duration{
		0:   100 * time.Millisecond,
		1:   200 * time.Millisecond,
		3:   800 * time.Millisecond,
		5:   3200 * time.Millisecond,
		6:   5 * time.Second,
		30:  5 * time.Second,
		40:  5 * time.Second,
		63:  5 * time.Second,
		64:  5 * time.Second,
		200: 5 * time.Second,
	}
	for attempt, want := range cases {
		if got := b.Delay(attempt); got != want {
			t.Errorf("Delay(%d) = %v, want %v", attempt, got, want)
		}
	}
}
