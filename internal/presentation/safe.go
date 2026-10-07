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

// wordmark is the site's letterspaced "THINKING MACHINES".
const wordmark = "T H I N K I N G   M A C H I N E S"

// Banner opens a chat: the Inkling mark beside the wordmark, the model name
// with its effort on a chip, and the folder. width is the terminal width; a
// narrow terminal, or one without 256 colors, gets the text alone. Session
// details belong in /status.
func Banner(model, effort, directory string, width int, theme Theme) string {
	text := []string{
		theme.Graphite(wordmark),
		theme.Bold(theme.Ink(ModelName(model))) + "  " + theme.Chip(" "+strings.ToUpper(singleLine(effort))+" "),
		theme.Graphite(singleLine(directory)),
	}
	mark := theme.Mark()
	if mark == nil || width < MarkWidth+3+len(wordmark)+1 {
		return strings.Join(text, "\n")
	}
	// Center the wordmark, a gap, the model, and the folder on the six rows.
	beside := append([]string{"", text[0], ""}, text[1:]...)
	var out strings.Builder
	for index, row := range mark {
		if index > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(row)
		if index < len(beside) && beside[index] != "" {
			out.WriteString("   " + beside[index])
		}
	}
	return out.String()
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

// AssistantLead starts the first line of a reply: an ink drop in the model
// color. Later lines use two spaces.
func AssistantLead(theme Theme) string {
	return theme.Accent("●") + " "
}

// UserTurn echoes a submitted prompt as a tinted band, like a site chip, so
// the user's words stand apart from the reply in scrollback. Erase-to-end
// extends the tint to the right edge without padding that a resize would
// wrap. Without a tint the accent marker carries the distinction.
func UserTurn(prompt string, theme Theme) string {
	lines := strings.Split(Safe(prompt), "\n")
	var out strings.Builder
	for index, line := range lines {
		if index > 0 {
			out.WriteByte('\n')
		}
		marker := "  "
		if index == 0 {
			marker = "› "
		}
		if theme.chip == "" {
			out.WriteString(theme.Accent(marker) + line)
			continue
		}
		out.WriteString(theme.chip + theme.sgr[roleAccent] + marker + theme.sgr[roleInk] + line + "\x1b[K" + reset)
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

// ResultPreview limits the total number of displayed lines, including the
// disclosure line. For failures, it favors the tail where exit status and error
// summaries commonly appear. Nonpositive budgets display nothing.
func ResultPreview(content string, isError bool, maxLines int, theme Theme) string {
	if maxLines <= 0 {
		return ""
	}
	text := strings.TrimSuffix(Safe(content), "\n")
	lines := strings.Split(text, "\n")
	style := theme.Graphite
	if isError {
		style = theme.Red
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

// Footer is the one-line status under the composer: model, effort, and a
// context gauge against the compaction threshold, drawn as the squares of a
// model card. compactTokens 0 means that automatic compaction is off; the
// footer then gives only the estimate.
func Footer(model, effort string, contextTokens, compactTokens int, theme Theme) string {
	separator := theme.Hairline(" · ")
	var out strings.Builder
	out.WriteString(theme.Graphite(ModelName(model)))
	out.WriteString(separator)
	out.WriteString(theme.Graphite(singleLine(effort)))
	out.WriteString(separator)
	if compactTokens > 0 {
		out.WriteString(theme.Gauge(contextTokens, compactTokens, 10))
		out.WriteString(theme.Graphite(" " + shortTokens(contextTokens) + " / " + shortTokens(compactTokens)))
	} else {
		out.WriteString(theme.Graphite("context ~" + shortTokens(contextTokens)))
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
