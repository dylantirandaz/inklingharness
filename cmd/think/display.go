package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/terminal"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// CLI notices are plain text. Only the presentation code can add terminal styles.
type safeWriter struct{ target io.Writer }

func (writer safeWriter) Write(data []byte) (int, error) {
	_, err := io.WriteString(writer.target, presentation.Safe(string(data)))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

type pendingTool struct {
	name, title string
	input       json.RawMessage
}

type consoleObserver struct {
	stdout, stderr                io.Writer
	screen                        *terminal.Screen
	theme                         presentation.Theme
	chat, showThinking, verbose   bool
	markdown                      *presentation.Markdown
	line                          strings.Builder
	thinking                      strings.Builder
	bodyStarted, reasoningStarted bool
	// leadShown is true after the first committed reply line, which carries
	// the assistant sigil; later lines align under its text.
	leadShown  bool
	plainEnded bool
	pending    []pendingTool
	nextTool   int
	// toolCount numbers the cards of one turn across its requests, so a
	// badge matches the tab that /tools N opens.
	toolCount int
	// afterTools is true once tool cards were shown and no reply text has
	// followed them yet; the next text starts a new block.
	afterTools bool
	// approval reports how long the user took to answer, which a tool's
	// elapsed time includes but its own work does not.
	approval *approvals
	// model names the endpoint in the wait status; empty names no model.
	model string
	// project is the folder that step titles name files from; empty in a
	// headless run, which keeps the paths as the model wrote them.
	project string
	// steps is the kind of each tool step of the turn, in order: the score
	// that ends the turn.
	steps []presentation.StepKind
}

func newConsoleObserver(stdout, stderr io.Writer, screen *terminal.Screen, chat bool, theme presentation.Theme, showThinking, verbose bool) *consoleObserver {
	return &consoleObserver{stdout: stdout, stderr: stderr, screen: screen, theme: theme, chat: chat, showThinking: showThinking, verbose: verbose, markdown: presentation.NewMarkdown(theme), plainEnded: true}
}

func (c *consoleObserver) Begin() {
	c.markdown = presentation.NewMarkdown(c.theme)
	c.line.Reset()
	c.thinking.Reset()
	c.bodyStarted, c.reasoningStarted, c.leadShown, c.plainEnded = false, false, false, true
	c.afterTools = false
	c.pending = c.pending[:0]
	c.nextTool, c.toolCount = 0, 0
	c.steps = c.steps[:0]
	c.activity(c.waitingLabel())
}

// waitingLabel names the wait for the first streamed token. The model is
// not thinking yet: the request is queued at the provider or in flight.
func (c *consoleObserver) waitingLabel() string {
	if c.model == "" {
		return "Waiting for the model"
	}
	return "Waiting for " + presentation.ModelName(c.model)
}

func (c *consoleObserver) activity(message string) {
	if c.screen != nil {
		c.screen.SetStatus(presentation.Safe(message))
	}
}

// replyIndent is the margin of reply text on a screen. Prose, code, and tool
// cards share it, so the transcript has one left edge and no marker before
// the reply.
const replyIndent = "  "

func (c *consoleObserver) Text(delta string) {
	c.flushThinking()
	if !c.bodyStarted || c.afterTools {
		if c.chat {
			fmt.Fprintln(c.stdout)
		}
		c.bodyStarted, c.afterTools = true, false
	}
	c.activity("Writing reply")
	if c.screen == nil {
		if c.chat && !c.leadShown {
			fmt.Fprint(c.stdout, presentation.AssistantLead(c.theme))
			c.leadShown = true
		}
		fmt.Fprint(c.stdout, presentation.Safe(delta))
		if delta != "" {
			c.plainEnded = strings.HasSuffix(delta, "\n")
		}
		return
	}
	var completeLines strings.Builder
	for delta != "" {
		part, rest, complete := strings.Cut(delta, "\n")
		if !complete {
			c.line.WriteString(part)
			break
		}
		if completeLines.Len() == 0 {
			completeLines.Grow(len(delta) + c.line.Len() + 3)
		}
		if c.line.Len() > 0 {
			c.line.WriteString(part)
			part = c.line.String()
		}
		c.commitLine(&completeLines, part)
		c.leadShown = true
		c.line.Reset()
		delta = rest
	}
	if completeLines.Len() > 0 {
		c.screen.ClearPreview()
		io.WriteString(c.stdout, completeLines.String())
	}
	if c.line.Len() > 0 {
		// Show tokens immediately, without the Markdown marks; style the line
		// only when it commits.
		c.screen.StreamPreview(replyIndent, c.markdown.Unmarked(c.line.String()))
	}
}

func (c *consoleObserver) Thinking(delta string) {
	c.activity("Thinking")
	if c.screen != nil {
		// The trace shows the pace of reasoning even when its text is hidden.
		c.screen.Mark(presentation.TraceReasoning, 1)
	}
	if !c.showThinking {
		return
	}
	if !c.reasoningStarted {
		fmt.Fprintln(c.stderr, "\n"+presentation.Section("reasoning", c.theme))
		c.reasoningStarted = true
	}
	for delta != "" {
		part, rest, complete := strings.Cut(delta, "\n")
		c.thinking.WriteString(part)
		if !complete {
			if c.screen != nil {
				c.screen.StreamReasoning(c.thinking.String())
			}
			break
		}
		c.flushThinking()
		delta = rest
	}
}

func (c *consoleObserver) flushThinking() {
	if c.thinking.Len() == 0 {
		return
	}
	text := presentation.Safe(c.thinking.String())
	c.thinking.Reset()
	if c.screen == nil {
		fmt.Fprintln(c.stderr, "  "+c.theme.Graphite(c.theme.Italic(text)))
		return
	}
	c.screen.ClearPreview()
	// Reasoning wraps under its margin, as a reply does.
	for _, row := range terminal.WrapWords(text, max(c.screen.Width()-3, 1)) {
		fmt.Fprintln(c.stderr, "  "+c.theme.Graphite(c.theme.Italic(row)))
	}
}

// commitLine renders one complete reply line; its continuation rows keep the
// same margin. The fence that closes a code block draws nothing: the block
// ends where its code ends, and the reply's own blank line follows it.
func (c *consoleObserver) commitLine(out *strings.Builder, line string) {
	inCode := c.markdown.InCode()
	rendered := c.markdown.Line(line)
	if inCode && !c.markdown.InCode() {
		return
	}
	for _, row := range terminal.WrapWords(rendered, max(c.screen.Width()-3, 1)) {
		out.WriteString(replyIndent)
		out.WriteString(row)
		out.WriteByte('\n')
	}
	c.leadShown = true
}

func (c *consoleObserver) flushText() {
	if c.screen == nil {
		if !c.plainEnded {
			fmt.Fprintln(c.stdout)
			c.plainEnded = true
		}
		return
	}
	c.screen.ClearPreview()
	var rows strings.Builder
	if c.line.Len() > 0 {
		c.commitLine(&rows, c.line.String())
		c.line.Reset()
	}
	// A reply that ends inside a fence leaves no open code state behind.
	if c.markdown.InCode() {
		c.markdown.EndCode()
	}
	io.WriteString(c.stdout, rows.String())
}

func (c *consoleObserver) ToolCallStart(name string) {
	c.flushThinking()
	c.flushText()
	c.activity("Preparing " + presentation.Safe(name))
}

func describeTool(name string, input json.RawMessage, theme presentation.Theme) presentation.ToolView {
	view, err := presentation.DescribeTool(name, input, theme)
	if err == nil {
		return view
	}
	return presentation.ToolView{
		Title:   presentation.Safe(name) + " (invalid or unknown arguments)",
		Details: presentation.Safe(err.Error()) + "\n" + presentation.Safe(string(input)),
	}
}

func (c *consoleObserver) ToolCall(name string, input json.RawMessage) {
	title, err := presentation.ToolTitle(name, input)
	if err != nil {
		title = presentation.Safe(name) + " (invalid or unknown arguments)"
	}
	title = presentation.TrimProject(c.project, title)
	c.pending = append(c.pending, pendingTool{name: name, title: title, input: input})
	if c.screen != nil {
		c.screen.SetStatus(title)
	}
	if c.verbose {
		fmt.Fprintf(c.stderr, "\n[tool %s] %s\n", presentation.Safe(name), presentation.Safe(string(input)))
	}
}

// ToolResult records a finished tool. On a terminal with a tint it is a card
// of its own, as presentation.ToolCard draws it. Elsewhere it is a row with
// its preview lines on a rail, and a closing corner that names the route to
// the full result.
func (c *consoleObserver) ToolResult(name string, result tools.Result, elapsed time.Duration) {
	c.flushText()
	if c.screen != nil {
		c.screen.Mark(presentation.TraceOf(presentation.KindOf(name, result.IsError)), 1)
	}
	if c.chat && !c.afterTools {
		fmt.Fprintln(c.stderr)
		c.bodyStarted = true
	}
	title := presentation.Safe(name)
	var input json.RawMessage
	number := 0
	if c.nextTool < len(c.pending) && c.pending[c.nextTool].name == name {
		title, input = c.pending[c.nextTool].title, c.pending[c.nextTool].input
		c.toolCount++
		number = c.toolCount
		c.pending[c.nextTool] = pendingTool{}
		c.nextTool++
		if c.nextTool == len(c.pending) {
			c.pending = c.pending[:0]
			c.nextTool = 0
		}
	}
	// A reply after tool cards starts a new block.
	c.leadShown, c.afterTools = false, true
	if c.chat && c.approval != nil {
		elapsed = max(elapsed-c.approval.takeWait(), 0)
	}
	step := presentation.ToolStep{Name: name, Title: title, Input: input, Content: result.Content, Failed: result.IsError, Elapsed: elapsed, Number: number}
	c.steps = append(c.steps, presentation.KindOf(name, result.IsError))
	var card []string
	if c.chat && c.screen != nil && !c.verbose {
		card = c.theme.ToolCard(step, max(c.screen.Width()-3, 1))
	}
	if card != nil {
		for _, row := range card {
			fmt.Fprintln(c.stderr, "  "+row)
		}
	} else {
		c.printToolRail(step, result)
	}
	if c.verbose {
		fmt.Fprintf(c.stderr, "[tool elapsed %s]\n", elapsed.Round(time.Millisecond))
	}
	if len(c.pending) > 0 {
		c.activity("Running tools")
	} else {
		c.activity(c.waitingLabel())
	}
}

// printToolRail records a finished tool as a row with its preview lines on a
// rail: for a terminal without a tint, a plain stream, and the verbose view
// that shows the whole result.
func (c *consoleObserver) printToolRail(step presentation.ToolStep, result tools.Result) {
	route, badge := "/tools", ""
	if step.Number > 0 {
		route += " " + strconv.Itoa(step.Number)
		if c.screen != nil {
			badge = "  " + c.theme.Key(strconv.Itoa(step.Number))
		}
	}
	var completion string
	if c.chat {
		completion = presentation.ToolRow(presentation.KindOf(step.Name, result.IsError), step.Title, step.Elapsed, c.theme)
	} else {
		completion = presentation.ToolCompletion(step.Title, result.IsError, c.theme)
	}
	fmt.Fprintln(c.stderr, "  "+completion+badge)
	if !c.chat && !result.IsError && step.Name != "bash" && step.Name != "task" && !c.verbose {
		return
	}
	var preview string
	if c.chat && !c.verbose {
		budget := 3
		if result.IsError {
			budget = 6
		}
		preview = presentation.ResultPreview(result.Content, result.IsError, budget, route, c.theme)
	} else {
		preview = presentation.Safe(result.Content)
	}
	if preview == "" && c.chat {
		return
	}
	lines := strings.Split(preview, "\n")
	for index, line := range lines {
		rail := "│"
		if index == len(lines)-1 {
			rail = "╰"
		}
		if c.chat && !result.IsError {
			line = c.theme.Graphite(line)
		}
		fmt.Fprintln(c.stderr, "  "+c.theme.Hairline(rail)+" "+line)
	}
}

func (c *consoleObserver) TurnDone(turn int, usage anthropic.Usage, stop anthropic.StopReason, elapsed time.Duration) {
	if stop == anthropic.StopToolUse {
		c.activity("Running tools")
	}
	if c.screen != nil {
		// The request's input is the context that the model just read, so
		// the gauge follows each request instead of waiting for the turn.
		c.screen.SetContextTokens(usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens + usage.OutputTokens)
	}
	if c.verbose {
		c.flushThinking()
		c.flushText()
		fmt.Fprintf(c.stderr, "\n[turn %d %s %s %s]\n", turn, stop, formatUsage(usage), elapsed.Round(time.Millisecond))
	}
}

func (c *consoleObserver) UnknownEvent(eventType string) {
	if c.verbose {
		fmt.Fprintf(c.stderr, "[unknown stream event %q]\n", presentation.Safe(eventType))
	}
}

func (c *consoleObserver) Status(message string) {
	c.flushThinking()
	c.flushText()
	c.activity(message)
	fmt.Fprintf(c.stderr, "\n  %s\n", presentation.Safe(message))
}

func (c *consoleObserver) Finish() {
	c.flushThinking()
	c.flushText()
	c.activity("")
}

// printUser echoes a sent prompt. On a tinted screen it is a card, wrapped at
// words with the time at the right edge of its first row. Otherwise it is a
// tinted band, with its rows under the prompt marker when the terminal
// width is known.
func printUser(output io.Writer, prompt string, width int, theme presentation.Theme) {
	stamp, pad := "", -1
	if width > 0 {
		var rows []string
		textWidth := max(width-3, 1)
		if theme.Soft() {
			// The card takes four cells for its margins and two for the marker.
			textWidth = max(width-1-4-2, 1)
		}
		for line := range strings.SplitSeq(presentation.Safe(prompt), "\n") {
			rows = append(rows, terminal.WrapWords(line, textWidth)...)
		}
		stamp = time.Now().Format("15:04")
		if card := theme.UserCard(rows, stamp, width-1); card != nil {
			fmt.Fprintln(output, "\n"+strings.Join(card, "\n"))
			return
		}
		prompt = strings.Join(rows, "\n")
		pad = width - 4 - presentation.CellWidth(rows[0]) - len(stamp)
		if pad < 2 {
			stamp, pad = "", -1
		}
	}
	fmt.Fprintln(output, "\n"+presentation.UserTurn(prompt, stamp, pad, theme))
}

func contextUse(o *options, messages []anthropic.EncodedMessage, usage *anthropic.Usage) presentation.ContextUse {
	return presentation.ContextUse{
		Model: o.model, Effort: displayEffort(o.effort),
		Tokens: agent.EstimateTokens(messages), CompactTokens: o.compactTokens,
		SessionTokens: usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens + usage.OutputTokens,
	}
}

// motionVariable turns the terminal animations off with the value "off".
const motionVariable = "INKLING_MOTION"

// motionSetting reads INKLING_MOTION: empty or "on" animates, "off" keeps
// every row still. Another value is an error, not a silent default.
func motionSetting(value string) (bool, error) {
	switch value {
	case "", "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be on or off, not %q", motionVariable, value)
	}
}

