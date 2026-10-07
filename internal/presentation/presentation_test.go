package presentation

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

func TestSafeControls(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"unicode", "héllo 世界\n\tcode", "héllo 世界\n\tcode"},
		{"csi", "before\x1b[2J\x1b[Hafter", "beforeafter"},
		{"osc", "a\x1b]52;c;clipboard\ab", "ab"},
		{"hyperlink", "\x1b]8;;https://host\x1b\\label\x1b]8;;\x1b\\", "label"},
		{"dcs", "a\x1bPpayload\x1b\\b", "ab"},
		{"apc", "a\x1b_payload\x1b\\b", "ab"},
		{"c1", "a\u009b2Jb\u009dpayload\u009cc", "abc"},
		{"c0", "a\x00\x08\r\x7fb", "ab"},
		{"bidi", "a\u202erisk\u202cb", "ariskb"},
		{"unfinished escape", "safe\x1b", "safe"},
		{"unfinished csi", "safe\x1b[31", "safe"},
		{"unfinished osc", "safe\x1b]52;secret", "safe"},
		{"escape intermediate", "a\x1b(Bb", "ab"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Safe(test.input); got != test.want {
				t.Fatalf("Safe(%q) = %q; want %q", test.input, got, test.want)
			}
		})
	}
}

func TestSafeSplitEscapesNeverReassemble(t *testing.T) {
	for _, input := range []string{"\x1b[2J", "\x1b]52;c;payload\a", "\x1bPpayload\x1b\\", "\u009b2J"} {
		for split := 0; split <= len(input); split++ {
			got := Safe(input[:split]) + Safe(input[split:])
			for _, r := range got {
				if unicode.IsControl(r) && r != '\n' && r != '\t' {
					t.Fatalf("split %d emitted control %U from %q", split, r, input)
				}
			}
		}
	}
}

// themes covers each color depth; the zero Theme is NoColor.
var themes = []Theme{{}, NewTheme(ANSI16), NewTheme(ANSI256), NewTheme(TrueColor)}

func TestMarkdownFenceStateAndPreview(t *testing.T) {
	m := NewMarkdown(Theme{})
	m.Preview("```go")
	if got := m.Line("**outside**"); got != "outside" {
		t.Fatalf("preview changed fence state: %q", got)
	}
	m.Line("````go")
	literal := "\tif a*b == `x_y` { // **literal** [x](url) }"
	if got := m.Line(literal); got != literal {
		t.Fatalf("code changed: %q", got)
	}
	m.Line("```")
	if got := m.Line("**still code**"); got != "**still code**" {
		t.Fatalf("short fence closed code: %q", got)
	}
	m.Preview("````")
	if got := m.Line("*still code*"); got != "*still code*" {
		t.Fatalf("closing preview changed state: %q", got)
	}
	m.Line("````")
	if got := m.Line("**outside**"); got != "outside" {
		t.Fatalf("fence did not close: %q", got)
	}
	m.Line("~~~text")
	if got := m.Line("raw\x1b[2J text"); got != "raw text" {
		t.Fatalf("code did not sanitize: %q", got)
	}
	m.Line("~~~")
}

func TestMarkdownInlineAndIncomplete(t *testing.T) {
	tests := []struct{ input, want string }{
		{"# Heading", "Heading"},
		{"- item", "▸ item"},
		{"**bold** and *emphasis*", "bold and emphasis"},
		{"snake_case_identifier", "snake_case_identifier"},
		{"`a*b _c_ [x](y)`", "`a*b _c_ [x](y)`"},
		{"``a ` b *c*``", "``a ` b *c*``"},
		{"*a `*` b*", "a `*` b"},
		{"[guide](https://example.test/a(b))", "guide (https://example.test/a(b))"},
		{"**unfinished", "**unfinished"},
		{"`unfinished *literal*", "`unfinished *literal*"},
		{"[unfinished](url", "[unfinished](url"},
	}
	for _, test := range tests {
		for _, theme := range themes {
			m := NewMarkdown(theme)
			got := m.Preview(test.input)
			if !theme.Colored() && strings.Contains(got, "\x1b") {
				t.Fatalf("plain renderer emitted ANSI: %q", got)
			}
			if Safe(got) != test.want {
				t.Fatalf("render(%q, %d) = %q; want %q", test.input, theme.depth, got, test.want)
			}
		}
	}
}

