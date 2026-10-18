package presentation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func stepRows(t *testing.T, step ToolStep, width int) []string {
	t.Helper()
	theme := NewTheme(TrueColor, LightBackground, AccentPlum)
	card := theme.ToolCard(step, width)
	if len(card) < 3 {
		t.Fatalf("a card needs two edges and a title, got %d rows", len(card))
	}
	// The first and last rows are the edges of the card.
	rows := make([]string, 0, len(card)-2)
	for _, row := range card[1 : len(card)-1] {
		rows = append(rows, Safe(row))
	}
	return rows
}

// Every row of a card, whatever its content, is as wide as the card, so the
// tint reaches the same edge on all of them and no row wraps.
func TestToolCardRowsFillTheWidth(t *testing.T) {
	long := strings.Repeat("a long target ", 20)
	for _, width := range []int{24, 40, 80, 120} {
		for _, step := range []ToolStep{
			{Name: "bash", Title: "Run " + long, Content: "one\ntwo\n" + long + "\nfour\nfive", Elapsed: 1500 * time.Millisecond, Number: 12},
			{Name: "bash", Title: "Run go test", Content: "line\n[exit code 1]", Failed: true, Elapsed: 40 * time.Millisecond, Number: 3},
			{Name: "read_file", Title: "Read " + long, Content: "a\nb\n[end of file: 2 lines]", Number: 1},
			{Name: "edit_file", Title: "Edit a.go", Input: json.RawMessage(`{"path":"a.go","old_string":"x","new_string":"` + long + `"}`), Content: "edited a.go: now lines 4-4", Number: 2},
		} {
			for _, row := range stepRows(t, step, width) {
				if cells := CellWidth(row); cells != width {
					t.Fatalf("width %d, %s: row is %d cells: %q", width, step.Name, cells, row)
				}
			}
		}
	}
}

// The title keeps the verb, the count, the time, and the number of its tab,
// even when the target is far too long for the row.
func TestToolCardTitleKeepsItsPartsWhenTheTargetIsLong(t *testing.T) {
	rows := stepRows(t, ToolStep{Name: "read_file", Title: "Read " + strings.Repeat("deep/", 30) + "file.go", Content: "[end of file: 9 lines]", Elapsed: 12 * time.Millisecond, Number: 4}, 60)
	title := rows[0]
	for _, want := range []string{"✓", "Read", "…", "9 lines", "12ms", "#4"} {
		if !strings.Contains(title, want) {
			t.Fatalf("title is missing %q: %q", want, title)
		}
	}
}

// A read, a search, and a listing leave a count in the title and no rows of
// output; a count that was cut says so.
func TestToolCardCollapsesReadsSearchesAndListings(t *testing.T) {
	for _, test := range []struct {
		name, title, content, want string
	}{
		{"read_file", "Read a.go", "package a\n\n[end of file: 15 lines]", "15 lines"},
		{"read_file", "Read a.go", "x\n[truncated: line limit; continue with offset 2001]", "1 lines shown"},
		{"read_file", "Read empty.txt", "", "0 lines"},
		{"grep", "Search Lease in .", "a.go:1:x\nb.go:2:y\nc.go:3:z", "3 matches"},
		{"grep", "Search Lease in .", "a.go:1:x", "1 match"},
		{"grep", "Search Lease in .", "a.go:1:x\n[truncated: result or 256 KiB output limit]", "1 match shown"},
		{"glob", "Find files **/*.go in .", "a.go\nb.go", "2 files"},
		{"glob", "Find files **/*.go in .", "", "0 files"},
	} {
		rows := stepRows(t, ToolStep{Name: test.name, Title: test.title, Content: test.content}, 80)
		if len(rows) != 1 || !strings.Contains(rows[0], test.want) {
			t.Fatalf("%s %q: rows %q, want one row with %q", test.name, test.content, rows, test.want)
		}
	}
}

// An edit shows what it changed: the removed lines, then the added lines,
// numbered from the line that the result names.
func TestToolCardShowsAnEditAsADiff(t *testing.T) {
	input := json.RawMessage(`{"path":"a.go","old_string":"one\ntwo","new_string":"uno\ndos\ntres"}`)
	rows := stepRows(t, ToolStep{Name: "edit_file", Title: "Edit a.go", Input: input, Content: "edited a.go: now lines 9-11", Number: 1}, 80)
	want := []string{"9 - one", "10 - two", "9 + uno", "10 + dos", "11 + tres"}
	if len(rows) != 1+len(want) {
		t.Fatalf("diff has %d rows: %q", len(rows), rows)
	}
	for index, line := range want {
		if !strings.Contains(rows[index+1], line) {
			t.Fatalf("diff row %d = %q, want %q", index, rows[index+1], line)
		}
	}
	if !strings.Contains(rows[0], "line 9") {
		t.Fatalf("title does not name the line: %q", rows[0])
	}
}

// A long change is cut to the row budget, and a notice names the route to the
// rest.
func TestToolCardCutsALongDiff(t *testing.T) {
	input, err := json.Marshal(map[string]string{"path": "a.txt", "content": strings.Repeat("line\n", 40)})
	if err != nil {
		t.Fatal(err)
	}
	rows := stepRows(t, ToolStep{Name: "write_file", Title: "Write a.txt", Input: input, Content: "wrote 200 bytes to a.txt", Number: 5}, 80)
	if len(rows) != 1+diffRowLimit {
		t.Fatalf("a long diff has %d rows, want the title and %d", len(rows), diffRowLimit)
	}
	if last := rows[len(rows)-1]; !strings.Contains(last, "more lines") || !strings.Contains(last, "/tools 5") {
		t.Fatalf("the notice of a cut diff = %q", last)
	}
	if !strings.Contains(rows[0], "40 lines") {
		t.Fatalf("title does not count the lines: %q", rows[0])
	}
}

