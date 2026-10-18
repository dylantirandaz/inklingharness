package presentation

import "strings"

// The model cards of the Inkling page draw size as a waffle: a grid of small
// squares, filled in rows, with the part that is used in color and the rest
// faint. Waffle does the same for any quantity.

// series are the colors of the parts of a waffle, taken from the page's charts.
var series = [...]shaded{
	{dark: rgb{91, 151, 242}, light: rgb{1, 85, 191}},    // cobalt
	{dark: rgb{45, 190, 144}, light: rgb{14, 153, 114}},  // emerald
	{dark: rgb{247, 162, 36}, light: rgb{214, 134, 20}},  // amber
	{dark: rgb{3, 160, 244}, light: rgb{3, 135, 231}},    // azure
	{dark: rgb{242, 97, 76}, light: rgb{239, 64, 44}},    // red
	{dark: rgb{176, 214, 105}, light: rgb{120, 160, 50}}, // lime
	{dark: rgb{208, 122, 174}, light: rgb{143, 63, 113}}, // plum
	{dark: rgb{160, 160, 160}, light: rgb{120, 120, 120}},
}

// SeriesCount is the number of distinct series colors; a larger series wraps.
const SeriesCount = len(series)

// WaffleSegment is a run of cells in one series color.
type WaffleSegment struct {
	// Series picks a color; SeriesAccent is the model color of the theme.
	Series, Cells int
}

// SeriesAccent colors a segment with the model's accent, as the page colors
// the active parameters of a model.
const SeriesAccent = -1

// waffleCell is one square: its series color, or none when it is empty.
type waffleCell struct {
	filled bool
	series int
}

// Waffle draws the segments in order, row by row, in a grid of columns by
// rows squares. Cells that no segment fills are faint. Without color a filled
// square is "▪" and an empty one a dot, so the state stays visible.
func (t Theme) Waffle(segments []WaffleSegment, columns, rows int) []string {
	if columns <= 0 || rows <= 0 {
		return nil
	}
	cells := make([]waffleCell, columns*rows)
	next := 0
	for _, segment := range segments {
		for range segment.Cells {
			if next == len(cells) {
				break
			}
			cells[next] = waffleCell{filled: true, series: segment.Series}
			next++
		}
	}
	out := make([]string, 0, rows)
	for row := range rows {
		var line strings.Builder
		// Neighbors of one color share a sequence, so a row stays short.
		previous := ""
		for _, cell := range cells[row*columns : (row+1)*columns] {
			escape, glyph := t.sgr[roleHairline], "▪"
			if cell.filled {
				escape = t.seriesEscape(cell.series)
			} else if !t.Colored() {
				glyph = "·"
			}
			if escape != previous {
				line.WriteString(escape)
				previous = escape
			}
			line.WriteString(glyph)
		}
		if t.Colored() {
			line.WriteString(reset)
		}
		out = append(out, line.String())
	}
	return out
}

// seriesEscape is the foreground of a series; empty without color.
func (t Theme) seriesEscape(index int) string {
	if index == SeriesAccent {
		return t.sgr[roleAccent]
	}
	return t.foreground(series[index%len(series)].on(t.shade), "")
}

// SeriesMark is a square in the color of a series, for a legend.
func (t Theme) SeriesMark(index int) string {
	if !t.Colored() {
		return "▪"
	}
	return t.seriesEscape(index) + "▪" + reset
}

// WaffleCells shares cells among values of one quantity, with capacity as the
// size of the whole grid. A value that is not zero gets at least one cell, so
// a small part stays visible, and the total never exceeds cells. A quantity
// over capacity fills the grid, shared by proportion.
func WaffleCells(values []int, capacity, cells int) []int {
	total := 0
	for _, value := range values {
		total += max(value, 0)
	}
	whole := max(capacity, total)
	shares := make([]int, len(values))
	if whole == 0 || cells <= 0 {
		return shares
	}
	used := 0
	for index, value := range values {
		if value <= 0 {
			continue
		}
		shares[index] = max(1, value*cells/whole)
		used += shares[index]
	}
	// Take back what the minimum added, from the largest shares first.
	for used > cells {
		largest := 0
		for index, share := range shares {
			if share > shares[largest] {
				largest = index
			}
		}
		shares[largest]--
		used--
	}
	return shares
}
