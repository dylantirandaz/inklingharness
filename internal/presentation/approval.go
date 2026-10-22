package presentation

import "strings"

// ApprovalRequest is a tool call that waits for the user, drawn as a rounded
// box: the action in bold, the work, and what the action may touch. The
// texts are plain; the theme styles them when it draws the request at a
// width.
type ApprovalRequest struct {
	// Title names the action, such as "Edit app.py".
	Title string
	// Caption tells what the action does and what it may touch.
	Caption string
	// Work is the change, the command, or the input. It is empty when the
	// title tells everything.
	Work []WorkLine
}

// WorkLine is one line of the work of a request.
type WorkLine struct {
	Text string
	Kind WorkKind
}

// WorkKind marks the lines of a change, which take the tints of a diff.
type WorkKind uint8

const (
	WorkPlain WorkKind = iota
	WorkAdded
	WorkRemoved
)

// approvalMargin puts a request at the column of the step bullets.
const approvalMargin = 2

// NewApprovalRequest is the request for a call of the tool name, as view
// describes it. The work holds every line of the details, never a preview,
// because the user approves what they see.
func NewApprovalRequest(name string, view ToolView) ApprovalRequest {
	title := singleLine(view.Title)
	lines := strings.Split(Safe(view.Details), "\n")
	if lines[0] == title {
		lines = lines[1:]
	}
	// The first line of the details tells what the call does; the rest is
	// the work.
	description := ""
	if len(lines) > 0 {
		description, lines = lines[0], lines[1:]
	}
	// Every call runs with full filesystem access; two words say so.
	request := ApprovalRequest{Title: title, Caption: "Not sandboxed"}
	if description != "" {
		request.Caption = description + " · not sandboxed"
	}
	// The work of these tools is the object of the title, so the title
	// names only the action and the work shows the object once.
	switch name {
	case "bash":
		request.Title = "Run a command"
	case "task":
		request.Title = "Delegate a task"
	case "remember":
		request.Title = "Remember a fact"
	}
	for _, line := range lines {
		kind := WorkPlain
		switch {
		case strings.HasPrefix(line, "+ "):
			kind = WorkAdded
		case strings.HasPrefix(line, "- "):
			kind = WorkRemoved
		}
		request.Work = append(request.Work, WorkLine{Text: line, Kind: kind})
	}
	return request
}

// ApprovalRows draws request in width cells: a row of air, then a rounded
// box with the action in bold, the work, and the caption in gray. The edge
// fades from plum to blue; below 256 colors it is a gray line. With air
// blank rows inside the box set the work apart. A long line of the work
// wraps; it is never cut, because the user approves what they see.
func (t Theme) ApprovalRows(request ApprovalRequest, width int, air bool) []string {
	margin := strings.Repeat(" ", approvalMargin)
	outer := max(width-approvalMargin, 16)
	inside := outer - 4
	content := []string{t.Bold(t.Ink(Ellipsize(request.Title, inside)))}
	if air {
		content = append(content, "")
	}
	for _, line := range request.Work {
		for _, piece := range wrapCells(expandTabs(line.Text), inside-2) {
			// A changed line fills the box, so its tint is a clean band.
			if line.Kind != WorkPlain {
				piece += strings.Repeat(" ", max(inside-2-CellWidth(piece), 0))
			}
			content = append(content, "  "+t.workText(line.Kind, piece))
		}
	}
	if air {
		content = append(content, "")
	}
	// The caption says what the action may touch, so it wraps and is never cut.
	for _, piece := range wrapWords(request.Caption, inside) {
		content = append(content, t.Graphite(piece))
	}
	// A gradient needs 256 colors; below that the edge is a plain line.
	box := t.roundedFrame(content, outer)
	if t.Soft() {
		box = t.GradientBox(content, outer)
	}
	rows := make([]string, 0, len(box)+1)
	rows = append(rows, "")
	for _, row := range box {
		rows = append(rows, margin+row)
	}
	return rows
}

