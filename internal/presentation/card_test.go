package presentation

import (
	"reflect"
	"strings"
	"testing"
)

// A card is a frame: every row is exactly the width, the edges close it, and
// a styled row keeps its text inside the two sides.
func TestCardFramesEveryRowAtTheWidth(t *testing.T) {
	theme := NewTheme(TrueColor, LightBackground, AccentPlum)
	rows := theme.Card([]string{theme.Graphite("quiet") + " text " + theme.Bold("bold"), ""}, 30)
	if len(rows) != 4 {
		t.Fatalf("card has %d rows, want the 2 rows and 2 edges: %q", len(rows), rows)
	}
	for index, row := range rows {
		plain := Safe(row)
		if width := CellWidth(plain); width != 30 {
			t.Fatalf("row %d is %d cells wide: %q", index, width, plain)
		}
		first, last := []rune(plain)[0], []rune(plain)[len([]rune(plain))-1]
		switch index {
		case 0:
			if first != '┌' || last != '┐' {
				t.Fatalf("top edge = %q", plain)
			}
		case len(rows) - 1:
			if first != '└' || last != '┘' {
				t.Fatalf("bottom edge = %q", plain)
			}
		default:
			if first != '│' || last != '│' {
				t.Fatalf("row %d is not inside the frame: %q", index, plain)
			}
		}
	}
	if !strings.Contains(Safe(rows[1]), "quiet text bold") {
		t.Fatalf("the text left the frame: %q", Safe(rows[1]))
	}
}

// A request is a closed box after a row of air: every row is the same width
// and stays inside the screen, the box holds the whole work, even a line
// longer than the screen, a removed line takes the removed tint, and the
// caption closes the box.
func TestApprovalRowsBoxTheWholeWork(t *testing.T) {
	theme := NewTheme(TrueColor, DarkBackground, AccentPlum)
	long := "+ " + strings.Repeat("abcdefghij", 12)
	request := NewApprovalRequest("edit_file", ToolView{
		Title:   "Edit app.py",
		Details: "Replace one exact occurrence in app.py\n- old line\n" + long,
	})
	if request.Title != "Edit app.py" || !strings.HasPrefix(request.Caption, "Replace one exact occurrence in app.py") {
		t.Fatalf("label = %q / %q", request.Title, request.Caption)
	}
	if len(request.Work) != 2 || request.Work[0].Kind != WorkRemoved || request.Work[1].Kind != WorkAdded {
		t.Fatalf("work = %+v", request.Work)
	}
	for _, air := range []bool{false, true} {
		rows := theme.ApprovalRows(request, 70, air)
		if rows[0] != "" || !strings.HasPrefix(strings.TrimSpace(Safe(rows[1])), "╭") || !strings.HasPrefix(strings.TrimSpace(Safe(rows[len(rows)-1])), "╰") {
			t.Fatalf("the request is not a box after a row of air: %q", rows)
		}
		var shown strings.Builder
		for _, row := range rows[1:] {
			plain := Safe(row)
			if CellWidth(plain) != 70 {
				t.Fatalf("box row is %d cells, want 70: %q", CellWidth(plain), plain)
			}
			shown.WriteString(strings.TrimSpace(strings.Trim(strings.TrimSpace(plain), "│")))
			if strings.Contains(plain, "- old line") && !strings.Contains(row, theme.removed) {
				t.Fatalf("the removed line has no removed tint: %q", row)
			}
		}
		if !strings.Contains(shown.String(), strings.TrimPrefix(long, "+ ")) || !strings.Contains(shown.String(), "- old line") {
			t.Fatalf("the box lost part of the work (air %t): %q", air, shown.String())
		}
		if caption := Safe(rows[len(rows)-2]); !strings.Contains(caption, "not sandboxed") {
			t.Fatalf("the caption does not close the box: %q", caption)
		}
	}
}

// A command is the work itself, so the title names only the action.
func TestApprovalRequestShowsTheObjectOnce(t *testing.T) {
	command := NewApprovalRequest("bash", ToolView{Title: "Run go test ./...", Details: "Bash · timeout 120s\ngo test ./..."})
	if command.Title != "Run a command" || len(command.Work) != 1 || command.Work[0].Text != "go test ./..." {
		t.Fatalf("command request = %+v", command)
	}
	if !strings.HasPrefix(command.Caption, "Bash · timeout 120s") {
		t.Fatalf("caption = %q", command.Caption)
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

func TestMarkdownTracksTheOpenFence(t *testing.T) {
	m := NewMarkdown(Theme{})
	for _, test := range []struct {
		line string
		code bool
	}{
		{"plain text", false},
		{"```go", true},
		{"x := 1", true},
		{"```", false},
		{"after", false},
	} {
		m.Line(test.line)
		if m.InCode() != test.code {
			t.Fatalf("%q: in code %t", test.line, m.InCode())
		}
	}
	m.Line("```")
	m.EndCode()
	if m.InCode() || m.Line("*plain*") != "plain" {
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
