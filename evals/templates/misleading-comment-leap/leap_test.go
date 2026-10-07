package fixture

import (
	"testing"
	"time"
)

func TestIsLeap(t *testing.T) {
	cases := map[int]bool{1900: false, 2000: true, 2023: false, 2024: true, 2100: false, 2400: true}
	for year, want := range cases {
		if got := IsLeap(year); got != want {
			t.Errorf("IsLeap(%d) = %v, want %v", year, got, want)
		}
	}
}

func TestDaysIn(t *testing.T) {
	if got := DaysIn(1900, time.February); got != 28 {
		t.Errorf("DaysIn(1900, February) = %d, want 28", got)
	}
	if got := DaysIn(2000, time.February); got != 29 {
		t.Errorf("DaysIn(2000, February) = %d, want 29", got)
	}
	if got := DaysIn(2023, time.April); got != 30 {
		t.Errorf("DaysIn(2023, April) = %d, want 30", got)
	}
}