// wrapWords cuts plain text into rows of at most width cells at spaces; a
// word longer than the row is cut at any rune.
func wrapWords(text string, width int) []string {
	var rows []string
	row := ""
	for _, word := range strings.Fields(text) {
		switch {
		case row == "":
			row = word
		case CellWidth(row)+1+CellWidth(word) <= width:
			row += " " + word
		default:
			rows = append(rows, row)
			row = word
		}
		for CellWidth(row) > width {
			pieces := wrapCells(row, width)
			rows = append(rows, pieces[:len(pieces)-1]...)
			row = pieces[len(pieces)-1]
		}
	}
	return append(rows, row)
}

// ApprovalChoices is the row of answers under a request, in the widest form
// that fits width cells: a cursor in the model color, the question in bold,
// then each key in bold and its answer in gray. Enter and Esc deny, as the
// footer says.
func (t Theme) ApprovalChoices(width int) string {
	margin := strings.Repeat(" ", approvalMargin)
	key := func(letter string) string { return t.Bold(t.Ink(letter)) }
	lead := t.Accent("❯") + " " + t.Bold(t.Ink("Allow?"))
	forms := [...]struct {
		plain string
		row   func() string
	}{
		{"❯ Allow?   y once   a this session   n no", func() string {
			return lead + "   " + key("y") + t.Graphite(" once") + "   " + key("a") + t.Graphite(" this session") + "   " + key("n") + t.Graphite(" no")
		}},
		{"❯ Allow?  y once  a session  n no", func() string {
			return lead + "  " + key("y") + t.Graphite(" once") + "  " + key("a") + t.Graphite(" session") + "  " + key("n") + t.Graphite(" no")
		}},
	}
	for _, form := range forms {
		if approvalMargin+CellWidth(form.plain) <= width {
			return margin + form.row()
		}
	}
	return key("y") + " " + key("a") + " " + key("n")
}

// workText styles one line of the work with the tints of a diff.
func (t Theme) workText(kind WorkKind, text string) string {
	switch kind {
	case WorkPlain:
		return t.Ink(text)
	case WorkAdded:
		return t.Added(text)
	case WorkRemoved:
		return t.Removed(text)
	}
	panic("presentation: unknown work kind")
}

// FailureRows tells that a turn did not finish, as a step does: a red bullet
// with the title in bold red, after a row of air, and the cause in gray
// hooked under it, wrapped at words to width cells.
func (t Theme) FailureRows(title, cause string, width int) []string {
	margin := strings.Repeat(" ", approvalMargin)
	room := max(width-approvalMargin-len(bodyIndent), 8)
	rows := []string{"", margin + t.Red("●") + " " + t.Bold(t.Red(Ellipsize(title, room)))}
	for index, piece := range wrapWords(singleLine(cause), room) {
		lead := bodyIndent
		if index == 0 {
			lead = "  " + t.Hairline("⎿") + "  "
		}
		rows = append(rows, margin+lead+t.Graphite(piece))
	}
	return rows
}

// TrimProject drops the project folder from the paths in text, so a label
// names a file as the user sees it from the project. It is for labels only:
// the work that a user approves keeps its paths as the model wrote them.
func TrimProject(directory, text string) string {
	directory = strings.TrimSuffix(directory, "/")
	if directory == "" {
		return text
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, directory+"/", ""), directory, ".")
}

// expandTabs sets a tab as four spaces, as the step rows do.
func expandTabs(text string) string { return strings.ReplaceAll(text, "\t", "    ") }

// wrapCells cuts plain text into rows of at most width cells, at any rune.
// An empty text is one empty row.
func wrapCells(text string, width int) []string {
	width = max(width, 1)
	var rows []string
	var row strings.Builder
	cells := 0
	for _, r := range text {
		size := RuneWidth(r)
		if cells+size > width && cells > 0 {
			rows = append(rows, row.String())
			row.Reset()
			cells = 0
		}
		row.WriteRune(r)
		cells += size
	}
	return append(rows, row.String())
}
