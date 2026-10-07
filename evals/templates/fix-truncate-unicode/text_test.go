package fixture

import (
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		text  string
		limit int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello…"},
		{"héllo wörld", 4, "héll…"},
		{"日本語テキスト", 2, "日本…"},
		{"日本語", 3, "日本語"},
		{"abc", 0, "…"},
		{"", 0, ""},
	}
	for _, c := range cases {
		got := Truncate(c.text, c.limit)
		if got != c.want || !utf8.ValidString(got) {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.text, c.limit, got, c.want)
		}
	}
}