// notifyAfter keeps quick turns quiet: a notification is useful only when the
// user may have left the terminal.
const notifyAfter = 5 * time.Second

// shipSignals shows the chat state outside the terminal: in the window title,
// which tab bars such as cmux show, and in desktop notifications that carry
// the reason, not only "waiting". Without a screen it does nothing.
type shipSignals struct {
	screen *terminal.Screen
	place  string
}

func (s shipSignals) ready() {
	if s.screen != nil {
		s.screen.SetTitle("○ inkling · " + s.place)
	}
}

func (s shipSignals) working() {
	if s.screen != nil {
		s.screen.SetTitle("● inkling · working · " + s.place)
	}
}

func (s shipSignals) approval(title string) {
	if s.screen != nil {
		s.screen.SetTitle("◉ inkling · approval · " + s.place)
		s.screen.Notify("Inkling needs approval · " + title)
	}
}

func (s shipSignals) finished(elapsed time.Duration, finalText string, err error) {
	if s.screen == nil || elapsed < notifyAfter {
		return
	}
	if err != nil {
		s.screen.Notify("Inkling stopped · " + firstLine(err.Error()))
		return
	}
	s.screen.Notify("Inkling finished · " + firstLine(finalText))
}

// firstLine gives a short summary for a notification body.
func firstLine(text string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if runes := []rune(line); len(runes) > 100 {
				return string(runes[:99]) + "…"
			}
			return line
		}
	}
	return "done"
}

