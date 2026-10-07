// Package presentation formats untrusted conversation text without terminal I/O.
package presentation

import (
	"fmt"
	"strings"
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

// Banner shows the model, effort, and folder. width is the terminal width for
// the rule; 0 omits the rule. Session details belong in /status.
func Banner(model, effort, directory string, width int, theme Theme) string {
	var out strings.Builder
	out.WriteString(theme.Gradient("◈ I N K L I N G"))
	out.WriteString(theme.Haze("   " + singleLine(model) + " · effort " + singleLine(effort)))
	if width > 0 {
		out.WriteByte('\n')
		out.WriteString(theme.Rule(min(width-1, 64)))
	}
	out.WriteByte('\n')
	out.WriteString(theme.Haze("  " + singleLine(directory)))
	return out.String()
}

// Section labels a block that is not a live turn, such as an earlier reply.
func Section(label string, theme Theme) string {
	return theme.Gradient("◈") + " " + theme.Haze(singleLine(label))
}

// AssistantLead starts the first line of a reply; later lines use two spaces.
func AssistantLead(theme Theme) string {
	return theme.Gradient("◈") + " "
}

// UserTurn echoes a submitted prompt: the ion glyph, then the text with
// continuation lines aligned under it.
func UserTurn(prompt string, theme Theme) string {
	var out strings.Builder
	for index, line := range strings.Split(Safe(prompt), "\n") {
		if index == 0 {
			out.WriteString(theme.Ion("❯ "))
		} else {
			out.WriteString("\n  ")
		}
		out.WriteString(line)
	}
	return out.String()
}

// ToolCompletion keeps the outcome visible without expanding the tool details.
func ToolCompletion(title string, failed bool, theme Theme) string {
	if failed {
		return theme.Ember("✗ ") + theme.Ember(singleLine(title))
	}
	return theme.Verdant("◆ ") + singleLine(title)
}

// ResultPreview limits the total number of displayed lines, including the
// disclosure line. For failures, it favors the tail where exit status and error
// summaries commonly appear. Nonpositive budgets display nothing.
func ResultPreview(content string, isError bool, maxLines int, theme Theme) string {
	if maxLines <= 0 {
		return ""
	}
	text := strings.TrimSuffix(Safe(content), "\n")
	lines := strings.Split(text, "\n")
	style := theme.Haze
	if isError {
		style = theme.Ember
	}
	if len(lines) <= maxLines {
		return styleLines(text, style)
	}
	kept := maxLines - 1
	omitted := len(lines) - kept
	notice := fmt.Sprintf("[%d lines omitted; /tools shows the full result]", omitted)
	if kept == 0 {
		return style(notice)
	}
	if isError {
		return styleLines(notice+"\n"+strings.Join(lines[len(lines)-kept:], "\n"), style)
	}
	return styleLines(strings.Join(lines[:kept], "\n")+"\n"+notice, style)
}

// styleLines styles each line separately, so a caller can prefix lines
// without carrying a color across a line break.
func styleLines(text string, style func(string) string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = style(line)
	}
	return strings.Join(lines, "\n")
}

// Footer is the one-line ship status under the composer: model, effort, and a
// context gauge against the compaction threshold. compactTokens 0 means that
// automatic compaction is off; the gauge then gives only the estimate.
func Footer(model, effort string, contextTokens, compactTokens int, theme Theme) string {
	name := model
	if _, slug, found := strings.Cut(model, "/"); found {
		name = slug
	}
	separator := theme.Haze(" · ")
	var out strings.Builder
	out.WriteString(theme.Haze(singleLine(name)))
	out.WriteString(separator)
	out.WriteString(theme.Haze(singleLine(effort)))
	out.WriteString(separator)
	if compactTokens > 0 {
		out.WriteString(theme.Gauge(contextTokens, compactTokens, 6))
		out.WriteString(theme.Haze(" " + shortTokens(contextTokens) + " of " + shortTokens(compactTokens)))
	} else {
		out.WriteString(theme.Haze("context ~" + shortTokens(contextTokens)))
	}
	return out.String()
}

func shortTokens(count int) string {
	switch {
	case count < 1000:
		return fmt.Sprintf("%d", count)
	case count < 100_000:
		return fmt.Sprintf("%.1fk", float64(count)/1000)
	default:
		return fmt.Sprintf("%dk", (count+500)/1000)
	}
}