func TestFullApprovalContent(t *testing.T) {
	content := strings.Repeat("line with *symbols*\n", 150) + "final unique line"
	writeInput, err := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{"target.go", content})
	if err != nil {
		t.Fatal(err)
	}
	color := NewTheme(TrueColor)
	view, err := DescribeTool("write_file", writeInput, color)
	if err != nil {
		t.Fatal(err)
	}
	plain := Safe(view.Details)
	for line := range strings.SplitSeq(content, "\n") {
		if !strings.Contains(plain, "+ "+line) {
			t.Fatalf("write approval omitted %q", line)
		}
	}
	if strings.Count(plain, "+ line with *symbols*") != 150 {
		t.Fatal("write content was abbreviated")
	}
	editInput := json.RawMessage(`{"path":"target.go","old_string":"old first\nold last\n","new_string":"new first\nnew last"}`)
	view, err = DescribeTool("edit_file", editInput, color)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"- old first", "- old last", "+ new first", "+ new last", "No newline"} {
		if !strings.Contains(Safe(view.Details), fragment) {
			t.Fatalf("edit approval omitted %q", fragment)
		}
	}
	if !strings.Contains(view.Details, color.sgr[roleEmber]) || !strings.Contains(view.Details, color.sgr[roleVerdant]) {
		t.Fatal("replacement diff lacks old/new colors")
	}
	view, err = DescribeTool("bash", json.RawMessage(`{"command":"printf start\nprintf end\n# final command","timeout_seconds":42}`), Theme{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Details, "printf start\nprintf end\n# final command") || !strings.Contains(view.Details, "42") {
		t.Fatalf("incomplete Bash approval: %q", view.Details)
	}
}

func TestMalformedToolInputs(t *testing.T) {
	tests := []struct{ name, input string }{
		{"missing", `{}`}, {"read_file", `null`}, {"read_file", `[]`},
		{"read_file", `{`}, {"read_file", `{}`}, {"read_file", `{"path":3}`},
		{"read_file", `{"path":"x","offset":0}`}, {"write_file", `{"path":"x"}`},
		{"write_file", `{"path":"x","content":null}`},
		{"edit_file", `{"path":"x","old_string":"","new_string":"y"}`},
		{"bash", `{"command":"x","timeout_seconds":601}`}, {"bash", `{"command":"x","timeout_seconds":1.5}`},
		{"grep", `{"pattern":"x","case_sensitive":"yes"}`}, {"glob", `{}`},
		{"todo_write", `{"items":null}`}, {"todo_write", `{"items":[{"content":"x","status":"unknown"}]}`},
		{"todo_write", `{"items":[{"content":"x","status":"in_progress"},{"content":"y","status":"in_progress"}]}`},
		{"task", `{"prompt":false}`},
	}
	for _, test := range tests {
		if _, err := DescribeTool(test.name, json.RawMessage(test.input), Theme{}); err == nil {
			t.Errorf("accepted %s %s", test.name, test.input)
		}
		if _, err := ToolTitle(test.name, json.RawMessage(test.input)); err == nil {
			t.Errorf("title accepted %s %s", test.name, test.input)
		}
	}
}

func TestToolViewsAndBannerSanitize(t *testing.T) {
	tests := []struct{ name, input string }{
		{"read_file", `{"path":"a\u001b[2Jb"}`},
		{"write_file", `{"path":"x","content":"a\u001b]52;c;secret\u0007b"}`},
		{"edit_file", `{"path":"x","old_string":"a\u001b[2J","new_string":"b"}`},
		{"list_dir", `{"path":"a\u001b[2J"}`}, {"glob", `{"pattern":"**/\u001b[2J*.go"}`},
		{"grep", `{"pattern":"needle\u001b[2J","include":"*.go","case_sensitive":false}`},
		{"bash", `{"command":"printf hi\u001b[2J"}`},
		{"todo_write", `{"items":[{"content":"a\u001b[2J","status":"pending"}]}`},
		{"task", `{"prompt":"work\u001b[2J"}`},
	}
	for _, test := range tests {
		view, err := DescribeTool(test.name, json.RawMessage(test.input), Theme{})
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if strings.Contains(view.Title+view.Details, "\x1b") {
			t.Fatalf("unsafe view for %s", test.name)
		}
		title, err := ToolTitle(test.name, json.RawMessage(test.input))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(title, "\x1b") {
			t.Fatalf("unsafe activity label for %s", test.name)
		}
	}
	banner := Banner("model\x1b[2J", "high", "folder\nspoof", 80, Theme{})
	if strings.Contains(banner, "\x1b") {
		t.Fatalf("unsafe banner: %q", banner)
	}
}

func TestResultPreviewBudgetAndDisclosure(t *testing.T) {
	content := "first\nsecond\nthird\nlast failure\n"
	for _, failure := range []bool{false, true} {
		for budget := 1; budget <= 4; budget++ {
			got := ResultPreview(content, failure, budget, Theme{})
			if len(strings.Split(got, "\n")) > budget {
				t.Fatalf("preview exceeded %d lines: %q", budget, got)
			}
			if budget < 4 && (!strings.Contains(got, "omitted") || !strings.Contains(got, "/tools")) {
				t.Fatalf("missing truncation disclosure: %q", got)
			}
			if failure && budget > 1 && !strings.Contains(got, "last failure") {
				t.Fatalf("failure tail missing: %q", got)
			}
		}
	}
	if got := ResultPreview(content, false, 0, Theme{}); got != "" {
		t.Fatalf("zero budget returned %q", got)
	}
	if got := ResultPreview("a\x1b[2Jb", false, 3, Theme{}); got != "ab" {
		t.Fatalf("unsafe result: %q", got)
	}
}

