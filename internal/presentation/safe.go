// Package presentation formats untrusted conversation text without terminal I/O.
package presentation

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
)

// Safe removes terminal escape sequences and controls, retaining newlines, tabs,
// and visible Unicode. An unfinished escape sequence is discarded through the
// end of this string. Each call stands alone: it never emits a partial escape
// that a subsequent call could complete.
func Safe(text string) string {
	const (
		plain = iota
		escape
		sequence
		controlString
		stringEscape
	)
	state := plain
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		switch state {
		case escape:
			switch r {
			case '[':
				state = sequence
			case ']', 'P', 'X', '^', '_':
				state = controlString
			case '\x1b':
			case '\n', '\t':
				out.WriteRune(r)
				state = plain
			default:
				if r < 0x20 || r > 0x2f {
					state = plain
				}
			}
			continue
		case sequence:
			if r == '\x1b' {
				state = escape
			} else if r == '\n' || r == '\t' {
				out.WriteRune(r)
				state = plain
			} else if r >= 0x40 && r <= 0x7e {
				state = plain
			}
			continue
		case controlString:
			if r == '\a' || r == '\u009c' {
				state = plain
			} else if r == '\x1b' {
				state = stringEscape
			}
			continue
		case stringEscape:
			if r == '\\' || r == '\a' || r == '\u009c' {
				state = plain
			} else if r != '\x1b' {
				state = controlString
			}
			continue
		}
		switch r {
		case '\x1b':
			state = escape
		case '\u009b':
			state = sequence
		case '\u0090', '\u0098', '\u009d', '\u009e', '\u009f':
			state = controlString
		case '\n', '\t':
			out.WriteRune(r)
		default:
			// Bidirectional overrides can disguise commands or file extensions.
			if !unicode.IsControl(r) && !(r >= '\u202a' && r <= '\u202e') && !(r >= '\u2066' && r <= '\u2069') {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func singleLine(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(Safe(text), "\n", " ↵ "), "\t", "    ")
}

func brief(text string) string {
	text = singleLine(text)
	count := 0
	for index := range text {
		if count == 76 {
			return text[:index] + "…"
		}
		count++
	}
	return text
}

// ModelName is the display name of a model: the part after the provider,
// with the words of an Inkling name capitalized as on the model cards.
func ModelName(model string) string {
	name := singleLine(model)
	if _, slug, found := strings.Cut(name, "/"); found {
		name = slug
	}
	if !strings.HasPrefix(strings.ToLower(name), "inkling") {
		return name
	}
	words := strings.Split(name, "-")
	for index, word := range words {
		if word != "" {
			words[index] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, "-")
}

// Section labels a block that is not a live turn, such as an earlier reply.
func Section(label string, theme Theme) string {
	return theme.Hairline("───") + " " + theme.Graphite(theme.Italic(singleLine(label)))
}

// AssistantLead starts the first line of a reply in plain chat: a dot in the
// model color. Later lines use two spaces.
func AssistantLead(theme Theme) string {
	return theme.Accent("●") + " "
}

// UserTurn echoes a submitted prompt as a tinted band, like a site chip, so
// the user's words stand apart from the reply in scrollback. A stamp, such as
// the time, ends the first line after pad spaces when pad is not negative.
// Erase-to-end extends the tint to the right edge without padding that a
// resize would wrap. Without a tint the accent marker carries the distinction.
func UserTurn(prompt, stamp string, pad int, theme Theme) string {
	lines := strings.Split(Safe(prompt), "\n")
	var out strings.Builder
	for index, line := range lines {
		if index > 0 {
			out.WriteByte('\n')
		}
		marker := "  "
		tail := ""
		if index == 0 {
			marker = "› "
			if stamp != "" && pad >= 0 {
				tail = strings.Repeat(" ", pad) + theme.Graphite(singleLine(stamp))
			}
		}
		if theme.chip == "" {
			out.WriteString(theme.Accent(marker) + line + tail)
			continue
		}
		if tail != "" {
			tail = strings.Repeat(" ", pad) + theme.sgr[roleGraphite] + singleLine(stamp)
		}
		out.WriteString(theme.chip + theme.sgr[roleAccent] + marker + theme.sgr[roleInk] + line + tail + "\x1b[K" + reset)
	}
	return out.String()
}

// ToolCompletion keeps the outcome visible without expanding the tool
// details: a green dot or a red cross, the verb in bold, then its target.
// The glyphs differ so the outcome does not depend on color.
func ToolCompletion(title string, failed bool, theme Theme) string {
	dot := theme.Green("●")
	if failed {
		dot = theme.Red("✕")
	}
	verb, target, _ := strings.Cut(singleLine(title), " ")
	line := dot + " " + theme.Bold(verb)
	if target != "" {
		line += " " + target
	}
	return line
}

// ResultLine is one line of a result preview. A notice says what the preview
// left out, so a screen can set it in a quieter color.
type ResultLine struct {
	Text   string
	Notice bool
}

// ResultPreview limits ordinary output lines and ends with a disclosure that
// names the route to the full result, such as "/tools 2", when text is
// omitted. Failure previews keep the start and end. Source truncation notices
// are always shown in full, even when they exceed the line budget.
// Nonpositive budgets display nothing.
func ResultPreview(content string, isError bool, maxLines int, route string, theme Theme) string {
	var out strings.Builder
	for index, line := range ResultLines(content, isError, maxLines, route) {
		if index > 0 {
			out.WriteByte('\n')
		}
		if line.Notice {
			out.WriteString(theme.Graphite(line.Text))
		} else {
			out.WriteString(line.Text)
		}
	}
	return out.String()
}

// ResultLines is ResultPreview as plain lines, so that a caller can fit,
// color, and indent each one itself.
func ResultLines(content string, isError bool, maxLines int, route string) []ResultLine {
	if maxLines <= 0 {
		return nil
	}
	text := strings.TrimRight(Safe(content), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	shortened := false
	if !isError {
		for index, line := range lines {
			if resultNotice(line) {
				continue
			}
			count := 0
			for offset := range line {
				if count == 160 {
					lines[index] = line[:offset] + "…"
					shortened = true
					break
				}
				count++
			}
		}
	}
	plain := func(texts []string) []ResultLine {
		out := make([]ResultLine, 0, len(texts))
		for _, text := range texts {
			out = append(out, ResultLine{Text: text})
		}
		return out
	}
	if len(lines) <= maxLines && !shortened {
		return plain(lines)
	}
	kept := min(maxLines-1, len(lines))
	// Retain the initial error context as well as the final error summary.
	head := kept
	if isError {
		head = 0
		if kept > 2 {
			head = 1
		}
	}
	tail := kept - head
	out := plain(lines[:head])
	omitted := len(lines) - kept
	for _, line := range lines[head : len(lines)-tail] {
		if resultNotice(line) {
			out = append(out, ResultLine{Text: line})
			omitted--
		}
	}
	notice := fmt.Sprintf("%d more lines · %s", omitted, route)
	if shortened {
		notice = fmt.Sprintf("%d more lines · long lines shortened · %s", omitted, route)
	}
	if omitted > 0 || shortened {
		out = append(out, ResultLine{Text: notice, Notice: true})
	}
	return append(out, plain(lines[len(lines)-tail:])...)
}

// These notices come from tool output limits, not the preview limit.
func resultNotice(line string) bool {
	return strings.HasPrefix(line, "[output truncated:") || strings.HasPrefix(line, "[truncated:")
}

// ContextUse is what the footer reports: model, effort, context against the
// compaction threshold, and total session token use. CompactTokens 0 means
// that automatic compaction is off.
type ContextUse struct {
	Model, Effort string
	Tokens        int
	CompactTokens int
	SessionTokens int
}

// FooterModel names the model and the effort that the next request uses. The
// footer keeps them and the context use visible after the header leaves
// scrollback.
func FooterModel(use ContextUse, theme Theme) string {
	return theme.Ink(ModelName(use.Model)) + theme.Hairline(" · ") + theme.Graphite(singleLine(use.Effort))
}

// FooterUsage is the context against the compaction threshold, with its
// four-cell gauge, and the total session use.
func FooterUsage(use ContextUse, shownTokens int, theme Theme) string {
	separator := theme.Hairline(" · ")
	var out strings.Builder
	if use.CompactTokens > 0 {
		out.WriteString(theme.Gauge(shownTokens, use.CompactTokens, 4))
		out.WriteByte(' ')
		out.WriteString(theme.Graphite(ShortTokens(shownTokens) + " / " + ShortTokens(use.CompactTokens)))
	} else {
		out.WriteString(theme.Graphite("context ~" + ShortTokens(shownTokens)))
	}
	out.WriteString(separator)
	out.WriteString(theme.Graphite(ShortTokens(use.SessionTokens) + " used"))
	return out.String()
}

// ShortTokens writes a token count with at most two decimals: 950, 7.9k, 200k.
func ShortTokens(count int) string {
	switch {
	case count < 1000:
		return fmt.Sprintf("%d", count)
	case count < 100_000:
		return fmt.Sprintf("%.1fk", float64(count)/1000)
	default:
		return fmt.Sprintf("%dk", (count+500)/1000)
	}
}

// TurnSummary closes a finished turn with its wall time, request count, and
// token use, so a long turn's cost stays visible in scrollback.
func TurnSummary(elapsed time.Duration, requests, inputTokens, outputTokens int, theme Theme) string {
	return theme.Graphite(TurnSummaryText(elapsed, requests, inputTokens, outputTokens))
}

// TurnSummaryText is the summary of a turn as plain text.
func TurnSummaryText(elapsed time.Duration, requests, inputTokens, outputTokens int) string {
	summary := "✓ " + ShortDuration(elapsed)
	if requests > 1 {
		summary += fmt.Sprintf(" · %d requests", requests)
	}
	return summary + " · " + ShortTokens(inputTokens) + " in · " + ShortTokens(outputTokens) + " out"
}

// pastTense names the first word of each tool title once the tool is done.
var pastTense = map[string]string{
	"Run": "Ran", "Write": "Wrote", "Edit": "Edited", "Search": "Searched",
	"Find": "Found", "Delegate": "Delegated", "Fetch": "Fetched",
	"Remember": "Remembered", "Ask": "Asked",
}

// ToolRow records a finished tool in the conversation: the mark of its kind,
// as in the full look, or a red cross for a failure, then the verb in bold (past
// tense once it succeeded), the target, and the time when it is a second or
// more. A failure has its own glyph, so the outcome does not depend on color.
func ToolRow(kind StepKind, title string, elapsed time.Duration, theme Theme) string {
	failed := kind == StepFailed
	dot := theme.Red("✕")
	if !failed {
		dot = theme.StepMark(kind)
	}
	verb, target, _ := strings.Cut(singleLine(title), " ")
	if past, found := pastTense[verb]; found && !failed {
		verb = past
	}
	line := dot + " " + theme.Bold(verb)
	if target != "" {
		line += " " + target
	}
	if elapsed >= time.Second {
		line += "  " + theme.Graphite(ShortDuration(elapsed))
	}
	return line
}

// ShortDuration keeps two significant figures at most: 85ms, 1.2s, 14s, 3m05s.
func ShortDuration(elapsed time.Duration) string {
	switch {
	case elapsed < time.Second:
		return fmt.Sprintf("%dms", elapsed.Milliseconds())
	case elapsed < 10*time.Second:
		return fmt.Sprintf("%.1fs", elapsed.Seconds())
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds", int(elapsed.Round(time.Second).Seconds()))
	default:
		elapsed = elapsed.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", int(elapsed.Minutes()), int(elapsed.Seconds())%60)
	}
}
