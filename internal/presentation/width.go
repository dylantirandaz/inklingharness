package presentation

import "unicode"

// RuneWidth is the number of terminal cells of a rune. Combining marks and
// joiners take none. Wide East Asian characters and emoji take two, as in
// ordinary modern terminals.
func RuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d || r == 0xfe0f || r == 0xfe0e {
		return 0
	}
	if r < 32 || r == 127 {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) || (r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f000 && r <= 0x1faff) || (r >= 0x20000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

// CellWidth is the number of terminal cells that plain text occupies.
func CellWidth(text string) int {
	width := 0
	for _, r := range text {
		width += RuneWidth(r)
	}
	return width
}

// clipCells cuts text so that it takes at most width cells.
func clipCells(text string, width int) string {
	cells := 0
	for offset, r := range text {
		cells += RuneWidth(r)
		if cells > width {
			return text[:offset]
		}
	}
	return text
}

// Ellipsize shortens text to at most width cells, ending with an ellipsis
// when it cuts.
func Ellipsize(text string, width int) string {
	if CellWidth(text) <= width {
		return text
	}
	return clipCells(text, max(width-1, 0)) + "…"
}

// EllipsizeStart shortens text to at most width cells by cutting its
// beginning, so the end of a path stays: "…/harbor/internal".
func EllipsizeStart(text string, width int) string {
	if CellWidth(text) <= width {
		return text
	}
	runes := []rune(text)
	cells := 1
	start := len(runes)
	for start > 0 && cells+RuneWidth(runes[start-1]) <= width {
		start--
		cells += RuneWidth(runes[start])
	}
	return "…" + string(runes[start:])
}