// latestToolDocuments reads saved messages only when the user opens tool
// output: every tool result since the last prompt, in the order of the cards.
// The live cards do not keep another copy of the transcript or full results.
func latestToolDocuments(messages []anthropic.EncodedMessage, theme presentation.Theme) ([]terminal.Document, error) {
	// The turn starts after the last user message that carries no tool
	// result: the prompt. Later user messages hold only tool results.
	start := 0
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role() != anthropic.RoleUser {
			continue
		}
		message, err := messages[index].Decode()
		if err != nil {
			return nil, fmt.Errorf("show tools: %w", err)
		}
		if !hasToolResult(message) {
			start = index + 1
			break
		}
	}
	calls := make(map[string]anthropic.ToolUseBlock)
	var documents []terminal.Document
	for _, encoded := range messages[start:] {
		message, err := encoded.Decode()
		if err != nil {
			return nil, fmt.Errorf("show tools: %w", err)
		}
		for _, block := range message.Content {
			switch typed := block.(type) {
			case anthropic.ToolUseBlock:
				calls[typed.ID] = typed
			case anthropic.ToolResultBlock:
				view := presentation.ToolView{
					Title:   "Result",
					Details: "Missing saved call for result " + typed.ToolUseID,
				}
				name := ""
				if call, found := calls[typed.ToolUseID]; found {
					view = describeTool(call.Name, call.Input, presentation.Theme{})
					name = call.Name
				}
				documents = append(documents, terminal.Document{
					Title:  view.Title,
					Text:   view.Details + "\n\nResult\n" + typed.Content,
					Format: terminal.PlainDocument,
					Drop:   theme.Drop(presentation.KindOf(name, typed.IsError)),
				})
			}
		}
	}
	return documents, nil
}