func TestApprovalFiltersTerminalControls(t *testing.T) {
	view := ToolView{
		Title:   "tool\x1b[2J\nspoof",
		Details: "before\x1b]52;c;hidden\aafter\n\tcode",
	}
	for _, theme := range themes {
		approval := Approval(view, theme)
		if strings.Contains(approval, "\x1b[2J") || strings.Contains(approval, "hidden") {
			t.Fatalf("unsafe approval: %q", approval)
		}
		if !theme.Colored() && strings.Contains(approval, "\x1b") {
			t.Fatalf("plain approval contains styles: %q", approval)
		}
		if !strings.Contains(Safe(approval), "beforeafter") {
			t.Fatalf("approval lost visible content: %q", approval)
		}
	}
}

func TestMarkdownPreservesIndentedCode(t *testing.T) {
	for _, theme := range themes {
		for _, text := range []string{
			"    *literal* [link](target) `code`",
			"\t**literal** _identifier_",
			"    - not a list",
			"    ```not a fence",
		} {
			m := NewMarkdown(theme)
			if got := m.Preview(text); got != text {
				t.Fatalf("code preview changed: %q", got)
			}
			if got := m.Line(text); got != text {
				t.Fatalf("code line changed: %q", got)
			}
			if got := Safe(m.Line("**outside**")); got != "outside" {
				t.Fatalf("indented fence changed block state: %q", got)
			}
		}
	}
}

func TestCompactViewsRemainSafeWithoutColor(t *testing.T) {
	unsafe := "label\x1b[2J\nsecond"
	for _, theme := range themes {
		for _, view := range []string{
			Section(unsafe, theme),
			ToolCompletion(unsafe, false, theme),
			ToolCompletion(unsafe, true, theme),
			Banner(unsafe, unsafe, unsafe, 40, theme),
			UserTurn(unsafe, theme),
		} {
			if strings.Contains(view, "\x1b[2J") || (!theme.Colored() && strings.Contains(view, "\x1b")) {
				t.Fatalf("unsafe compact view: %q", view)
			}
			if !strings.Contains(Safe(view), "label") || !strings.Contains(Safe(view), "second") {
				t.Fatal("compact view lost its label")
			}
		}
		if Safe(ToolCompletion("tool", false, theme)) == Safe(ToolCompletion("tool", true, theme)) {
			t.Fatal("tool outcome depends on color")
		}
	}
}

func TestColorDepthDetection(t *testing.T) {
	tests := []struct {
		environment map[string]string
		want        ColorDepth
	}{
		{map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}, NoColor},
		{map[string]string{"TERM": "dumb", "COLORTERM": "truecolor"}, NoColor},
		{map[string]string{"COLORTERM": "TrueColor", "TERM": "xterm"}, TrueColor},
		{map[string]string{"COLORTERM": "24bit"}, TrueColor},
		{map[string]string{"TERM": "xterm-256color"}, ANSI256},
		{map[string]string{"TERM": "xterm"}, ANSI16},
	}
	for _, test := range tests {
		if got := DetectColorDepth(func(name string) string { return test.environment[name] }); got != test.want {
			t.Errorf("%v: depth %d, want %d", test.environment, got, test.want)
		}
	}
}

// Decoration must never change the visible text or its width, or the screen
// diff and cursor math would drift.
func TestThemeDecorationKeepsVisibleText(t *testing.T) {
	text := "Compacting context ◈ 界"
	for _, theme := range themes {
		for phase := range 40 {
			if got := Safe(theme.Shimmer(text, phase)); got != text {
				t.Fatalf("shimmer depth %d phase %d changed text: %q", theme.depth, phase, got)
			}
		}
		if got := Safe(theme.Gradient(text)); got != text {
			t.Fatalf("gradient changed text: %q", got)
		}
		for _, width := range []int{1, 7, 80, 233} {
			if got := Safe(theme.Rule(width)); got != strings.Repeat("━", width) {
				t.Fatalf("rule width %d = %q", width, got)
			}
		}
		for used, want := range map[int]string{0: "▱▱▱▱", 50: "▰▰▱▱", 100: "▰▰▰▰", 250: "▰▰▰▰"} {
			if got := Safe(theme.Gauge(used, 100, 4)); got != want {
				t.Fatalf("gauge %d = %q, want %q", used, got, want)
			}
		}
	}
	if NewTheme(TrueColor).Rule(400) == NewTheme(TrueColor).Rule(399) || strings.Count(NewTheme(TrueColor).Rule(400), "\x1b[38;2;") > ruleSegments {
		t.Fatal("a wide rule must stay within the segment budget")
	}
}

func TestNearest256UsesCubeAndGray(t *testing.T) {
	for _, test := range []struct {
		color rgb
		want  int
	}{{rgb{0, 0, 0}, 16}, {rgb{255, 255, 255}, 231}, {rgb{255, 0, 0}, 196}, {rgb{128, 128, 128}, 244}, {rgb{94, 242, 232}, 86}} {
		if got := nearest256(test.color); got != test.want {
			t.Errorf("nearest256(%v) = %d, want %d", test.color, got, test.want)
		}
	}
}
