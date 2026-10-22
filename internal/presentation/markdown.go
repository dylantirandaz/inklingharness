package presentation

import "strings"

// Markdown retains only the currently open code fence. Pass complete lines to
// Line, and the whole current partial line to Preview (not successive chunks).
// A preview never advances state, so it can safely be replaced as text arrives.
type Markdown struct {
	theme       Theme
	fence       byte
	fenceLength int
	code        codeState
}

func NewMarkdown(theme Theme) *Markdown {
	return &Markdown{theme: theme}
}

func (m *Markdown) Line(text string) string {
	return m.render(Safe(text), true)
}

func (m *Markdown) Preview(text string) string {
	return m.render(Safe(text), false)
}

// Unmarked is a partial reply line as plain text without its Markdown marks,
// for the preview that streams before the line commits: no emphasis marks or
// backticks, a list mark as a bullet, and a heading without its hashes. It
// grows as the line grows, so its letters can dry in place, and the marks
// never flash on screen before they vanish. Inside a code block the line is
// code and stays as it is. It does not change the state of the fence.
func (m *Markdown) Unmarked(text string) string {
	text = Safe(text)
	if m.InCode() {
		return text
	}
	trimmed := strings.TrimLeft(text, " ")
	indent := text[:len(text)-len(trimmed)]
	if len(indent) >= 4 || strings.HasPrefix(trimmed, "\t") || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		return text
	}
	switch {
	case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "), strings.HasPrefix(trimmed, "+ "):
		trimmed = "▸ " + trimmed[2:]
	case strings.HasPrefix(trimmed, "#"):
		trimmed = strings.TrimLeft(strings.TrimLeft(trimmed, "#"), " ")
	}
	return indent + strings.NewReplacer("**", "", "__", "", "`", "").Replace(trimmed)
}

// InCode reports whether a code fence is open.
func (m *Markdown) InCode() bool { return m.fence != 0 }

// EndCode closes an open fence, as the end of a reply does.
func (m *Markdown) EndCode() {
	m.fence, m.fenceLength, m.code = 0, 0, codeState{}
}

func (m *Markdown) render(text string, commit bool) string {
	trimmed := strings.TrimLeft(text, " ")
	indent := len(text) - len(trimmed)
	var marker byte
	length := 0
	if indent <= 3 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
		marker = trimmed[0]
		length = markerRun(trimmed, 0, marker)
	}
	if m.fence != 0 {
		if marker == m.fence && length >= m.fenceLength && strings.TrimSpace(trimmed[length:]) == "" {
			if commit {
				m.EndCode()
			}
			// The block ends in air, not in a second mark.
			return ""
		}
		if !commit {
			// A preview colors against a copy, so a partial line never
			// leaves a comment or string open for the committed one.
			state := m.code
			return highlight(text, &state, m.theme)
		}
		return highlight(text, &m.code, m.theme)
	}
	if indent >= 4 || strings.HasPrefix(trimmed, "\t") {
		return text
	}
	if length >= 3 && (marker != '`' || !strings.Contains(trimmed[length:], "`")) {
		if commit {
			m.fence, m.fenceLength = marker, length
			m.code = codeState{lang: languageFor(trimmed[length:])}
		}
		// The fence shows as the language in gray after a short line of
		// force, a quiet label above the code, not as backticks. A fence
		// with no language leaves air.
		if language := strings.TrimSpace(trimmed[length:]); language != "" && m.theme.Blends() {
			return m.theme.forceRule(4, 4) + " " + m.theme.Graphite(language)
		}
		return m.theme.Graphite(strings.TrimSpace(trimmed[length:]))
	}
	if indent <= 3 && strings.HasPrefix(trimmed, "#") {
		count := markerRun(trimmed, 0, '#')
		if count <= 6 && count < len(trimmed) && trimmed[count] == ' ' {
			return m.theme.Bold(m.theme.Ink(inline(strings.TrimLeft(trimmed[count:], " "), m.theme, 0)))
		}
	}
	if strings.HasPrefix(trimmed, "> ") {
		return text[:indent] + m.theme.Hairline("│ ") + m.theme.Italic(inline(trimmed[2:], m.theme, 0))
	}
	if len(trimmed) >= 2 && (trimmed[0] == '-' || trimmed[0] == '*' || trimmed[0] == '+') && trimmed[1] == ' ' {
		// A list item leads with a small wedge, the step mark at the size
		// of the text.
		return text[:indent] + m.theme.Accent("▸ ") + inline(trimmed[2:], m.theme, 0)
	}
	return inline(text, m.theme, 0)
}

