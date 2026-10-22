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
// and diff lines, colored step marks, and lines of force. Below 256 colors a
// tint cannot be expressed, and the plain renderers take over.
func (t Theme) Soft() bool { return t.chip != "" }

// Card sets rows in a thin square frame of width cells: a gray line around
// the text and air inside it, with no fill, so the page shows through. Every
// box of the chat is angular. A box that waits for the user, such as the
// composer or an approval, takes the heavy frame and the line of force of
// GradientBox; a box that the user only reads takes this thin gray one. Rows
// must fit width-2*CardInset cells; a wider row is not cut.
func (t Theme) Card(rows []string, width int) []string {
	inner := max(width-2, 0)
	edge := strings.Repeat("─", inner)
	side, air := t.Frame("│"), strings.Repeat(" ", cardPadding)
	out := make([]string, 0, len(rows)+2)
	out = append(out, t.Frame("┌"+edge+"┐"))
	for _, row := range rows {
		gap := max(inner-2*cardPadding-visibleWidth(row), 0)
		out = append(out, side+air+row+strings.Repeat(" ", gap)+air+side)
	}
	return append(out, t.Frame("└"+edge+"┘"))
}

// visibleWidth is the number of cells that a styled row takes on screen.
func visibleWidth(row string) int { return CellWidth(Safe(row)) }

// edgeSteps is how many colors a line of force takes along its length. Few
// steps keep long runs of one color, so an edge costs a few dozen bytes.
const edgeSteps = 12

// GradientBox sets rows in a heavy square frame of width cells, the frame of
// a box that waits for the user. Its edge is a line of force: Futurist red at
// the left, amber, then the gray of a frame, so the box seems launched from
// the left. A row sits one cell inside each side and must fit width-4 cells;
// a shorter row is padded.
func (t Theme) GradientBox(rows []string, width int) []string {
	out := make([]string, 0, len(rows)+2)
	out = append(out, t.edgeRow("┏", "━", "┓", width))
	for _, row := range rows {
		out = append(out, t.boxRow(row, width))
	}
	return append(out, t.edgeRow("┗", "━", "┛", width))
}

// SetbackBox is GradientBox with its title raised in a tower of its own that
// steps back into the top of the frame, as the towers of Sant'Elia step back.
// When the title does not leave room for the step, the title is the first row
// inside the frame.
func (t Theme) SetbackBox(title string, rows []string, width int) []string {
	tower := CellWidth(title) + 4
	if tower > width-4 {
		return t.GradientBox(append([]string{t.Bold(t.Ink(title))}, rows...), width)
	}
	out := make([]string, 0, len(rows)+3)
	out = append(out, string(t.edgeRun("┏", "━", "┓", 0, tower-1, width))+strings.Repeat(" ", width-tower))
	// The title row: the tower's left wall and title, and from the foot of
	// its right wall the top of the frame runs on to the right edge.
	line := t.appendColor(nil, t.edgeColor(0, width), false)
	line = append(line, "┃"+reset+" "...)
	line = append(line, t.Bold(t.Ink(title))+" "...)
	line = append(line, t.edgeRun("┗", "━", "┓", tower-1, width-1, width)...)
	out = append(out, string(line))
	for _, row := range rows {
		out = append(out, t.boxRow(row, width))
	}
	return append(out, t.edgeRow("┗", "━", "┛", width))
}

// boxRow is a row inside a heavy frame of width cells.
func (t Theme) boxRow(row string, width int) string {
	inner := max(width-4, 0)
	left := t.foreground(t.edgeColor(0, width), "") + "┃" + reset
	right := t.foreground(t.edgeColor(width-1, width), "") + "┃" + reset
	return left + " " + row + strings.Repeat(" ", max(inner-visibleWidth(row), 0)) + " " + right
}

// edgeRow is a whole top or bottom edge of width cells.
func (t Theme) edgeRow(open, run, close string, width int) string {
	return string(t.edgeRun(open, run, close, 0, width-1, width))
}

// edgeRun draws the columns from first to last of an edge of a box of width
// cells: open at first, close at last, and run between them, each in the
// color of the line of force at its column of the whole box.
func (t Theme) edgeRun(open, run, close string, first, last, width int) []byte {
	var line []byte
	var drawn rgb
	for column := first; column <= last; column++ {
		glyph := run
		switch column {
		case first:
			glyph = open
		case last:
			glyph = close
		}
		if color := t.edgeColor(column, width); column == first || color != drawn {
			line = t.appendColor(line, color, false)
			drawn = color
		}
		line = append(line, glyph...)
	}
	return append(line, reset...)
}

// edgeColor is the color of a line of force at column of width cells.
func (t Theme) edgeColor(column, width int) rgb {
	return t.forceColor(float64(column) / float64(max(width-1, 1)))
}

// forceColor is the color of a line of force at a share from 0 to 1 along
// it: red, then amber at a third, then the gray of a frame at the end, in
// edgeSteps steps.
func (t Theme) forceColor(share float64) rgb {
	share = math.Round(share*edgeSteps) / edgeSteps
	red, amber, frame := palette[roleRed].color.on(t.shade), palette[roleAmber].color.on(t.shade), palette[roleFrame].color.on(t.shade)
	if share < 1.0/3 {
		return blend(red, amber, share*3)
	}
	return blend(amber, frame, (share-1.0/3)*1.5)
}

// forceRule is a heavy rule of room cells that has run out run cells, in the
// colors of a line of force, with a wedge at its end once it is complete.
func (t Theme) forceRule(run, room int) string {
	var line []byte
	var drawn rgb
	for column := range min(run, room) {
		glyph := "━"
		if column == room-1 {
			glyph = "▶"
		}
		if color := t.forceColor(float64(column) / float64(max(room-1, 1))); column == 0 || color != drawn {
			line = t.appendColor(line, color, false)
			drawn = color
		}
		line = append(line, glyph...)
	}
	if len(line) > 0 {
		line = append(line, reset...)
	}
	return string(line) + strings.Repeat(" ", max(room-run, 0))
}
