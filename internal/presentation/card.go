package presentation

import "strings"

// cardPadding is the number of cells between the card's edge and its text.
const cardPadding = 2

// CardInset is the number of cells that a card takes from the width of its
// text on each side.
const CardInset = cardPadding

// Soft reports whether the theme can draw a card: a faint tint behind the
// text. Below 256 colors there is no tint, and a caller draws a border.
func (t Theme) Soft() bool { return t.chip != "" }

// CardTone is the tint of a card. Paper is the page's own shade. The others
// carry meaning: a tool that worked, a tool that failed, and a request that
// waits for the user.
type CardTone uint8

const (
	TonePaper CardTone = iota
	ToneSuccess
	ToneFailure
	ToneNotice
)

// The tints are the brand green, red, and amber at a low share of the page.
var (
	successTint = shaded{dark: rgb{24, 42, 35}, light: rgb{238, 247, 243}}
	failureTint = shaded{dark: rgb{56, 30, 28}, light: rgb{253, 239, 236}}
	// A request for approval breathes between a faint and a stronger amber.
	noticeTint       = shaded{dark: rgb{46, 37, 20}, light: rgb{254, 247, 232}}
	noticeTintStrong = shaded{dark: rgb{68, 50, 20}, light: rgb{251, 236, 206}}
)

// toneColor is the tint of a tone. Breath from 0 to 1 only moves the notice
// between its faint and strong shades.
func (t Theme) toneColor(tone CardTone, breath float64) rgb {
	switch tone {
	case TonePaper:
		return chipColor.on(t.shade)
	case ToneSuccess:
		return successTint.on(t.shade)
	case ToneFailure:
		return failureTint.on(t.shade)
	case ToneNotice:
		return blend(noticeTint.on(t.shade), noticeTintStrong.on(t.shade), breath)
	}
	panic("presentation: unknown card tone")
}

// A card is set on a soft tint with rounded corners, like the cards of the
// Inkling page: no border, only a shade of paper. The corners are quadrant
// blocks, so the card begins and ends half a row inside its first and last
// row, and two cards in a row are a full row apart. Each method returns the
// rows unchanged, or nothing, when the theme has no tint.

func (t Theme) edge(left, middle, right string, color rgb, width int) string {
	if !t.Soft() || width < 2 {
		return ""
	}
	return t.foreground(color, "") + left + strings.Repeat(middle, width-2) + right + reset
}

func (t Theme) rowOn(background, row string, width int) string {
	padding := max(width-2*cardPadding-CellWidth(Safe(row)), 0)
	edge := strings.Repeat(" ", cardPadding)
	return background + edge + strings.ReplaceAll(row, reset, reset+background) + strings.Repeat(" ", padding) + edge + reset
}

// CardTop is the first edge of a paper card of width cells.
func (t Theme) CardTop(width int) string {
	return t.edge("▗", "▄", "▖", t.toneColor(TonePaper, 0), width)
}

// CardBottom is the last edge of a paper card of width cells.
func (t Theme) CardBottom(width int) string {
	return t.edge("▝", "▀", "▘", t.toneColor(TonePaper, 0), width)
}

// CardRow sets one row of text on the paper tint, padded to width cells. A
// row that holds a reset keeps the tint after it. A row wider than the card
// is not cut.
func (t Theme) CardRow(row string, width int) string {
	if !t.Soft() {
		return row
	}
	return t.rowOn(t.chip, row, width)
}

// Card sets rows inside a paper card of width cells.
func (t Theme) Card(rows []string, width int) []string {
	return t.ToneCard(rows, width, TonePaper, 0)
}

// ToneCard sets rows inside a card of the given tone. Breath from 0 to 1
// moves a notice between faint and strong, so a caller can make it breathe.
// It returns the rows unchanged when the theme has no tint or the card has no
// room.
func (t Theme) ToneCard(rows []string, width int, tone CardTone, breath float64) []string {
	if !t.Soft() || width < 2*cardPadding+2 {
		return rows
	}
	color := t.toneColor(tone, breath)
	background := t.background(color)
	out := make([]string, 0, len(rows)+2)
	out = append(out, t.edge("▗", "▄", "▖", color, width))
	for _, row := range rows {
		out = append(out, t.rowOn(background, row, width))
	}
	return append(out, t.edge("▝", "▀", "▘", color, width))
}