func markerRun(text string, start int, marker byte) int {
	end := start
	for end < len(text) && text[end] == marker {
		end++
	}
	return end - start
}

// codeEnd matches the exact backtick run, allowing literal backticks within a
// longer-delimited inline code span.
func codeEnd(text string, start, count int) int {
	for index := start; index < len(text); {
		next := strings.IndexByte(text[index:], '`')
		if next < 0 {
			return -1
		}
		index += next
		run := markerRun(text, index, '`')
		if run == count {
			return index
		}
		index += run
	}
	return -1
}

func emphasisEnd(text string, start int, marker byte, count int) int {
	for index := start; index < len(text); index++ {
		if text[index] == '\\' {
			index++
			continue
		}
		if text[index] == '`' {
			run := markerRun(text, index, '`')
			end := codeEnd(text, index+run, run)
			if end >= 0 {
				index = end + run - 1
				continue
			}
			index += run - 1
			continue
		}
		if text[index] == marker {
			run := markerRun(text, index, marker)
			if run == count && index > start && text[index-1] != ' ' && text[index-1] != '\t' {
				if marker != '_' || index+run == len(text) || !wordByte(text[index+run]) {
					return index
				}
			}
			index += run - 1
		}
	}
	return -1
}

func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b >= 0x80
}

// inline deliberately leaves incomplete or unsupported constructs readable.
// Recursion is limited to nesting within one line, never the message history.
func inline(text string, theme Theme, depth int) string {
	if depth >= 8 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for index := 0; index < len(text); {
		switch text[index] {
		case '\\':
			if index+1 < len(text) && strings.ContainsRune("\\`*_{}[]()#+-.!", rune(text[index+1])) {
				out.WriteByte(text[index+1])
				index += 2
				continue
			}
		case '`':
			run := markerRun(text, index, '`')
			end := codeEnd(text, index+run, run)
			if end >= 0 {
				// Color marks the code, so the backticks go. Without color they
				// stay, so code remains distinct in plain text.
				if theme.Colored() {
					out.WriteString(theme.Code(text[index+run : end]))
				} else {
					out.WriteString(theme.Code(text[index : end+run]))
				}
				index = end + run
				continue
			}
			// An unfinished code span must stay literal while streaming.
			out.WriteString(text[index:])
			return out.String()
		case '*', '_':
			marker := text[index]
			run := markerRun(text, index, marker)
			if run <= 2 && index+run < len(text) && text[index+run] != ' ' && text[index+run] != '\t' &&
				(marker != '_' || index == 0 || !wordByte(text[index-1])) {
				end := emphasisEnd(text, index+run, marker, run)
				if end >= 0 {
					out.WriteString(theme.Bold(inline(text[index+run:end], theme, depth+1)))
					index = end + run
					continue
				}
			}
			out.WriteString(text[index : index+run])
			index += run
			continue
		case '[':
			labelEnd := strings.Index(text[index+1:], "](")
			if labelEnd >= 0 {
				labelEnd += index + 1
				targetStart := labelEnd + 2
				balance := 1
				end := targetStart
				for ; end < len(text); end++ {
					if text[end] == '(' {
						balance++
					}
					if text[end] == ')' {
						balance--
						if balance == 0 {
							break
						}
					}
				}
				if balance == 0 {
					out.WriteString(inline(text[index+1:labelEnd], theme, depth+1))
					out.WriteString(" (")
					out.WriteString(theme.Graphite(theme.Underline(text[targetStart:end])))
					out.WriteByte(')')
					index = end + 1
					continue
				}
			}
		}
		out.WriteByte(text[index])
		index++
	}
	return out.String()
}
