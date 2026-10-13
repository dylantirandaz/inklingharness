package tools

import (
	"strings"
	"testing"
)

func TestCleanTerminalText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "a\nb\n", "a\nb\n"},
		{"progress", "Reading ... 5%\rReading ... 10%\rReading ... 100%\ndone\n", "Reading ... 100%\ndone\n"},
		{"partial overwrite", "abcdef\rXY\n", "XYcdef\n"},
		{"crlf kept", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"progress then crlf", "1%\r50%\r100%\r\n", "100%\r\n"},
		{"no final newline", "x\ry", "y"},
		{"trailing carriage return", "abc\r", "abc"},
		{"colors", "\x1b[1;31merror\x1b[0m: bad\n", "error: bad\n"},
		{"cursor", "\x1b[2K\x1b[1Gline\n", "line\n"},
		{"osc title", "\x1b]0;title\x07ok\n", "ok\n"},
		{"osc st", "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\\n", "link\n"},
		{"charset", "\x1b(Bok\n", "ok\n"},
		{"lone escape", "a\x1b", "a\x1b"},
		{"unterminated csi", "a\x1b[12", "a\x1b[12"},
		{"utf8 overlay", "héllo\rHÉ\n", "HÉllo\n"},
		{"invalid utf8", "\xff\x1b[0m\xfe\n", "\xff\xfe\n"},
	}
	for _, c := range cases {
		if got := cleanTerminalText(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOutputCollectorCleansTerminalText(t *testing.T) {
	c := newOutputCollector(t.TempDir(), func(string) bool { return false })
	_, _ = c.Write([]byte("\x1b[32m0%\r50%\r100%\x1b[0m\nok\n"))
	if got := c.finish(); got != "100%\nok\n" {
		t.Fatalf("got %q", got)
	}
}

func TestOutputCollectorCleansTruncatedText(t *testing.T) {
	c := newOutputCollector(t.TempDir(), func(string) bool { return false })
	progress := strings.Repeat("x\r", outputInlineBytes) + "y"
	_, _ = c.Write([]byte("\x1b[1mstart\x1b[0m\n" + progress + "\nend\x1b[0m\n"))
	got := c.finish()
	defer c.discard()
	if strings.Contains(got, "\x1b") || strings.Count(got, "\r") != 0 {
		t.Fatalf("escape or carriage return kept: %q", got[:min(len(got), 200)])
	}
	if !strings.HasPrefix(got, "start\n") || !strings.HasSuffix(got, "end\n") || !strings.Contains(got, "[output truncated:") {
		t.Fatalf("unexpected result: %q", got[:min(len(got), 200)])
	}
}
