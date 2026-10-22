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

// StepKind is what a tool step did, which names the color of its ink drop.
type StepKind uint8

const (
	// StepRead looked at something: a file, a search, a page.
	StepRead StepKind = iota
	// StepChange changed a file or the memory.
	StepChange
	// StepRun ran a command, a task, or a tool from the user's files.
	StepRun
	// StepFailed is any step that failed.
	StepFailed
)

// KindOf is the kind of a step of the tool name.
func KindOf(name string, failed bool) StepKind {
	if failed {
		return StepFailed
	}
	switch name {
	case "read_file", "read_files", "grep", "glob", "web_fetch", "web_search", "mcp_list":
		return StepRead
	case "edit_file", "write_file", "remember":
		return StepChange
	}
	return StepRun
}

// Drop is the ink drop of a kind of step, in a color of the Inkling picture:
// blue for a read, green for a change, ink for a run, and red for a failure.
func (t Theme) Drop(kind StepKind) string {
	switch kind {
	case StepRead:
		return t.Blue("●")
	case StepChange:
		return t.Green("●")
	case StepRun:
		return t.Ink("●")
	case StepFailed:
		return t.Red("●")
	}
	panic("presentation: unknown step kind")
}

// scoreLimit is the most drops that the score of a turn shows; a longer turn
// ends its score with an ellipsis.
const scoreLimit = 32

// TurnRule closes a finished turn with its score: one ink drop for each step,
// in order and in the color of what the step did, so the scrollback keeps the
// shape of every turn. Then come the wall time, the request count, and the
// token use in gray. A turn without steps is one drop of ink: the reply.
func (t Theme) TurnRule(summary string, steps []StepKind) string {
	summary = strings.TrimPrefix(singleLine(summary), "✓ ")
	var score strings.Builder
	if len(steps) == 0 {
		score.WriteString(t.Drop(StepRun))
	}
	for index, kind := range steps {
		if index == scoreLimit {
			score.WriteString(t.Graphite("…"))
			break
		}
		score.WriteString(t.Drop(kind))
	}
	return "  " + score.String() + " " + t.Graphite(summary)
}
