package main

import (
	"encoding/json"
	"fmt"
	"io"
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

type pendingTool struct{ name, title string }

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
}

func newConsoleObserver(stdout, stderr io.Writer, screen *terminal.Screen, chat bool, theme presentation.Theme, showThinking, verbose bool) *consoleObserver {
	return &consoleObserver{stdout: stdout, stderr: stderr, screen: screen, theme: theme, chat: chat, showThinking: showThinking, verbose: verbose, markdown: presentation.NewMarkdown(theme), plainEnded: true}
}

func (c *consoleObserver) Begin() {
	c.markdown = presentation.NewMarkdown(c.theme)
	c.line.Reset()
	c.thinking.Reset()
	c.bodyStarted, c.reasoningStarted, c.leadShown, c.plainEnded = false, false, false, true
	c.pending = c.pending[:0]
	c.nextTool = 0
	c.activity("Thinking")
}

func (c *consoleObserver) activity(message string) {
	if c.screen != nil {
		c.screen.SetStatus(presentation.Safe(message))
	}
}

// replyPrefix starts the first reply line with the assistant sigil and
// indents the others under it.
func (c *consoleObserver) replyPrefix() string {
	if c.leadShown {
		return "  "
	}
	return presentation.AssistantLead(c.theme)
}

func (c *consoleObserver) Text(delta string) {
	c.flushThinking()
	if !c.bodyStarted {
		if c.chat {
			fmt.Fprintln(c.stdout)
		}
		c.bodyStarted = true
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
		completeLines.WriteString(c.replyPrefix())
		c.leadShown = true
		completeLines.WriteString(c.markdown.Line(part))
		completeLines.WriteByte('\n')
		c.line.Reset()
		delta = rest
	}
	if completeLines.Len() > 0 {
		c.screen.SetPreview("")
		io.WriteString(c.stdout, completeLines.String())
	}
	if c.line.Len() > 0 {
		// Show tokens immediately; parse Markdown only when the line commits.
		c.screen.StreamPreview(c.replyPrefix(), presentation.Safe(c.line.String()))
	}
}

func (c *consoleObserver) Thinking(delta string) {
	c.activity("Thinking")
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
				c.screen.SetPreview("  " + c.theme.Graphite(c.theme.Italic(presentation.Safe(c.thinking.String()))))
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
	if c.screen != nil {
		c.screen.SetPreview("")
	}
	fmt.Fprintln(c.stderr, "  "+c.theme.Graphite(c.theme.Italic(presentation.Safe(c.thinking.String()))))
	c.thinking.Reset()
}

func (c *consoleObserver) flushText() {
	if c.screen == nil {
		if !c.plainEnded {
			fmt.Fprintln(c.stdout)
			c.plainEnded = true
		}
		return
	}
	c.screen.SetPreview("")
	if c.line.Len() > 0 {
		fmt.Fprintln(c.stdout, c.replyPrefix()+c.markdown.Line(c.line.String()))
		c.leadShown = true
		c.line.Reset()
	}
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
	c.pending = append(c.pending, pendingTool{name: name, title: title})
	if c.screen != nil {
		c.screen.SetToolStatus(title)
	}
	if c.verbose {
		fmt.Fprintf(c.stderr, "\n[tool %s] %s\n", presentation.Safe(name), presentation.Safe(string(input)))
	}
}