// A step that failed shows more of its output than one that worked, and the
// output of a step that worked is cut to a few lines with a route to the rest.
func TestToolCardOutputBudgets(t *testing.T) {
	output := strings.Repeat("output line\n", 20)
	worked := stepRows(t, ToolStep{Name: "bash", Title: "Run x", Content: output, Number: 2}, 80)
	if len(worked) != 1+outputRowLimit || !strings.Contains(worked[len(worked)-1], "/tools 2") {
		t.Fatalf("output of a step that worked: %q", worked)
	}
	failed := stepRows(t, ToolStep{Name: "bash", Title: "Run x", Content: output, Failed: true, Number: 2}, 80)
	if len(failed) != 1+failureRowLimit || !strings.Contains(failed[0], "✗") {
		t.Fatalf("output of a step that failed: %q", failed)
	}
}

// Below 256 colors there is no tint to draw a card on, and a card cannot fit
// in a few cells; the caller keeps the rows with a rail.
func TestToolCardNeedsATintAndRoom(t *testing.T) {
	step := ToolStep{Name: "bash", Title: "Run x", Content: "out"}
	for _, theme := range []Theme{{}, NewTheme(ANSI16, DarkBackground, AccentBlue)} {
		if theme.ToolCard(step, 80) != nil {
			t.Fatalf("depth %d drew a card", theme.depth)
		}
	}
	if NewTheme(TrueColor, LightBackground, AccentBlue).ToolCard(step, 12) != nil {
		t.Fatal("a card of 12 cells has no room for a title")
	}
}

// The tint carries the outcome: the same step on a different tone is a
// different card, and a failure is never drawn as a success.
func TestToolCardTintFollowsTheOutcome(t *testing.T) {
	theme := NewTheme(TrueColor, LightBackground, AccentPlum)
	worked := strings.Join(theme.ToolCard(ToolStep{Name: "bash", Title: "Run x"}, 60), "")
	failed := strings.Join(theme.ToolCard(ToolStep{Name: "bash", Title: "Run x", Failed: true}, 60), "")
	if !strings.Contains(worked, theme.background(successTint.on(LightBackground))) || strings.Contains(worked, theme.background(failureTint.on(LightBackground))) {
		t.Fatal("a step that worked is not on the success tint")
	}
	if !strings.Contains(failed, theme.background(failureTint.on(LightBackground))) || strings.Contains(failed, theme.background(successTint.on(LightBackground))) {
		t.Fatal("a step that failed is not on the failure tint")
	}
}

// ResultLines gives the lines that ResultPreview joins, with the disclosure
// marked as a notice.
func TestResultLinesMarkTheNotice(t *testing.T) {
	lines := ResultLines(strings.Repeat("x\n", 10), false, 4, "/tools 7")
	if len(lines) != 4 || !lines[3].Notice || lines[3].Text != "7 more lines · /tools 7" {
		t.Fatalf("lines = %+v", lines)
	}
	for _, line := range lines[:3] {
		if line.Notice || line.Text != "x" {
			t.Fatalf("a kept line = %+v", line)
		}
	}
	if ResultLines("anything", false, 0, "/tools") != nil || ResultLines("", false, 3, "/tools") != nil {
		t.Fatal("an empty preview must have no lines")
	}
}

func TestUserCardHoldsTheStampAndWrappedRows(t *testing.T) {
	theme := NewTheme(TrueColor, DarkBackground, AccentPlum)
	card := theme.UserCard([]string{"first row", "second row"}, "23:09", 40)
	if len(card) != 4 {
		t.Fatalf("card has %d rows", len(card))
	}
	plain := make([]string, len(card))
	for index, row := range card {
		plain[index] = Safe(row)
		if cells := CellWidth(plain[index]); cells != 40 {
			t.Fatalf("row %d is %d cells wide", index, cells)
		}
	}
	if !strings.Contains(plain[1], "› first row") || !strings.HasSuffix(strings.TrimRight(plain[1], " "), "23:09") {
		t.Fatalf("first row = %q", plain[1])
	}
	if strings.Contains(plain[2], "23:09") || !strings.Contains(plain[2], "  second row") {
		t.Fatalf("second row = %q", plain[2])
	}
	// A stamp that does not fit leaves the row without it.
	narrow := Safe(theme.UserCard([]string{"a prompt that fills the row"}, "23:09", 36)[1])
	if strings.Contains(narrow, "23:09") {
		t.Fatalf("a stamp that does not fit was drawn: %q", narrow)
	}
	if NewTheme(ANSI16, DarkBackground, AccentPlum).UserCard([]string{"x"}, "", 40) != nil {
		t.Fatal("a card without a tint")
	}
}

func TestTurnRuleCarriesTheSummaryAcrossTheWidth(t *testing.T) {
	theme := NewTheme(TrueColor, LightBackground, AccentPlum)
	summary := TurnSummaryText(5200*time.Millisecond, 4, 15600, 223)
	rule := Safe(theme.TurnRule(summary, 80))
	if CellWidth(rule) != 80 || !strings.Contains(rule, summary) || !strings.HasPrefix(rule, "── ") || !strings.HasSuffix(rule, "──") {
		t.Fatalf("rule = %q", rule)
	}
	if plain := (Theme{}).TurnRule(summary, 80); plain != summary {
		t.Fatalf("a rule without a tint = %q", plain)
	}
}
