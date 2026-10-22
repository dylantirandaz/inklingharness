package presentation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ToolStep is a finished tool call, ready to be set on a card.
type ToolStep struct {
	// Name is the tool, such as "bash". Title is the call as the screen names
	// it, such as "Run ls".
	Name, Title string
	// Input is the arguments of the call; Content is the text of the result.
	Input   json.RawMessage
	Content string
	Failed  bool
	Elapsed time.Duration
	// Number is the tab that /tools opens for this step; 0 for none.
	Number int
}

const (
	// A step that worked shows a few lines of its output; a step that failed
	// shows more, because the cause is in them.
	outputRowLimit  = 3
	failureRowLimit = 6
	// A change shows this many rows of its diff.
	diffRowLimit = 10
	// bodyIndent puts the rows of a result under the verb; the first row
	// carries the hook in its place, with two cells after it.
	bodyIndent = "     "
)

// ToolCard sets a finished tool call as rows of the turn: a step mark with
// the verb in bold, the target, and the facts in gray; then what the tool
// left behind, hooked under the mark. A read, a search, or a listing leaves
// only its count. A change shows its diff. Anything else shows a few lines
// of its output. A command that failed names its exit code with the facts,
// not as a line of output. It returns nil below 256 colors, or when the row
// has no room, and the caller draws the plain rail.
func (t Theme) ToolCard(step ToolStep, width int) []string {
	if !t.Soft() || width < 2*cardPadding+16 {
		return nil
	}
	inner := width - 2*cardPadding
	margin := strings.Repeat(" ", cardPadding)
	summary := toolSummary(step)
	if output, code, found := strings.Cut(step.Content, "\n[exit code "); step.Failed && found && strings.HasSuffix(code, "]") {
		step.Content, summary = output, "exit "+strings.TrimSuffix(code, "]")
	}
	rows := []string{margin + t.stepTitle(step, summary, inner)}
	for index, row := range t.stepBody(step, inner) {
		if index == 0 {
			// The hook ties the first row of the result to its bullet.
			row = "  " + t.Hairline("⎿") + "  " + strings.TrimPrefix(row, bodyIndent)
		}
		rows = append(rows, margin+row)
	}
	return rows
}

// stepTitle is the first row of a step, read left to right as one sentence:
// a wedge in the color of what the tool did, the verb in bold, the
// target in ink, then the facts in gray (a count, and the time when it is a
// second or more), and last the number of its tab in a faint gray. The
// target gives way first when the row is too narrow, then the tab number,
// then the facts.
func (t Theme) stepTitle(step ToolStep, summary string, inner int) string {
	verb, target, _ := strings.Cut(singleLine(step.Title), " ")
	if past, found := pastTense[verb]; found && !step.Failed {
		verb = past
	}
	var facts []string
	if summary != "" {
		facts = append(facts, summary)
	}
	// A step of a few milliseconds is the normal case; only a slow one is
	// news.
	if step.Elapsed >= time.Second {
		facts = append(facts, ShortDuration(step.Elapsed))
	}
	suffix := ""
	if len(facts) > 0 {
		suffix = " · " + strings.Join(facts, " · ")
	}
	tab := ""
	if step.Number > 0 {
		tab = "  #" + strconv.Itoa(step.Number)
	}
	verb = Ellipsize(verb, max(inner-2, 1))
	// The row keeps the verb and a little of the target.
	if inner < titleMinimum(verb, suffix+tab, target) {
		tab = ""
	}
	if inner < titleMinimum(verb, suffix, target) {
		suffix = ""
	}
	fixed := 2 + CellWidth(verb) + CellWidth(suffix) + CellWidth(tab) // the mark, a space, the verb, the facts
	if target != "" {
		target = Ellipsize(target, max(inner-fixed-1, 1))
	}
	bullet, verbText := t.StepMark(KindOf(step.Name, step.Failed)), t.Bold(t.Ink(verb))
	if step.Failed {
		verbText = t.Bold(t.Red(verb))
	}
	row := bullet + " " + verbText
	if target != "" {
		row += " " + t.Ink(target)
	}
	return row + t.Graphite(suffix) + t.Hairline(tab)
}

// titleMinimum is the narrowest row that holds the verb, the trailing text,
// and a target cut to an ellipsis.
func titleMinimum(verb, trailing, target string) int {
	width := 2 + CellWidth(verb) + CellWidth(trailing)
	if target != "" {
		width += 2
	}
	return width
}

// stepBody is what a step shows under its first row.
func (t Theme) stepBody(step ToolStep, inner int) []string {
	room := max(inner-len(bodyIndent), 1)
	route := "/tools"
	if step.Number > 0 {
		route += " " + strconv.Itoa(step.Number)
	}
	switch {
	case step.Failed:
		return t.outputRows(ResultLines(step.Content, true, failureRowLimit, route), room, t.Ink)
	case step.Name == "edit_file" || step.Name == "write_file":
		return t.changeRows(step, room, route)
	case step.Name == "read_file" || step.Name == "grep" || step.Name == "glob":
		return nil
	}
	return t.outputRows(ResultLines(step.Content, false, outputRowLimit, route), room, t.Graphite)
}

// outputRows sets result lines under the verb, each cut to room cells, in the
// given color. A notice is always gray.
func (t Theme) outputRows(lines []ResultLine, room int, color func(string) string) []string {
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		// A blank line of output costs a row and says nothing.
		if strings.TrimSpace(line.Text) == "" {
			continue
		}
		text := Ellipsize(strings.ReplaceAll(line.Text, "\t", "    "), room)
		if line.Notice {
			rows = append(rows, bodyIndent+t.Graphite(text))
		} else {
			rows = append(rows, bodyIndent+color(text))
		}
	}
	return rows
}

