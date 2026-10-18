package presentation

import (
	"reflect"
	"strings"
	"testing"
)

// A card row fills its width, and the tint survives the resets inside the
// row, so a styled word never leaves a hole in the card.
func TestCardRowFillsWidthAndKeepsTint(t *testing.T) {
	theme := NewTheme(TrueColor, LightBackground, AccentPlum)
	row := theme.Graphite("quiet") + " text " + theme.Bold("bold")
	got := theme.CardRow(row, 30)
	if width := CellWidth(Safe(got)); width != 30 {
		t.Fatalf("card row is %d cells wide: %q", width, Safe(got))
	}
	parts := strings.Split(got, reset)
	for index, part := range parts[:len(parts)-1] {
		if !strings.HasPrefix(part, theme.chip) {
			t.Fatalf("part %d of the card row has lost the tint: %q", index, part)
		}
	}
	if parts[len(parts)-1] != "" {
		t.Fatalf("the card row does not end on a reset: %q", got)
	}
}

// Below 256 colors there is no tint, so a card draws nothing of its own and
// the caller keeps its border.
func TestCardWithoutTintReturnsRowsUnchanged(t *testing.T) {
	rows := []string{"one", "two"}
	for _, theme := range []Theme{{}, NewTheme(ANSI16, DarkBackground, AccentBlue)} {
		if theme.Soft() {
			t.Fatalf("depth %d has a tint", theme.depth)
		}
		if got := theme.Card(rows, 30); !reflect.DeepEqual(got, rows) {
			t.Fatalf("card without tint changed its rows: %q", got)
		}
	}
}

func TestCardEdgesSpanTheWidth(t *testing.T) {
	theme := NewTheme(TrueColor, DarkBackground, AccentGreen)
	for _, edge := range []string{theme.CardTop(20), theme.CardBottom(20)} {
		if width := CellWidth(Safe(edge)); width != 20 {
			t.Fatalf("card edge is %d cells wide: %q", width, Safe(edge))
		}
	}
}

func TestWaffleCellsShareTheGrid(t *testing.T) {
	for _, test := range []struct {
		name     string
		values   []int
		capacity int
		cells    int
		want     []int
	}{
		{"proportional", []int{50, 25, 0}, 200, 40, []int{10, 5, 0}},
		{"a small part stays visible", []int{1}, 1_000_000, 10, []int{1}},
		{"over capacity fills the grid by proportion", []int{300, 100}, 100, 40, []int{30, 10}},
		{"the minimum never overflows the grid", []int{1, 1, 1, 1}, 1000, 3, []int{0, 1, 1, 1}},
		{"nothing to show", []int{0, 0}, 0, 10, []int{0, 0}},
	} {
		got := WaffleCells(test.values, test.capacity, test.cells)
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: %v, want %v", test.name, got, test.want)
		}
	}
}

// Without color a waffle still shows what is filled: a square for a filled
// cell and a dot for an empty one.
func TestWaffleShowsStateWithoutColor(t *testing.T) {
	got := (Theme{}).Waffle([]WaffleSegment{{Series: 0, Cells: 3}, {Series: 1, Cells: 4}}, 5, 3)
	want := []string{"▪▪▪▪▪", "▪▪···", "·····"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waffle = %q, want %q", got, want)
	}
}

func TestMarkdownBlockKinds(t *testing.T) {
	m := NewMarkdown(Theme{})
	for _, test := range []struct {
		line string
		kind BlockKind
		info string
		code bool
	}{
		{"plain text", ProseLine, "", false},
		{"```go", CodeStart, "go", true},
		{"x := 1", CodeLine, "", true},
		{"```", CodeEnd, "", false},
		{"after", ProseLine, "", false},
	} {
		block := m.Block(test.line)
		if block.Kind != test.kind || block.Info != test.info || m.InCode() != test.code {
			t.Fatalf("%q: kind %d info %q in code %t", test.line, block.Kind, block.Info, m.InCode())
		}
	}
	m.Block("```")
	m.EndCode()
	if m.InCode() || m.Block("*plain*").Text != "plain" {
		t.Fatal("EndCode did not leave the fence")
	}
}

// With color the color marks inline code and the backticks go; without color
// the backticks stay, so the code remains distinct in plain text. The code has
// no tint, so a word of code never breaks the line into boxes.
func TestInlineCodeKeepsDelimitersOnlyWithoutColor(t *testing.T) {
	for _, theme := range []Theme{
		NewTheme(TrueColor, LightBackground, AccentBlue),
		NewTheme(ANSI256, DarkBackground, AccentBlue),
		NewTheme(ANSI16, DarkBackground, AccentBlue),
	} {
		got := NewMarkdown(theme).Line("run `go test` now")
		if Safe(got) != "run go test now" || strings.Contains(got, theme.chip) && theme.chip != "" {
			t.Fatalf("inline code at depth %d = %q", theme.depth, got)
		}
	}
	if got := NewMarkdown(Theme{}).Line("run `go test` now"); got != "run `go test` now" {
		t.Fatalf("inline code without color = %q", got)
	}
}
