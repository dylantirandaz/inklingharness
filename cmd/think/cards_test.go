package main

import (
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

var (
	tintedTheme = presentation.NewTheme(presentation.TrueColor, presentation.LightBackground, presentation.AccentPlum)
	plainTheme  = presentation.NewTheme(presentation.ANSI16, presentation.LightBackground, presentation.AccentPlum)
)

// Code sits on a card as wide as its text, so the tint reaches the same right
// edge on every row, whether a line is short, long, or wraps.
func TestCodeLinesFillTheCardAtAnyLength(t *testing.T) {
	const width = 40
	markdown := presentation.NewMarkdown(tintedTheme)
	var rows []string
	for _, line := range []string{"```go", "x := 1", strings.Repeat("word ", 20), "```"} {
		rows = append(rows, blockRows(tintedTheme, markdown.Block(line), width)...)
	}
	if len(rows) < 6 {
		t.Fatalf("code on a card has %d rows: %q", len(rows), rows)
	}
	for _, row := range rows {
		if cells := presentation.CellWidth(presentation.Safe(row)); cells != width {
			t.Fatalf("card row is %d cells wide, want %d: %q", cells, width, presentation.Safe(row))
		}
	}
	if !strings.Contains(presentation.Safe(rows[1]), "go") {
		t.Fatalf("the card does not name its language: %q", presentation.Safe(rows[1]))
	}
}

// Without a tint code stays between rules, as before, and prose is not a card.
func TestCodeWithoutTintKeepsItsRules(t *testing.T) {
	markdown := presentation.NewMarkdown(plainTheme)
	fence := blockRows(plainTheme, markdown.Block("```go"), 40)
	if len(fence) != 1 || !strings.Contains(presentation.Safe(fence[0]), "────") {
		t.Fatalf("fence without tint = %q", fence)
	}
	prose := blockRows(tintedTheme, presentation.NewMarkdown(tintedTheme).Block("plain words"), 40)
	if len(prose) != 1 || strings.Contains(prose[0], "▗") {
		t.Fatalf("prose became a card: %q", prose)
	}
}

// Every row of the status card fits the text width of the card, whatever the
// length of the values.
func TestStatusCardRowsFitTheCard(t *testing.T) {
	for _, screenWidth := range []int{60, 80, 140} {
		inner := innerWidth(screenWidth)
		rows := statusCard(tintedTheme, screenWidth, statusFacts{
			model: "thinkingmachines/inkling-small", effort: "default",
			directory: "/a/very/long/path/" + strings.Repeat("segment/", 12), sessionID: "902ec62d8b6a0f7ea145852783386c52",
			messages: 8, estimate: 558, compactTokens: 200_000,
		})
		joined := strings.Join(rows, "\n")
		for _, want := range []string{"Inkling-Small", "Auto-approve", "no", "558", "200k"} {
			if !strings.Contains(presentation.Safe(joined), want) {
				t.Fatalf("status card at %d is missing %q:\n%s", screenWidth, want, presentation.Safe(joined))
			}
		}
		for _, row := range rows {
			if cells := presentation.CellWidth(presentation.Safe(row)); cells > inner {
				t.Fatalf("row is %d cells, the card holds %d: %q", cells, inner, presentation.Safe(row))
			}
		}
	}
}

func TestCardFitsOnlyWithTintAndRoom(t *testing.T) {
	for _, test := range []struct {
		theme presentation.Theme
		width int
		want  bool
	}{
		{tintedTheme, 120, true},
		{tintedTheme, 43, true},
		{tintedTheme, 42, false},
		{plainTheme, 120, false},
		{presentation.Theme{}, 120, false},
	} {
		if got := cardFits(test.theme, test.width); got != test.want {
			t.Errorf("cardFits(width %d, tint %t) = %t", test.width, test.theme.Soft(), got)
		}
	}
}
