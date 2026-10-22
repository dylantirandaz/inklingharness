package presentation

import "strings"

// UserCard sets a sent prompt on a faint band across the whole row, with a
// row of padding above and below, an edge of blue ink at the left, the text
// in bold, and the stamp, such as the time, at the right end of the first
// row when it fits. The band is one background that the terminal extends to
// the edge, so it costs a few bytes a row. The caller wraps the prompt into
// rows that fit width-6 cells. It returns nil below 256 colors, where
// UserTurn draws the prompt.
func (t Theme) UserCard(rows []string, stamp string, width int) []string {
	if !t.Soft() || len(rows) == 0 {
		return nil
	}
	stamp = singleLine(stamp)
	edge := t.sgr[roleBlue] + "▌"
	band := func(text string) string { return t.chip + edge + text + "\x1b[K" + reset }
	out := make([]string, 0, len(rows)+2)
	out = append(out, band(""))
	for index, row := range rows {
		line := " " + bold + t.sgr[roleInk] + row + "\x1b[22m"
		// The stamp ends two cells before the edge, as the time of a step does.
		if gap := width - 2 - CellWidth(row) - CellWidth(stamp) - cardPadding; index == 0 && stamp != "" && gap >= 2 {
			line += strings.Repeat(" ", gap) + t.sgr[roleGraphite] + stamp
		}
		out = append(out, band(line))
	}
	return append(out, band(""))
}

// TurnRule closes a finished turn with one quiet row: the three drops of the
// Inkling picture, blue, red, and green, as a signature, then the wall time,
// the request count, and the token use in gray.
func (t Theme) TurnRule(summary string) string {
	summary = strings.TrimPrefix(singleLine(summary), "✓ ")
	return "  " + t.Blue("●") + t.Red("●") + t.Green("●") + " " + t.Graphite(summary)
}
