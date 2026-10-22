package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// DocumentFormat selects the style of a read-only document.
type DocumentFormat uint8

const (
	PlainDocument DocumentFormat = iota
	DiffDocument
)

// Document holds untrusted text. View removes terminal controls before display.
// Mark, when set, is the styled step mark that opens the document's tab, as
// the step that the document shows opened its row in the transcript.
type Document struct {
	Title  string
	Text   string
	Format DocumentFormat
	Mark   string
}

type documentRow struct {
	start, end, line int
}

type indexedDocument struct {
	title, text string
	ready       bool
	width, top  int
	rows        []documentRow
}

type documentView struct {
	title     string
	documents []Document
	// labels are the tab texts, numbered and clipped once.
	labels   []string
	cache    []indexedDocument
	selected int
	// The main screen is left in place. DEC mode 1049 saves its cursor while
	// the viewer uses separate live rows on the temporary alternate screen.
	live                 []string
	cursorRow, cursorCol int
	cursorVisible        bool
	rows, cols           int
}

// View shows immutable documents on a temporary alternate screen. It uses the
// screen's reader and cannot run beside Prompt, Choose, or another View. Each
// document is a tab: arrows, Tab, or a digit select one; arrows, j/k,
// PageUp/PageDown, Home/End, and g/G move through its text. Escape, q, Enter,
// or Ctrl-C returns to the normal transcript.
func (s *Screen) View(ctx context.Context, title string, documents []Document, selected int) error {
	if len(documents) == 0 {
		return errors.New("terminal view requires at least one document")
	}
	if selected < 0 || selected >= len(documents) {
		return errors.New("terminal view selection is out of range")
	}
	for _, document := range documents {
		switch document.Format {
		case PlainDocument, DiffDocument:
		default:
			return fmt.Errorf("terminal view has unknown document format %d", document.Format)
		}
	}
	_, err := s.await(ctx, modeView, "", newDocumentView(title, documents, selected))
	return err
}

func newDocumentView(title string, documents []Document, selected int) *documentView {
	labels := make([]string, len(documents))
	for index, document := range documents {
		label := fitDocumentLabel(documentTitle(document.Title), tabLabelWidth)
		if label == "" {
			label = "Document " + strconv.Itoa(index+1)
		}
		labels[index] = strconv.Itoa(index+1) + " " + label
	}
	return &documentView{
		title: documentTitle(title), documents: documents, selected: selected,
		labels: labels, cache: make([]indexedDocument, len(documents)),
	}
}

func documentTitle(text string) string {
	return strings.Join(strings.Fields(presentation.Safe(text)), " ")
}

func (s *Screen) enterViewLocked(view *documentView) {
	view.live = s.live
	view.cursorRow, view.cursorCol = s.cursorRow, s.cursorCol
	view.cursorVisible = s.cursorVisible
	view.rows, view.cols = s.rows, s.cols
	s.view, s.live = view, nil
	s.cursorRow, s.cursorCol, s.cursorVisible = 0, 0, false
	s.outputLocked("\x1b[?1049h\x1b[H\x1b[2J\x1b[?25l")
}

// leaveViewLocked must also work after a write fault. It restores the main
// buffer before an awaiting caller can return, including on EOF or Close.
func (s *Screen) leaveViewLocked() {
	view := s.view
	s.view = nil
	s.live = view.live
	s.cursorRow, s.cursorCol = view.cursorRow, view.cursorCol
	s.cursorVisible = view.cursorVisible
	s.paint.Reset()
	s.paint.WriteString("\x1b[?1049l")
	if s.cursorVisible {
		s.paint.WriteString("\x1b[?25h")
	} else {
		s.paint.WriteString("\x1b[?25l")
	}
	if view.rows != s.rows || view.cols != s.cols {
		s.reflowCursorLocked(s.rows, s.cols)
		s.eraseIntoPaintLocked()
	}
	n, err := s.output.Write(s.paint.Bytes())
	if err == nil && n != s.paint.Len() {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.fault = errors.Join(s.fault, err)
	}
	s.paint.Reset()
	s.layoutDirty = true
}

// prepare removes controls once and stores byte ranges, not styled output.
// Only a width change scans the whole document. Scrolling formats visible rows.
func (d *indexedDocument) prepare(source Document, width int) {
	if !d.ready {
		d.title = documentTitle(source.Title)
		d.text = documentText(source.Text)
		d.ready = true
	}
	if d.width == width {
		return
	}
	anchor := 0
	if d.top < len(d.rows) {
		anchor = d.rows[d.top].start
	}
	d.width = width
	d.rows = d.rows[:0]
	start, line, cells := 0, 0, 0
	for offset, r := range d.text {
		if r == '\n' {
			d.rows = append(d.rows, documentRow{start, offset, line})
			start, line, cells = offset+1, offset+1, 0
			continue
		}
		w := runeWidth(r)
		if cells > 0 && cells+w > width {
			d.rows = append(d.rows, documentRow{start, offset, line})
			start, cells = offset, 0
		}
		cells += w
	}
	d.rows = append(d.rows, documentRow{start, len(d.text), line})
	d.top = max(0, sort.Search(len(d.rows), func(i int) bool { return d.rows[i].start > anchor })-1)
}

