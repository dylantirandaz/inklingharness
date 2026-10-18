package presentation

import "strings"

// UserCard sets a sent prompt on a paper card of width cells, so what you
// sent stays as the shape of the composer that it came from. The caller
// wraps the prompt into rows that fit width-4-2 cells. The first row starts
// with the marker, and ends with the stamp, such as the time, when the row
// leaves room for it. It returns nil when the theme has no tint.
func (t Theme) UserCard(rows []string, stamp string, width int) []string {
	if !t.Soft() || len(rows) == 0 {
		return nil
	}
	inner := width - 2*cardPadding
	card := make([]string, 0, len(rows))
	for index, row := range rows {
		if index > 0 {
			card = append(card, "  "+row)
			continue
		}
		line := t.Bold(t.Accent("›")) + " " + row
		stamp = singleLine(stamp)
		if gap := inner - 2 - CellWidth(row) - CellWidth(stamp); stamp != "" && gap >= 2 {
			line += strings.Repeat(" ", gap) + t.Graphite(stamp)
		}
		card = append(card, line)
	}
	return t.Card(card, width)
}

// TurnRule closes a finished turn with a hairline that carries the summary, as
// the working row above the composer does while the turn runs: the wall time,
// the request count, and the token use stay visible in scrollback, and the
// rule marks where the next turn begins. Without a tint the summary is a
// plain line.
func (t Theme) TurnRule(summary string, width int) string {
	summary = singleLine(summary)
	if !t.Soft() {
		return t.Graphite(summary)
	}
	label := "── " + summary + " "
	return t.Hairline("──") + " " + t.Graphite(summary) + " " + t.Hairline(strings.Repeat("─", max(width-CellWidth(label), 0)))
}
