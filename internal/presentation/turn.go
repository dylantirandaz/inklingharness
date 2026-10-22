package presentation

import "strings"

// bandSlant is how many cells the leading edge of a prompt band leans over
// its first rows, and the column where the text of the prompt starts.
const bandSlant = 3

// UserCard sets a sent prompt on a faint band across the whole row, with a
// row of padding above and below, the text in bold, and the stamp, such as
// the time, at the right end of the first row when it fits. The leading edge
// of the band leans forward, one cell a row, as the planes of the Futurists
// lean into their motion, and a stroke runs down it in the colors of a line
// of force, from red to amber. The band is one background that the terminal
// extends to the edge, so it costs a few bytes a row. The caller wraps the
// prompt into rows that fit width-6 cells. It returns nil below 256 colors,
// where UserTurn draws the prompt.
func (t Theme) UserCard(rows []string, stamp string, width int) []string {
	if !t.Soft() || len(rows) == 0 {
		return nil
	}
	stamp = singleLine(stamp)
	total := len(rows) + 2
	band := func(index int, text string) string {
		indent := max(bandSlant-1-index, 0)
		// While the edge leans, a quadrant glyph that fills the upper right
		// and the lower left draws one cell of a slash, and the slashes of
		// the rows join into one line. Where the edge stands, it is a bar.
		stroke := "▞"
		if index >= bandSlant {
			stroke = "▌"
		}
		color := t.appendColor(nil, t.forceColor(float64(index)/float64(total-1)/3), false)
		if text != "" {
			// The text keeps its column while the edge leans.
			text = strings.Repeat(" ", bandSlant-1-indent) + text
		}
		return strings.Repeat(" ", indent) + t.chip + string(color) + stroke + text + "\x1b[K" + reset
	}
	out := make([]string, 0, total)
	out = append(out, band(0, ""))
	for index, row := range rows {
		line := bold + t.sgr[roleInk] + row + "\x1b[22m"
		// The stamp ends two cells before the edge, as the time of a step does.
		if gap := width - cardPadding - bandSlant - CellWidth(row) - CellWidth(stamp); index == 0 && stamp != "" && gap >= 2 {
			line += strings.Repeat(" ", gap) + t.sgr[roleGraphite] + stamp
		}
		out = append(out, band(index+1, line))
	}
	return append(out, band(total-1, ""))
}

// StepKind is what a tool step did, which names the color of its mark.
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

// StepMark is the mark of a kind of step: a wedge that points the way the
// work moves, in the color of what the step did: blue for a read, green for
// a change, ink for a run, and red for a failure.
func (t Theme) StepMark(kind StepKind) string {
	switch kind {
	case StepRead:
		return t.Blue("▶")
	case StepChange:
		return t.Green("▶")
	case StepRun:
		return t.Ink("▶")
	case StepFailed:
		return t.Red("▶")
	}
	panic("presentation: unknown step kind")
}

// scoreLimit is the most marks that the score of a turn shows; a longer
// turn ends its score with an ellipsis.
const scoreLimit = 32

// TurnRule closes a finished turn with its score: one wedge for each step,
// in order and in the color of what the step did, so the scrollback keeps the
// shape of every turn and the run of wedges reads as its speed. Then come the
// wall time, the request count, and the token use in gray. A turn without
// steps is one wedge of ink: the reply.
func (t Theme) TurnRule(summary string, steps []StepKind) string {
	summary = strings.TrimPrefix(singleLine(summary), "✓ ")
	var score strings.Builder
	if len(steps) == 0 {
		score.WriteString(t.StepMark(StepRun))
	}
	for index, kind := range steps {
		if index == scoreLimit {
			score.WriteString(t.Graphite("…"))
			break
		}
		score.WriteString(t.StepMark(kind))
	}
	return "  " + score.String() + " " + t.Graphite(summary)
}

// trainLength is the number of wedges in the working train.
const trainLength = 3

// WorkingTrain is the mark of the working row at a phase of its motion:
// three wedges that end in the column of the step marks, with a bright wedge
// that runs along them toward that column and its echo behind it, as Balla
// painted a moving thing by repeating it where it just was. Every fourth
// phase has no bright wedge, so the run reads as a beat. Without motion pass
// one phase always. Below 256 colors the bright wedge is a filled one that
// runs along hollow ones, in the model color.
func (t Theme) WorkingTrain(phase int) string {
	head := phase % (trainLength + 1)
	if !t.Blends() {
		var train strings.Builder
		for index := range trainLength {
			if index == head {
				train.WriteString("▶")
			} else {
				train.WriteString("▷")
			}
		}
		return t.Accent(train.String())
	}
	hairline, accent := palette[roleHairline].color.on(t.shade), accents[t.accent].color.on(t.shade)
	var line []byte
	for index := range trainLength {
		color := blend(hairline, accent, 0.15)
		switch index {
		case head:
			color = accent
		case head - 1:
			color = blend(hairline, accent, 0.45)
		}
		line = t.appendColor(line, color, false)
		line = append(line, "▶"...)
	}
	return string(append(line, reset...))
}