// Tabs use four-cell stops on each logical line. Expand them once so a wrapped
// tab cannot hide text or need a full-line scan when its row becomes visible.
func documentText(raw string) string {
	text := presentation.Safe(raw)
	if !strings.ContainsRune(text, '\t') {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	column := 0
	for _, r := range text {
		switch r {
		case '\t':
			spaces := 4 - column%4
			out.WriteString("    "[:spaces])
			column += spaces
		case '\n':
			out.WriteByte('\n')
			column = 0
		default:
			out.WriteRune(r)
			column += runeWidth(r)
		}
	}
	return out.String()
}

// viewChrome is the fixed furniture around the document body. Small screens
// give up the rule first, then the footer, then the tab row.
type viewChrome struct {
	tabs, rule, footer bool
	bodyRows           int
	// textWidth is the width of document text; the scrollbar and its gap
	// take the remaining cells when the screen is wide enough.
	textWidth int
	scrollbar bool
}

// scrollbarMinimumWidth is the narrowest screen that gives two cells to a
// scrollbar without crowding the text.
const scrollbarMinimumWidth = 40

func (s *Screen) viewChromeLocked() viewChrome {
	width := max(s.cols-1, 2)
	chrome := viewChrome{tabs: s.rows > 1, footer: s.rows > 2, rule: s.rows >= 6, textWidth: width}
	if width >= scrollbarMinimumWidth {
		chrome.scrollbar = true
		chrome.textWidth = width - 2
	}
	used := 0
	for _, present := range []bool{chrome.tabs, chrome.footer, chrome.rule} {
		if present {
			used++
		}
	}
	chrome.bodyRows = max(1, s.rows-used)
	return chrome
}

// fitDocumentLabel clips only labels. Document text wraps, and all its rows
// remain available, including empty lines and the final line.
func fitDocumentLabel(text string, width int) string {
	cells := 0
	for offset, r := range text {
		cells += runeWidth(r)
		if cells > width {
			return text[:offset]
		}
	}
	return text
}

// tabLabelWidth bounds one tab so several documents share the row.
const tabLabelWidth = 28

// viewTabsLocked draws one tab per document, with the selected one in the
// accent and a window of its neighbors that fits the width. The view title
// ends the row when room remains.
func (s *Screen) viewTabsLocked(width int) string {
	view := s.view
	labels := view.labels
	// A tab is its mark, a space, and its label.
	tabWidth := func(index int) int {
		cells := cellWidth(labels[index])
		if view.documents[index].Mark != "" {
			cells += 2
		}
		return cells
	}
	first, last := view.selected, view.selected
	cells := tabWidth(view.selected)
	for first > 0 || last < len(labels)-1 {
		grew := false
		if last < len(labels)-1 && cells+2+tabWidth(last+1) <= width-4 {
			last++
			cells += 2 + tabWidth(last)
			grew = true
		}
		if first > 0 && cells+2+tabWidth(first-1) <= width-4 {
			first--
			cells += 2 + tabWidth(first)
			grew = true
		}
		if !grew {
			break
		}
	}
	var row strings.Builder
	if first > 0 {
		row.WriteString(s.theme.Graphite("… "))
		cells += 2
	}
	for index := first; index <= last; index++ {
		if index > first {
			row.WriteString("  ")
		}
		// A mark is a cell of its own before the label, so the label widths
		// that placed the tabs stay true.
		if mark := view.documents[index].Mark; mark != "" {
			row.WriteString(mark + " ")
		}
		if index == view.selected {
			row.WriteString(s.theme.Bold(s.theme.Ink(labels[index])))
		} else {
			row.WriteString(s.theme.Graphite(labels[index]))
		}
	}
	if last < len(labels)-1 {
		row.WriteString(s.theme.Graphite(" …"))
		cells += 2
	}
	if room := width - cells - 3; view.title != "" && cellWidth(view.title) <= room {
		row.WriteString(strings.Repeat(" ", room-cellWidth(view.title)+3))
		row.WriteString(s.theme.Graphite(view.title))
	}
	return row.String()
}

// scrollThumb gives the rows of the thumb for a body of bodyRows that shows
// rows total starting at top. A document that fits has no thumb.
func scrollThumb(top, total, bodyRows int) (start, end int) {
	if total <= bodyRows || bodyRows <= 0 {
		return 0, 0
	}
	size := max(1, bodyRows*bodyRows/total)
	start = top * (bodyRows - size) / (total - bodyRows)
	return start, start + size
}

func (s *Screen) layoutViewLocked() {
	view := s.view
	width := max(s.cols-1, 2)
	chrome := s.viewChromeLocked()
	doc := &view.cache[view.selected]
	source := view.documents[view.selected]
	doc.prepare(source, chrome.textWidth)
	doc.top = min(doc.top, max(0, len(doc.rows)-chrome.bodyRows))
	clear(s.frame)
	s.frame = s.frame[:0]
	s.activityRow, s.footerRow = -1, -1
	if chrome.tabs {
		s.frame = append(s.frame, s.viewTabsLocked(width))
	}
	if chrome.rule {
		s.frame = append(s.frame, s.theme.Rule(width))
	}
	thumbStart, thumbEnd := scrollThumb(doc.top, len(doc.rows), chrome.bodyRows)
	for offset := range chrome.bodyRows {
		index := doc.top + offset
		text := ""
		if index < len(doc.rows) {
			row := doc.rows[index]
			text = doc.text[row.start:row.end]
			prefix := doc.text[row.line:min(row.line+6, len(doc.text))]
			switch source.Format {
			case DiffDocument:
				text = presentation.DiffLine(text, prefix, s.theme)
			case PlainDocument:
				// A tool document shows its change as "- " and "+ " lines.
				if strings.HasPrefix(prefix, "- ") || strings.HasPrefix(prefix, "+ ") {
					text = presentation.DiffLine(text, prefix, s.theme)
				}
			}
		}
		if chrome.scrollbar && thumbEnd > thumbStart {
			text += strings.Repeat(" ", max(chrome.textWidth-cellWidth(withoutStyles(text)), 0)+1)
			if offset >= thumbStart && offset < thumbEnd {
				text += s.theme.Accent("┃")
			} else {
				text += s.theme.Hairline("│")
			}
		}
		s.frame = append(s.frame, text)
	}
	for len(s.frame) < s.rows {
		s.frame = append(s.frame, "")
	}
	if chrome.footer {
		shown := min(doc.top+chrome.bodyRows, len(doc.rows))
		position := strconv.Itoa(doc.top+1) + "-" + strconv.Itoa(shown) + "/" + strconv.Itoa(len(doc.rows))
		if len(view.documents) > 1 {
			position = strconv.Itoa(view.selected+1) + "/" + strconv.Itoa(len(view.documents)) + " · " + position
		}
		hint := "↑↓ j k scroll · PgUp PgDn · g G ends · ←→ Tab 1-9 tabs · Esc q close"
		if cellWidth(position)+3+cellWidth(hint) > width {
			hint = "↑↓ PgUp/PgDn g G · ←→ Tab tabs · q close"
		}
		if cellWidth(position)+3+cellWidth(hint) > width {
			hint = "↑↓ ←→ · q close"
		}
		if cellWidth(position)+3+cellWidth(hint) > width {
			hint = "q close"
		}
		footer := hint + " · " + position
		s.frame[len(s.frame)-1] = s.theme.Graphite(fitDocumentLabel(footer, width))
	}
	s.frameRow, s.frameCol = 0, 0
	s.layoutDirty = false
}

func (s *Screen) viewKeyLocked(k key) {
	if k.paste {
		return
	}
	view := s.view
	chrome := s.viewChromeLocked()
	doc := &view.cache[view.selected]
	doc.prepare(view.documents[view.selected], chrome.textWidth)
	bodyRows := chrome.bodyRows
	last := max(0, len(doc.rows)-bodyRows)
	selected, top := view.selected, doc.top
	switch k.kind {
	case keyEscape, keyEnter:
		s.finishLocked(answer{})
		return
	case keyText:
		switch k.text {
		case "q":
			s.finishLocked(answer{})
			return
		case "j":
			doc.top = min(last, doc.top+1)
		case "k":
			doc.top = max(0, doc.top-1)
		case "g":
			doc.top = 0
		case "G":
			doc.top = last
		case "\t":
			view.selected = (view.selected + 1) % len(view.documents)
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if number := int(k.text[0] - '0'); number <= len(view.documents) {
				view.selected = number - 1
			}
		}
	case keyLeft:
		view.selected = max(0, view.selected-1)
	case keyRight:
		view.selected = min(len(view.documents)-1, view.selected+1)
	case keyUp:
		doc.top = max(0, doc.top-1)
	case keyDown:
		doc.top = min(last, doc.top+1)
	case keyPageUp:
		doc.top = max(0, doc.top-bodyRows)
	case keyPageDown:
		doc.top = min(last, doc.top+bodyRows)
	case keyHome:
		doc.top = 0
	case keyEnd:
		doc.top = last
	case keyNewline, keyInterrupt, keyDelete, keyBackspace, keyClear,
		keyWordLeft, keyWordRight, keyWordBackspace, keyWordDelete,
		keyHistoryPrevious, keyHistoryNext:
	default:
		panic(fmt.Sprintf("terminal view: unknown key kind %d", k.kind))
	}
	if selected != view.selected || top != doc.top {
		s.layoutDirty = true
	}
}
