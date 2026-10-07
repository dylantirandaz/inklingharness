package terminal

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxInputBytes = 1 << 20

type keyKind uint8

const (
	keyText keyKind = iota
	keyEnter
	keyNewline
	keyInterrupt
	keyEscape
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyDelete
	keyBackspace
	keyClear
)

type key struct {
	kind  keyKind
	text  string
	paste bool
	stale bool
}

type parser struct {
	pending               []byte
	paste                 bool
	pasteCR, stalePending bool
	escapeAt              time.Time
}

func (p *parser) feed(data []byte, now time.Time) []key {
	p.pending = append(p.pending, data...)
	var keys []key
	for len(p.pending) > 0 {
		b := p.pending[0]
		stale := p.stalePending
		if b == 27 {
			if p.escapeAt.IsZero() {
				p.escapeAt = now
			}
			if len(p.pending) == 1 {
				break
			}
			if p.pending[1] == '[' || p.pending[1] == 'O' {
				end := 2
				for end < len(p.pending) && (p.pending[end] < 0x40 || p.pending[end] > 0x7e) {
					end++
				}
				if end == len(p.pending) {
					if end < 64 {
						break
					}
					p.pending = p.pending[end:]
					p.escapeAt = time.Time{}
					p.stalePending = false
					continue
				}
				sequence := string(p.pending[:end+1])
				p.pending = p.pending[end+1:]
				p.escapeAt = time.Time{}
				p.stalePending = false
				if sequence == "\x1b[200~" {
					p.paste = true
					p.pasteCR = false
					continue
				}
				if sequence == "\x1b[201~" {
					p.paste = false
					continue
				}
				var kind keyKind
				known := true
				switch sequence {
				case "\x1b[A", "\x1bOA":
					kind = keyUp
				case "\x1b[B", "\x1bOB":
					kind = keyDown
				case "\x1b[C", "\x1bOC":
					kind = keyRight
				case "\x1b[D", "\x1bOD":
					kind = keyLeft
				case "\x1b[H", "\x1bOH", "\x1b[1~", "\x1b[7~":
					kind = keyHome
				case "\x1b[F", "\x1bOF", "\x1b[4~", "\x1b[8~":
					kind = keyEnd
				case "\x1b[3~":
					kind = keyDelete
				default:
					known = false
				}
				if known && !p.paste {
					keys = append(keys, key{kind: kind, stale: stale})
				}
				continue
			}
			p.pending = p.pending[1:]
			p.escapeAt = time.Time{}
			p.stalePending = false
			keys = append(keys, key{kind: keyEscape, paste: p.paste, stale: stale})
			continue
		}
		if !utf8.FullRune(p.pending) {
			break
		}
		r, size := utf8.DecodeRune(p.pending)
		p.pending = p.pending[size:]
		p.stalePending = false
		k := key{kind: keyText, text: string(r), paste: p.paste, stale: stale}
		switch r {
		case 3:
			k.kind = keyInterrupt
		case '\r':
			k.kind = keyEnter
		case '\n':
			k.kind = keyNewline
		case 27:
			k.kind = keyEscape
		case 8, 127:
			k.kind = keyBackspace
		case 21:
			k.kind = keyClear
		default:
			if unicode.IsControl(r) && r != '\t' {
				continue
			}
		}
		if p.paste {
			skipLF := p.pasteCR && r == '\n'
			p.pasteCR = r == '\r'
			if skipLF {
				continue
			}
			if r == '\r' || r == '\n' {
				k = key{kind: keyText, text: "\n", paste: true, stale: stale}
			} else if k.kind != keyText {
				continue
			}
		}
		keys = append(keys, k)
	}
	return keys
}

// escapeTimeout separates a lone Escape key from the start of an escape
// sequence that has not fully arrived.
const escapeTimeout = 40 * time.Millisecond

func (p *parser) expire(now time.Time) []key {
	if deadline, pending := p.escapeDeadline(); pending && !now.Before(deadline) {
		stale := p.stalePending
		p.pending = nil
		p.escapeAt = time.Time{}
		p.stalePending = false
		return []key{{kind: keyEscape, paste: p.paste, stale: stale}}
	}
	return nil
}

// escapeDeadline reports when a pending lone Escape becomes a key.
func (p *parser) escapeDeadline() (time.Time, bool) {
	if len(p.pending) == 1 && p.pending[0] == 27 && !p.escapeAt.IsZero() {
		return p.escapeAt.Add(escapeTimeout), true
	}
	return time.Time{}, false
}

// RuneWidth treats combining marks and joiners as zero cells. Wide East Asian
// characters and emoji use two cells, matching ordinary modern terminals.
func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d || r == 0xfe0f || r == 0xfe0e {
		return 0
	}
	if r < 32 || r == 127 {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) || (r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f000 && r <= 0x1faff) || (r >= 0x20000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

func previousCluster(text []rune, pos int) int {
	if pos == 0 {
		return 0
	}
	pos--
	for pos > 0 && runeWidth(text[pos]) == 0 {
		pos--
	}
	return pos
}

func nextCluster(text []rune, pos int) int {
	if pos >= len(text) {
		return len(text)
	}
	pos++
	for pos < len(text) && runeWidth(text[pos]) == 0 {
		pos++
	}
	return pos
}

// styledRows wraps only visible cells, preserving trusted SGR sequences. Other
// terminal controls belong to the caller's sanitization boundary.
func styledRows(text string, width int, color bool) []string {
	if width < 1 {
		width = 1
	}
	rows := []string{}
	var row strings.Builder
	cells := 0
	style := ""
	for len(text) > 0 {
		if strings.HasPrefix(text, "\x1b[") {
			end := 2
			for end < len(text) && (text[end] < 0x40 || text[end] > 0x7e) {
				end++
			}
			if end < len(text) {
				if color && text[end] == 'm' {
					sequence := text[:end+1]
					if sequence == "\x1b[0m" || sequence == "\x1b[m" {
						style = ""
					} else {
						style += sequence
					}
					row.WriteString(sequence)
				}
				text = text[end+1:]
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		if r == '\n' {
			rows = append(rows, row.String())
			row.Reset()
			cells = 0
			row.WriteString(style)
			continue
		}
		if r == '\t' {
			spaces := 4 - cells%4
			text = strings.Repeat(" ", spaces) + text
			continue
		}
		w := runeWidth(r)
		if cells+w > width {
			rows = append(rows, row.String())
			row.Reset()
			cells = 0
			row.WriteString(style)
		}
		row.WriteRune(r)
		cells += w
	}
	return append(rows, row.String())
}

func cellWidth(text string) int {
	width := 0
	for _, r := range text {
		width += runeWidth(r)
	}
	return width
}

func withoutStyles(text string) string {
	var b strings.Builder
	for len(text) > 0 {
		if strings.HasPrefix(text, "\x1b[") {
			end := 2
			for end < len(text) && (text[end] < 0x40 || text[end] > 0x7e) {
				end++
			}
			if end < len(text) && text[end] == 'm' {
				text = text[end+1:]
				continue
			}
		}
		b.WriteByte(text[0])
		text = text[1:]
	}
	return b.String()
}
