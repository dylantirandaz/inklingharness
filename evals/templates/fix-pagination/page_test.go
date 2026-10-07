package fixture

import (
	"slices"
	"testing"
)

func TestPage(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	cases := []struct {
		page, size int
		want       []string
	}{
		{1, 2, []string{"a", "b"}},
		{2, 2, []string{"c", "d"}},
		{3, 2, []string{"e"}},
		{4, 2, nil},
		{1, 5, items},
		{1, 10, items},
		{0, 2, nil},
		{1, 0, nil},
	}
	for _, c := range cases {
		if got := Page(items, c.page, c.size); !slices.Equal(got, c.want) {
			t.Errorf("Page(items, %d, %d) = %v, want %v", c.page, c.size, got, c.want)
		}
	}
}