func hasToolResult(message anthropic.Message) bool {
	for _, block := range message.Content {
		if _, ok := block.(anthropic.ToolResultBlock); ok {
			return true
		}
	}
	return false
}

// diffDocuments splits a unified patch into one document per file, titled
// with the path, its change kind, and the counts of added and removed lines.
// A patch that does not start with a file header stays one document.
func diffDocuments(patch string) []terminal.Document {
	var documents []terminal.Document
	start := 0
	for start < len(patch) {
		end := len(patch)
		if next := strings.Index(patch[start+1:], "\ndiff --git "); next >= 0 {
			end = start + 1 + next + 1
		}
		file := patch[start:end]
		documents = append(documents, terminal.Document{Title: diffTitle(file), Text: file, Format: terminal.DiffDocument})
		start = end
	}
	return documents
}

func diffTitle(file string) string {
	path, kind := "Changes", ""
	added, removed := 0, 0
	for line := range strings.SplitSeq(file, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git a/"):
			path = line[len("diff --git a/"):]
			if _, after, found := strings.Cut(path, " b/"); found {
				path = after
			}
		case strings.HasPrefix(line, "new file mode"):
			kind = "new"
		case strings.HasPrefix(line, "deleted file mode"):
			kind = "deleted"
		case strings.HasPrefix(line, "rename to "):
			kind = "renamed"
		case strings.HasPrefix(line, "Binary files"):
			kind = "binary"
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	title := path
	if kind != "" {
		title += " · " + kind
	}
	if added > 0 || removed > 0 {
		title += fmt.Sprintf(" · +%d −%d", added, removed)
	}
	return title
}
