package tools

import "strings"

// cleanTerminalText returns command output as a terminal would show it.
// Progress bars rewrite one line with carriage returns, so each line keeps
// only the visible result of its overwrites. ANSI escape sequences, such as
// colors and cursor moves, are removed. A carriage return before a line feed
// is kept, so CRLF line endings stay visible.
func cleanTerminalText(text string) string {
	if !strings.ContainsAny(text, "\r\x1b") {
		return text
	}
	text = stripEscapes(text)
	if !strings.Contains(text, "\r") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for line := range strings.SplitAfterSeq(text, "\n") {
		body, newline := strings.CutSuffix(line, "\n")
		crlf := false
		if newline {
			body, crlf = strings.CutSuffix(body, "\r")
		}
		if strings.Contains(body, "\r") {
			body = overlay(body)
		}
		out.WriteString(body)
		if crlf {
			out.WriteByte('\r')
		}
		if newline {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// overlay applies carriage returns inside one line: each segment overwrites
// the line from its first column.
func overlay(line string) string {
	var visible []rune
	for segment := range strings.SplitSeq(line, "\r") {
		for index, r := range []rune(segment) {
			if index < len(visible) {
				visible[index] = r
			} else {
				visible = append(visible, r)
			}
		}
	}
	return string(visible)
}

// stripEscapes removes CSI sequences (ESC [ ... final byte), OSC sequences
// (ESC ] ... BEL or ESC \), and two-byte escapes.
func stripEscapes(text string) string {
	if !strings.Contains(text, "\x1b") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); {
		if text[i] != 0x1b || i+1 >= len(text) {
			out.WriteByte(text[i])
			i++
			continue
		}
		switch next := text[i+1]; {
		case next == '[':
			j := i + 2
			for j < len(text) && text[j] >= 0x20 && text[j] <= 0x3f {
				j++
			}
			if j < len(text) && text[j] >= 0x40 && text[j] <= 0x7e {
				i = j + 1
				continue
			}
			out.WriteByte(text[i])
			i++
		case next == ']':
			j := i + 2
			end := -1
			for ; j < len(text); j++ {
				if text[j] == 0x07 {
					end = j + 1
					break
				}
				if text[j] == 0x1b && j+1 < len(text) && text[j+1] == '\\' {
					end = j + 2
					break
				}
				if text[j] == '\n' {
					break
				}
			}
			if end < 0 {
				out.WriteByte(text[i])
				i++
				continue
			}
			i = end
		case next >= 0x40 && next <= 0x5f, next == '(' || next == ')':
			if next == '(' || next == ')' {
				i += min(3, len(text)-i)
			} else {
				i += 2
			}
		default:
			out.WriteByte(text[i])
			i++
		}
	}
	return out.String()
}
