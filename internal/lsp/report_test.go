package lsp

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func at(level severity, line, character int, message string) diagnostic {
	var item diagnostic
	item.Range.Start = position{Line: line, Character: character}
	item.Severity = level
	item.Message = message
	return item
}

func TestReport(t *testing.T) {
	var many []diagnostic
	var firstTwenty []string
	for line := range 25 {
		many = append(many, at(severityError, line, 0, "bad"))
		if line < maxReportLines {
			firstTwenty = append(firstTwenty, fmt.Sprintf("a.go:%d:1: error: bad", line+1))
		}
	}
	long := strings.Repeat("é", maxMessageRunes+1)
	tests := []struct {
		name        string
		path        string
		diagnostics []diagnostic
		want        string
	}{
		{name: "no diagnostics", path: "/work/a.go", want: ""},
		{
			name:        "only information, hints, and no severity",
			path:        "/work/a.go",
			diagnostics: []diagnostic{at(severityInformation, 0, 0, "info"), at(severityHint, 1, 0, "hint"), at(0, 2, 0, "none"), at(9, 3, 0, "unknown")},
			want:        "",
		},
		{
			name: "errors first, then by position",
			path: "/work/pkg/a.go",
			diagnostics: []diagnostic{
				at(severityWarning, 0, 4, "unused"),
				at(severityError, 9, 2, "late"),
				at(severityHint, 0, 0, "hint"),
				at(severityError, 2, 7, "second"),
				at(severityError, 2, 0, "first"),
			},
			want: "pkg/a.go:3:1: error: first\npkg/a.go:3:8: error: second\npkg/a.go:10:3: error: late\npkg/a.go:1:5: warning: unused",
		},
		{name: "outside the root", path: "/other/a.go", diagnostics: []diagnostic{at(severityError, 0, 0, "x")}, want: "/other/a.go:1:1: error: x"},
		{name: "sibling with the root as prefix", path: "/workspace/a.go", diagnostics: []diagnostic{at(severityError, 0, 0, "x")}, want: "/workspace/a.go:1:1: error: x"},
		{name: "message on one line", path: "/work/a.go", diagnostics: []diagnostic{at(severityError, 0, 0, "  line one\n\tline two\r\n")}, want: "a.go:1:1: error: line one line two"},
		{name: "long message", path: "/work/a.go", diagnostics: []diagnostic{at(severityError, 0, 0, long)}, want: "a.go:1:1: error: " + strings.Repeat("é", maxMessageRunes-1) + "…"},
		{name: "message at the limit", path: "/work/a.go", diagnostics: []diagnostic{at(severityError, 0, 0, long[len("é"):])}, want: "a.go:1:1: error: " + long[len("é"):]},
		{name: "exactly twenty", path: "/work/a.go", diagnostics: many[:maxReportLines], want: strings.Join(firstTwenty, "\n")},
		{name: "more than twenty", path: "/work/a.go", diagnostics: many, want: strings.Join(firstTwenty, "\n") + "\n(5 more)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := fmt.Sprint(test.diagnostics)
			got := report("/work", test.path, test.diagnostics)
			if got != test.want {
				t.Fatalf("report =\n%s\nwant\n%s", got, test.want)
			}
			if after := fmt.Sprint(test.diagnostics); after != before {
				t.Fatalf("report changed its input:\n%s\nwas\n%s", after, before)
			}
		})
	}
	if count := utf8.RuneCountInString(shortMessage(long)); count != maxMessageRunes {
		t.Fatalf("a cut message has %d characters, want %d", count, maxMessageRunes)
	}
}

func TestURIPath(t *testing.T) {
	for _, path := range []string{"/work/a.go", "/work/a b/c#d%e?.go", "/wörk/ü.go", "/a:b/c.go"} {
		uri := fileURI(path)
		got, valid := uriPath(uri)
		if !valid || got != path {
			t.Errorf("uriPath(%q) = %q, %v; want %q", uri, got, valid, path)
		}
	}
	tests := []struct {
		uri   string
		path  string
		valid bool
	}{
		{uri: "file:///a%3Ab/c.go", path: "/a:b/c.go", valid: true},
		{uri: "file://localhost/a/c.go", path: "/a/c.go", valid: true},
		{uri: "file:///a/./b/../c.go", path: "/a/c.go", valid: true},
		{uri: "file://server/a/c.go"},
		{uri: "untitled:Untitled-1"},
		{uri: "https://example.com/a.go"},
		{uri: "file:a.go"},
		{uri: "%zz"},
	}
	for _, test := range tests {
		got, valid := uriPath(test.uri)
		if got != test.path || valid != test.valid {
			t.Errorf("uriPath(%q) = %q, %v; want %q, %v", test.uri, got, valid, test.path, test.valid)
		}
	}
}
