package presentation

import (
	"math"
	"strings"
)

// cardPadding is the number of cells of air between a frame and its text.
const cardPadding = 2

// CardInset is the number of cells that a card takes from the width of its
// text on each side: the frame and the air inside it.
const CardInset = cardPadding + 1

// Soft reports whether the theme draws the full look: tints behind prompts
// and diff lines, colored ink drops, and gradient edges. Below 256 colors a
// tint cannot be expressed, and the plain renderers take over.
func (t Theme) Soft() bool { return t.chip != "" }

// Card sets rows in a thin rounded frame of width cells: a gray line around
// the text and air inside it, with no fill, so the page shows through. Every
// box of the chat is rounded; a box that waits for the user, such as the
// composer or an approval, takes the gradient edge of GradientBox, and a box
// that the user only reads takes this gray one. Rows must fit
// width-2*CardInset cells; a wider row is not cut.
func (t Theme) Card(rows []string, width int) []string {
	inner := max(width-2, 0)
	edge := strings.Repeat("─", inner)
	side, air := t.Frame("│"), strings.Repeat(" ", cardPadding)
	out := make([]string, 0, len(rows)+2)
	out = append(out, t.Frame("╭"+edge+"╮"))
	for _, row := range rows {
		gap := max(inner-2*cardPadding-visibleWidth(row), 0)
		out = append(out, side+air+row+strings.Repeat(" ", gap)+air+side)
	}
	return append(out, t.Frame("╰"+edge+"╯"))
}

// visibleWidth is the number of cells that a styled row takes on screen.
func visibleWidth(row string) int { return CellWidth(Safe(row)) }

// edgeSteps is how many colors a gradient edge takes from plum to blue. Few
// steps keep long runs of one color, so an edge costs a few dozen bytes.
const edgeSteps = 12

// GradientBox sets rows in a rounded box of width cells whose edge runs from
// plum at the left to blue at the right, the two colors of the Inkling page.
// A row sits one cell inside each side and must fit width-4 cells; a shorter
// row is padded.
func (t Theme) GradientBox(rows []string, width int) []string {
	inner := max(width-4, 0)
	left := t.foreground(t.edgeColor(0, width), "") + "│" + reset
	right := t.foreground(t.edgeColor(width-1, width), "") + "│" + reset
	out := make([]string, 0, len(rows)+2)
	out = append(out, t.edgeRow("╭", "╮", width))
	for _, row := range rows {
		out = append(out, left+" "+row+strings.Repeat(" ", max(inner-visibleWidth(row), 0))+" "+right)
	}
	return append(out, t.edgeRow("╰", "╯", width))
}

// edgeRow is the top or the bottom of a gradient box.
func (t Theme) edgeRow(open, close string, width int) string {
	var line []byte
	var drawn rgb
	for column := range width {
		glyph := "─"
		switch column {
		case 0:
			glyph = open
		case width - 1:
			glyph = close
		}
		if color := t.edgeColor(column, width); column == 0 || color != drawn {
			line = t.appendColor(line, color, false)
			drawn = color
		}
		line = append(line, glyph...)
	}
	return string(append(line, reset...))
}

// edgeColor is the color of a gradient edge at column of width cells.
func (t Theme) edgeColor(column, width int) rgb {
	share := float64(column) / float64(max(width-1, 1))
	return blend(plum.on(t.shade), blue.on(t.shade), math.Round(share*edgeSteps)/edgeSteps)
}