var changedLines = regexp.MustCompile(`lines (\d+)-\d+|at line (\d+)`)

// firstChangedLine reads the line of an edit from the tool's result, such as
// "edited a.go: now lines 6-6". It is 0 when the result does not say.
func firstChangedLine(content string) int {
	match := changedLines.FindStringSubmatch(content)
	if match == nil {
		return 0
	}
	for _, digits := range match[1:] {
		if digits != "" {
			line, _ := strconv.Atoi(digits)
			return line
		}
	}
	return 0
}

// changeRows shows a change as a diff: the removed lines in red, the added
// lines in green, and the line numbers in gray when the result gives them.
func (t Theme) changeRows(step ToolStep, room int, route string) []string {
	var args toolInput
	if json.Unmarshal(step.Input, &args) != nil {
		return nil
	}
	var removed, added []string
	first := 1
	switch step.Name {
	case "edit_file":
		if args.OldString == nil || args.NewString == nil {
			return nil
		}
		removed, added = splitLines(*args.OldString), splitLines(*args.NewString)
		first = firstChangedLine(step.Content)
	case "write_file":
		if args.Content == nil {
			return nil
		}
		added = splitLines(*args.Content)
	}
	type change struct {
		sign, text string
		number     int
	}
	var changes []change
	for offset, text := range removed {
		changes = append(changes, change{"-", text, first + offset})
	}
	for offset, text := range added {
		changes = append(changes, change{"+", text, first + offset})
	}
	shown, omitted := changes, 0
	if len(changes) > diffRowLimit {
		shown, omitted = changes[:diffRowLimit-1], len(changes)-(diffRowLimit-1)
	}
	// The gutter holds the line numbers; it is empty when no line is known.
	digits := 0
	if first > 0 {
		digits = len(strconv.Itoa(first + max(len(removed), len(added)) - 1))
	}
	gutterWidth := 0
	if digits > 0 {
		gutterWidth = digits + 1
	}
	bodies := make([]string, len(shown))
	block := 0
	for index, line := range shown {
		bodies[index] = line.sign + " " + Ellipsize(strings.ReplaceAll(line.text, "\t", "    "), max(room-gutterWidth-2, 1))
		block = max(block, CellWidth(bodies[index]))
	}
	rows := make([]string, 0, len(shown)+1)
	for index, line := range shown {
		gutter := ""
		if digits > 0 {
			gutter = t.Graphite(fmt.Sprintf("%*d", digits, line.number)) + " "
		}
		// Every line of the block takes the same width, so the tints form
		// one clean rectangle instead of a ragged edge.
		body := bodies[index] + strings.Repeat(" ", block-CellWidth(bodies[index]))
		if line.sign == "-" {
			body = t.Removed(body)
		} else {
			body = t.Added(body)
		}
		rows = append(rows, bodyIndent+gutter+body)
	}
	if omitted > 0 {
		rows = append(rows, bodyIndent+t.Graphite(fmt.Sprintf("%d more lines · %s", omitted, route)))
	}
	return rows
}

// splitLines splits text into lines without the empty one that a final
// newline leaves.
func splitLines(text string) []string {
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// toolSummary is the count that a read, a search, a listing, or a change puts
// in the first row, so that its output does not need rows of its own. It is
// empty for every other step, and for a step that failed.
func toolSummary(step ToolStep) string {
	if step.Failed {
		return ""
	}
	switch step.Name {
	case "read_file":
		return readSummary(step.Content)
	case "grep":
		return countSummary(step.Content, "match", "matches")
	case "glob":
		return countSummary(step.Content, "file", "files")
	case "write_file":
		var args toolInput
		if json.Unmarshal(step.Input, &args) == nil && args.Content != nil {
			if *args.Content == "" {
				return "0 lines"
			}
			return plural(len(splitLines(*args.Content)), "line", "lines")
		}
	case "edit_file":
		if first := firstChangedLine(step.Content); first > 0 {
			return "line " + strconv.Itoa(first)
		}
	}
	return ""
}

func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, many)
}

// readSummary is the length of the file that a read reached the end of, or
// the number of lines that it showed before a limit.
func readSummary(content string) string {
	text := strings.TrimRight(Safe(content), "\n")
	if text == "" {
		return "0 lines"
	}
	lines := strings.Split(text, "\n")
	last := lines[len(lines)-1]
	var total int
	if _, err := fmt.Sscanf(last, "[end of file: %d lines", &total); err == nil {
		return plural(total, "line", "lines")
	}
	if strings.HasPrefix(last, "[truncated:") {
		return fmt.Sprintf("%d lines shown", len(lines)-1)
	}
	return plural(len(lines), "line", "lines")
}

// countSummary counts the result lines of a search or a listing; a notice
// that the result was cut is not one of them.
func countSummary(content, one, many string) string {
	text := strings.TrimRight(Safe(content), "\n")
	if text == "" {
		return "0 " + many
	}
	lines := strings.Split(text, "\n")
	cut := false
	if strings.HasPrefix(lines[len(lines)-1], "[truncated:") {
		lines, cut = lines[:len(lines)-1], true
	}
	summary := plural(len(lines), one, many)
	if cut {
		summary += " shown"
	}
	return summary
}
