// Package presentation formats untrusted conversation text without terminal I/O.
package presentation

import (
	"fmt"
	"math"
	"path/filepath"
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

// wordmark uses the site's letter spacing without claiming its identity.
const wordmark = "I N K L I N G"

// Banner identifies the workspace. Current model, effort, and context use
// belong in the footer; full session details remain available through /status.
func Banner(directory string, width int, theme Theme) string {
	if !bannerHasMark(width, theme) {
		text := bannerText(directory, theme)
		return "\n  " + strings.Join(text, "\n\n  ") + "\n"
	}
	return bannerAt(1, directory, theme)
}

// BannerFrames reveals the mark and workspace without moving the composer.
// It is nil when the terminal has no room or color support for the mark.
func BannerFrames(directory string, width int, theme Theme, count int) []string {
	if !bannerHasMark(width, theme) || count < 2 {
		return nil
	}
	frames := make([]string, count)
	for index := range frames {
		frames[index] = bannerAt(float64(index)/float64(count-1), directory, theme)
	}
	return frames
}

func bannerHasMark(width int, theme Theme) bool {
	return theme.depth >= ANSI256 && width >= 64
}

func bannerText(directory string, theme Theme) []string {
	return []string{
		theme.Graphite(wordmark),
		theme.Bold(theme.Ink(singleLine(filepath.Base(directory)))),
		theme.Graphite("/help commands") + theme.Hairline(" · ") + theme.Graphite("/status session"),
	}
}

// Keep all frames the same height so the prompt stays in place.
func bannerAt(progress float64, directory string, theme Theme) string {
	text := bannerText(directory, theme)
	if progress < 1 {
		typed := int(math.Round(float64(len(wordmark)) * min(max((progress-0.25)/0.5, 0), 1)))
		text[0] = theme.Graphite(wordmark[:typed])
		if progress < 0.8 {
			text[1], text[2] = "", ""
		}
	}
	// Separate the workspace name from command hints beside the small mark.
	beside := []string{"", text[0], "", text[1], text[2]}
	var out strings.Builder
	out.WriteByte('\n')
	for index, row := range theme.MarkReveal(progress) {
		if index > 0 {
			out.WriteByte('\n')
		}
		out.WriteString("  ")
		out.WriteString(row)
		if index < len(beside) && beside[index] != "" {
			out.WriteString("   " + beside[index])
		}
	}
	out.WriteByte('\n')
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

// ResultPreview limits ordinary output lines and adds a disclosure when text
// is omitted. Failure previews keep the start and end. Source truncation
// notices are always shown in full, even when they exceed the line budget.
// Nonpositive budgets display nothing.
func ResultPreview(content string, isError bool, maxLines int, theme Theme) string {
	if maxLines <= 0 {
		return ""
	}
	text := strings.TrimRight(Safe(content), "\n")
	if text == "" {
		return ""
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
	if len(lines) <= maxLines && !shortened {
		return text
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
	var out strings.Builder
	appendLine := func(line string) {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line)
	}
	for _, line := range lines[:head] {
		appendLine(line)
	}
	omitted := len(lines) - kept
	for _, line := range lines[head : len(lines)-tail] {
		if resultNotice(line) {
			appendLine(line)
			omitted--
		}
	}
	notice := fmt.Sprintf("[%d lines omitted; /tools shows the full result]", omitted)
	if shortened {
		notice = fmt.Sprintf("[%d lines omitted; long lines shortened; /tools shows the full result]", omitted)
	}
	if omitted > 0 || shortened {
		appendLine(theme.Graphite(notice))
	}
	for _, line := range lines[len(lines)-tail:] {
		appendLine(line)
	}
	return out.String()
}

// These notices come from tool output limits, not the preview limit.
func resultNotice(line string) bool {
	return strings.HasPrefix(line, "[output truncated:") || strings.HasPrefix(line, "[truncated:")
}

// ContextUse is what the footer reports: the model, the effort, and the
// context estimate against the compaction threshold. CompactTokens 0 means
// that automatic compaction is off.
type ContextUse struct {
	Model, Effort string
	Tokens        int
	CompactTokens int
}

// Footer keeps the model and current context visible after the header leaves
// scrollback. Show the gauge only near the compaction threshold, where it
// gives useful warning instead of adding an empty row of marks.
func Footer(use ContextUse, shownTokens int, glow float64, theme Theme) string {
	separator := theme.Hairline(" · ")
	var out strings.Builder
	out.WriteString(theme.Graphite(ModelName(use.Model)))
	out.WriteString(separator)
	out.WriteString(theme.Graphite(singleLine(use.Effort)))
	out.WriteString(separator)
	if use.CompactTokens > 0 {
		if shownTokens >= use.CompactTokens-use.CompactTokens/4 {
			out.WriteString(theme.Gauge(shownTokens, use.CompactTokens, 4, glow))
			out.WriteByte(' ')
		}
		out.WriteString(theme.Graphite(shortTokens(shownTokens) + " / " + shortTokens(use.CompactTokens) + " context"))
	} else {
		out.WriteString(theme.Graphite("context ~" + shortTokens(shownTokens)))
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
