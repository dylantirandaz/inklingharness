package terminal

import (
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// The replies to the background query must decide the shade and must not
// swallow or reorder keys typed around them.
func TestBackgroundRepliesAndTypedInput(t *testing.T) {
	tests := []struct {
		name      string
		received  string
		found     bool
		shade     presentation.Shade
		reported  bool
		remaining string
	}{
		{"light with ST", "\x1b]11;rgb:ffff/ffff/ffff\x1b\\\x1b[?62;22c", true, presentation.LightBackground, true, ""},
		{"dark with BEL", "\x1b]11;rgb:1e1e/1e1e/2e2e\a\x1b[?1;2c", true, presentation.DarkBackground, true, ""},
		{"two hex digits", "\x1b]11;rgb:fd/f6/e3\a\x1b[?6c", true, presentation.LightBackground, true, ""},
		{"no OSC 11 support", "\x1b[?1;2c", true, presentation.DarkBackground, false, ""},
		{"typed keys around replies", "ab\x1b]11;rgb:0000/0000/0000\x1b\\c\x1b[?1;2cd", true, presentation.DarkBackground, true, "abcd"},
		{"incomplete DA1", "\x1b]11;rgb:ffff/ffff/ffff\x1b\\\x1b[?62;2", false, presentation.DarkBackground, false, ""},
		{"malformed color", "\x1b]11;rgb:zz/00/00\a\x1b[?1c", true, presentation.DarkBackground, false, ""},
	}
	for _, test := range tests {
		replies, rest, found := cutDeviceAttributes([]byte(test.received))
		if found != test.found {
			t.Fatalf("%s: found = %t", test.name, found)
		}
		if !found {
			continue
		}
		if string(rest) != test.remaining {
			t.Fatalf("%s: typed input = %q, want %q", test.name, rest, test.remaining)
		}
		shade, reported := backgroundShade(replies)
		if shade != test.shade || reported != test.reported {
			t.Fatalf("%s: shade %d reported %t, want %d %t", test.name, shade, reported, test.shade, test.reported)
		}
	}
}
