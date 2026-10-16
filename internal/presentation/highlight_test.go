package presentation

import (
	"regexp"
	"strings"
	"testing"
)

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripSGR(text string) string { return sgr.ReplaceAllString(text, "") }

func TestHighlightColorsTokens(t *testing.T) {
	theme := NewTheme(TrueColor, DarkBackground, AccentGreen)
	state := codeState{lang: languageFor("go")}
	line := highlight(`	return fmt.Sprintf("%d", 42) // done`, &state, theme)
	for _, want := range []string{theme.Blue("return"), theme.Accent("Sprintf"), theme.Green(`"%d"`), theme.Amber("42"), theme.Graphite("// done")} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if got := stripSGR(line); got != `	return fmt.Sprintf("%d", 42) // done` {
		t.Fatalf("highlighting changed the text: %q", got)
	}
}

func TestHighlightCarriesBlockCommentsAndStrings(t *testing.T) {
	theme := NewTheme(TrueColor, DarkBackground, AccentGreen)
	state := codeState{lang: languageFor("python")}
	highlight(`doc = """first`, &state, theme)
	if state.str != `"""` {
		t.Fatalf("triple quote should stay open, got %q", state.str)
	}
	line := highlight(`last""" + x`, &state, theme)
	if state.str != "" || !strings.HasPrefix(line, theme.Green(`last"""`)) {
		t.Fatalf("triple quote should close: %q", line)
	}
	state = codeState{lang: languageFor("ts title=app.ts")}
	highlight("/* open", &state, theme)
	if !state.comment {
		t.Fatal("block comment should stay open")
	}
	highlight("still */ let x = 1", &state, theme)
	if state.comment {
		t.Fatal("block comment should close")
	}
}

func TestMarkdownPreviewDoesNotAdvanceCodeState(t *testing.T) {
	theme := NewTheme(TrueColor, DarkBackground, AccentGreen)
	markdown := NewMarkdown(theme)
	markdown.Line("```go")
	markdown.Preview("/* partial")
	if markdown.code.comment {
		t.Fatal("a preview must not open a comment")
	}
	if got := markdown.Line("x := 1"); !strings.Contains(got, theme.Amber("1")) {
		t.Fatalf("fence line should be highlighted: %q", got)
	}
	markdown.Line("```")
	if markdown.code.lang != nil {
		t.Fatal("closing the fence should drop the language")
	}
}

func TestHighlightLeavesUnknownAndUncoloredText(t *testing.T) {
	plain := NewTheme(NoColor, DarkBackground, AccentGreen)
	state := codeState{lang: languageFor("go")}
	if got := highlight(`x := "a"`, &state, plain); got != `x := "a"` {
		t.Fatalf("no-color output changed: %q", got)
	}
	if languageFor("brainfuck") != nil {
		t.Fatal("unknown languages must stay plain")
	}
}

func TestDiffFenceTintsLines(t *testing.T) {
	theme := NewTheme(TrueColor, LightBackground, AccentBlue)
	state := codeState{lang: languageFor("diff")}
	if got := highlight("+added", &state, theme); got != theme.Added("+added") {
		t.Fatalf("added line: %q", got)
	}
	if got := highlight("-gone", &state, theme); got != theme.Removed("-gone") {
		t.Fatalf("removed line: %q", got)
	}
	if theme.Added("x") == theme.Green("x") {
		t.Fatal("truecolor diff lines should carry a tint")
	}
}

func BenchmarkHighlightLine(b *testing.B) {
	theme := NewTheme(TrueColor, DarkBackground, AccentGreen)
	line := `	if err := json.Unmarshal(data, &value); err != nil { return fmt.Errorf("decode %s: %w", name, err) } // 42`
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		state := codeState{lang: goLang}
		highlight(line, &state, theme)
	}
}