func (c *consoleObserver) ToolResult(name string, result tools.Result, elapsed time.Duration) {
	c.flushText()
	title := presentation.Safe(name)
	if c.nextTool < len(c.pending) && c.pending[c.nextTool].name == name {
		title = c.pending[c.nextTool].title
		c.pending[c.nextTool] = pendingTool{}
		c.nextTool++
		if c.nextTool == len(c.pending) {
			c.pending = c.pending[:0]
			c.nextTool = 0
		}
	}
	var completion string
	if c.chat {
		outcome := c.theme.Green("Done")
		if result.IsError {
			outcome = c.theme.Red("Failed")
		}
		completion = outcome + "  " + title + c.theme.Graphite(" · "+elapsed.Round(time.Millisecond).String())
	} else {
		completion = presentation.ToolCompletion(title, result.IsError, c.theme)
	}
	fmt.Fprintln(c.stderr, "  "+completion)
	if c.chat || result.IsError || name == "bash" || name == "task" || c.verbose {
		var preview string
		if c.chat && !c.verbose {
			budget := 3
			if result.IsError {
				budget = 6
			}
			preview = presentation.ResultPreview(result.Content, result.IsError, budget, c.theme)
		} else {
			preview = presentation.Safe(result.Content)
		}
		if preview != "" || !c.chat {
			lead := "    "
			if !c.chat {
				lead += c.theme.Hairline("│") + " "
			}
			for line := range strings.SplitSeq(preview, "\n") {
				fmt.Fprintln(c.stderr, lead+line)
			}
		}
	}
	if c.verbose {
		fmt.Fprintf(c.stderr, "[tool elapsed %s]\n", elapsed.Round(time.Millisecond))
	}
	if len(c.pending) > 0 {
		c.activity("Running tools")
	} else {
		c.activity("Thinking")
	}
}

func (c *consoleObserver) TurnDone(turn int, usage anthropic.Usage, stop anthropic.StopReason, elapsed time.Duration) {
	if stop == anthropic.StopToolUse {
		c.activity("Running tools")
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

func printUser(output io.Writer, prompt string, theme presentation.Theme) {
	fmt.Fprintln(output, "\n"+presentation.UserTurn(prompt, theme))
}

func contextUse(o *options, messages []anthropic.EncodedMessage) presentation.ContextUse {
	return presentation.ContextUse{Model: o.model, Effort: displayEffort(o.effort), Tokens: agent.EstimateTokens(messages), CompactTokens: o.compactTokens}
}

// motionVariable turns the terminal animations off with the value "off".
const motionVariable = "INKLING_MOTION"

// bannerFrames is the number of frames of the opening animation.
const bannerFrames = 24

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

// printLatestTools uses saved tool messages, not a second in-memory transcript.
// Only user messages carry tool results, so it decodes no assistant message
// except the one that holds the shown calls.
func printLatestTools(output io.Writer, messages []anthropic.EncodedMessage, theme presentation.Theme) error {
	for end := len(messages); end > 1; end-- {
		if messages[end-1].Role() != anthropic.RoleUser {
			continue
		}
		message, err := messages[end-1].Decode()
		if err != nil {
			return fmt.Errorf("show tools: %w", err)
		}
		var results []anthropic.ToolResultBlock
		for _, block := range message.Content {
			if result, ok := block.(anthropic.ToolResultBlock); ok {
				results = append(results, result)
			}
		}
		if len(results) == 0 {
			continue
		}
		calls, err := messages[end-2].Decode()
		if err != nil {
			return fmt.Errorf("show tools: %w", err)
		}
		for _, result := range results {
			var call *anthropic.ToolUseBlock
			for _, block := range calls.Content {
				if candidate, ok := block.(anthropic.ToolUseBlock); ok && candidate.ID == result.ToolUseID {
					call = &candidate
					break
				}
			}
			if call == nil {
				fmt.Fprintf(output, "Missing saved call for result %s\n%s\n", presentation.Safe(result.ToolUseID), presentation.ToolCompletion("Result", result.IsError, theme))
				fmt.Fprintln(output, presentation.Safe(result.Content))
				continue
			}
			view := describeTool(call.Name, call.Input, theme)
			fmt.Fprintf(output, "\n%s\n%s\n\n%s\n", presentation.ToolCompletion(view.Title, result.IsError, theme), view.Details, presentation.Safe(result.Content))
		}
		return nil
	}
	fmt.Fprintln(output, "No tool results are stored in this session yet.")
	return nil
}
