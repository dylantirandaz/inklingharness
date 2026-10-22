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

// A code fence shows only its language, in gray, on every terminal, and a
// closing fence is air, so code copies without marks around it.
func TestCodeFenceIsTheLanguageAlone(t *testing.T) {
	for _, theme := range []presentation.Theme{tintedTheme, plainTheme} {
		markdown := presentation.NewMarkdown(theme)
		fence := presentation.Safe(markdown.Line("```go"))
		markdown.Line("x := 1")
		if fence != "go" || markdown.Line("```") != "" {
			t.Fatalf("fence = %q", fence)
		}
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
