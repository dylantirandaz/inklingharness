package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

const (
	// DEC private mode 2026 makes a supporting terminal show each frame at
	// once. Other terminals ignore the mode.
	syncBegin = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
)

type Choice int

const (
	Deny Choice = iota
	AllowOnce
	AllowSession
)

type inputMode uint8

const (
	modeBusy inputMode = iota
	modePrompt
	modeChoice
	modeView
)

type answer struct {
	text   string
	choice Choice
	err    error
}

// Screen owns one terminal input reader and serializes every output operation.
// Call Close before using either descriptor outside the screen again.
type Screen struct {
	mu sync.Mutex
	// place is what the footer says about where the work happens.
	place struct{ folder, branch string }
	// request is the approval shown above the choices; nil when it went to
	// the scrollback because the screen could not hold it.
	request *presentation.ApprovalRequest
	// choiceSince is when the current approval appeared; see choiceGrace.
	choiceSince time.Time
	// box is true while the live area draws its input inside a rounded bar;
	// the input then wraps four cells narrower.
	box bool
	// soft is true while the screen draws the gallery: the key hints sit in
	// the status bar under the composer, and an approval hangs on the page
	// in its own frame instead of a box.
	soft          bool
	input, output *os.File
	wake          wakePipe
	state         syscall.Termios
	onInterrupt   func()
	ctx           context.Context
	stop          chan struct{}
	done          chan struct{}
	resizes       chan os.Signal
	closed        bool
	fault         error
	closeErr      error
	theme         presentation.Theme
	rows, cols    int
	live          []string
	frame         []string
	layoutDirty   bool
	frameRow      int
	frameCol      int
	// activityRow and footerRow index the rows of frame that change without a
	// layout; -1 means that the frame has no such row.
	activityRow, footerRow int
	cursorVisible          bool
	paint                  bytes.Buffer
	// paintRows counts the rows of the current frame; paintMulti marks an
	// erase or scrollback text. Either makes the frame use synchronization.
	paintRows                int
	paintMulti               bool
	cursorRow, cursorCol     int
	pending                  bytes.Buffer
	preview, status          string
	reasoningText            string
	reasoningDirty           bool
	background               string
	title                    string
	rule                     cachedRule
	activitySince, lastFrame time.Time
	effects                  effects
	mode                     inputMode
	result                   chan answer
	view                     *documentView
	placeholder              string
	text                     []rune
	position, inputBytes     int
	history                  []string
	historyPosition          int
	draft                    string
	draftPosition            int
	verticalColumn           int
	verticalMove             bool
	inputTop, inputRows      int
	activityRows, footerRows int
	parser                   parser
	// commands feed the slash menu; menuIndex is its selection and
	// menuDismissed hides it until the input changes.
	commands      []Command
	menuMatches   []Command
	menuIndex     int
	menuDismissed bool
	// typing closes at the next prompt key other than Enter; see ArmTyping.
	typing chan struct{}
	// queue holds the messages that the user sent while the agent worked,
	// oldest first.
	queue []queuedMessage
}

// cachedRule keeps the composer rule for one width, so a redraw does not
// compute its gradient again.
type cachedRule struct {
	width int
	text  string
}

// wakePipe ends the read loop's wait when state that the wait depends on
// changes: a new animation, a resize, cancellation, or Close. Without it the
// loop would have to poll. A zero value has no pipe and never wakes.
type wakePipe struct {
	read, write int
	open        bool
}

func newWakePipe() (wakePipe, error) {
	var descriptors [2]int
	if err := syscall.Pipe(descriptors[:]); err != nil {
		return wakePipe{}, fmt.Errorf("terminal wake pipe: %w", err)
	}
	for _, descriptor := range descriptors {
		syscall.CloseOnExec(descriptor)
		if err := syscall.SetNonblock(descriptor, true); err != nil {
			syscall.Close(descriptors[0])
			syscall.Close(descriptors[1])
			return wakePipe{}, fmt.Errorf("terminal wake pipe: %w", err)
		}
	}
	return wakePipe{read: descriptors[0], write: descriptors[1], open: true}, nil
}

// signal makes the next or current wait return. A full pipe already holds a
// pending wake, so a failed write needs no handling.
func (w wakePipe) signal() {
	if w.open {
		syscall.Write(w.write, []byte{0})
	}
}

func (w wakePipe) drain() {
	var data [64]byte
	for {
		if n, err := syscall.Read(w.read, data[:]); n <= 0 || err != nil {
			return
		}
	}
}

func (w wakePipe) close() {
	if w.open {
		syscall.Close(w.read)
		syscall.Close(w.write)
	}
}

// New takes over the terminal. It asks the terminal for its background, so
// the theme fits a light or a dark terminal; NoColor skips the question and
// draws without color.
func New(ctx context.Context, input, output *os.File, onInterrupt func(), style Style) (*Screen, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || output == nil {
		return nil, errors.New("terminal requires input and output descriptors")
	}
	if os.Getenv("TERM") == "dumb" {
		return nil, errors.New("terminal requires cursor control; TERM=dumb")
	}
	state, err := getState(input)
	if err != nil {
		return nil, fmt.Errorf("terminal input: %w", err)
	}
	if _, err := getState(output); err != nil {
		return nil, fmt.Errorf("terminal output: %w", err)
	}
	if err := checkDescriptor(int(input.Fd())); err != nil {
		return nil, err
	}
	rows, cols, err := terminalSize(input)
	if err != nil {
		return nil, err
	}
	wake, err := newWakePipe()
	if err != nil {
		return nil, err
	}
	if err := checkDescriptor(wake.read); err != nil {
		wake.close()
		return nil, err
	}
	raw := rawState(state)
	if err := setState(input, &raw); err != nil {
		wake.close()
		restoreErr := setState(input, &state)
		return nil, errors.Join(err, restoreErr)
	}
	theme, typed := presentation.NewTheme(style.Depth, presentation.DarkBackground, style.Accent), []byte(nil)
	if style.Depth != presentation.NoColor {
		var background *terminalBackground
		background, typed, err = queryBackground(input, output, wake.read)
		if err != nil {
			wake.close()
			restoreErr := setState(input, &state)
			return nil, errors.Join(err, restoreErr)
		}
		// A terminal that does not report its background is taken as dark,
		// the common default.
		if background != nil {
			theme = theme.OnPaper(background.r, background.g, background.b)
		}
	}
	s := &Screen{
		input: input, output: output, wake: wake, state: state, onInterrupt: onInterrupt,
		ctx: ctx, stop: make(chan struct{}), done: make(chan struct{}),
		resizes: make(chan os.Signal, 1), rows: rows, cols: cols, layoutDirty: true,
		theme: theme, activityRow: -1, footerRow: -1,
		effects: effects{motion: style.Motion, epoch: time.Now()},
	}
	// Keys typed while the terminal answered the background query get the
	// same handling as keys read later.
	s.mu.Lock()
	interrupt, interruptedResult := false, s.result
	for _, k := range s.parser.feed(typed, time.Now()) {
		if s.keyLocked(k) {
			interrupt = true
		}
	}
	s.mu.Unlock()
	// Bracketed paste on, cursor hidden, window title saved for Close, and a
	// steady bar cursor for the composer.
	if err := s.outputLocked("\x1b[?2004h\x1b[?25l\x1b[22;0t\x1b[6 q"); err == nil {
		s.drawLocked()
	}
	if s.fault != nil {
		_, cursorErr := io.WriteString(output, "\x1b[?2004l\x1b[?25h\x1b[0 q\x1b[23;0t")
		restoreErr := setState(input, &state)
		wake.close()
		return nil, errors.Join(s.fault, cursorErr, restoreErr)
	}
	signal.Notify(s.resizes, syscall.SIGWINCH)
	go s.watch()
	go s.readLoop()
	if interrupt {
		go s.interrupted(interruptedResult)
	}
	return s, nil
}

// Theme is the theme that New chose for this terminal. It does not change.
func (s *Screen) Theme() presentation.Theme { return s.theme }

// watch applies resizes and wakes the read loop when the context ends, so
// the read loop itself can block without a timeout.
func (s *Screen) watch() {
	for {
		select {
		case <-s.stop:
			return
		case <-s.ctx.Done():
			s.wake.signal()
			return
		case <-s.resizes:
			rows, cols, err := terminalSize(s.input)
			s.mu.Lock()
			if !s.closed {
				if err != nil {
					s.failLocked(fmt.Errorf("terminal resize: %w", err))
				} else {
					s.resizeLocked(rows, cols)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Screen) outputLocked(text string) error {
	if s.fault != nil {
		return s.fault
	}
	for len(text) > 0 {
		n, err := io.WriteString(s.output, text)
		text = text[n:]
		if err != nil {
			s.failLocked(fmt.Errorf("terminal write: %w", err))
			return s.fault
		}
		if n == 0 {
			s.failLocked(fmt.Errorf("terminal write: %w", io.ErrShortWrite))
			return s.fault
		}
	}
	return nil
}

func (s *Screen) failLocked(err error) {
	s.fault = errors.Join(s.fault, err)
	if s.result != nil {
		s.finishLocked(answer{err: s.fault})
	}
}

// beginPaint starts one frame. Every output of an operation goes into paint
// and leaves in one write.
func (s *Screen) beginPaint() {
	s.paint.Reset()
	s.paint.WriteString(syncBegin)
	s.paintRows, s.paintMulti = 0, false
}

// flushPaintLocked writes the frame. Only a frame that changes several rows
// can show half drawn, so only such a frame pays for the synchronized-output
// markers; a spinner or preview update of one row does not.
func (s *Screen) flushPaintLocked() {
	if s.fault != nil || s.paint.Len() <= len(syncBegin) {
		s.paint.Reset()
		return
	}
	if s.paintMulti || s.paintRows > 1 {
		s.paint.WriteString(syncEnd)
	} else {
		s.paint.Next(len(syncBegin))
	}
	if _, err := s.paint.WriteTo(s.output); err != nil {
		s.failLocked(fmt.Errorf("terminal write: %w", err))
	}
	s.paint.Reset()
}

func (s *Screen) eraseIntoPaintLocked() {
	if len(s.live) == 0 {
		return
	}
	s.moveLocked(0, 0)
	s.paint.WriteString("\x1b[J")
	s.paintMulti = true
	clear(s.live)
	s.live = s.live[:0]
	s.cursorRow, s.cursorCol = 0, 0
}

// moveLocked uses rows relative to the live area's first line. New rows are
// created separately with line feeds so the transcript stays in scrollback.
func (s *Screen) moveLocked(row, col int) {
	if row < s.cursorRow {
		s.paint.WriteString("\x1b[")
		s.paint.WriteString(strconv.Itoa(s.cursorRow - row))
		s.paint.WriteByte('A')
	} else if row > s.cursorRow {
		s.paint.WriteString("\x1b[")
		s.paint.WriteString(strconv.Itoa(row - s.cursorRow))
		s.paint.WriteByte('B')
	}
	if col != s.cursorCol {
		s.paint.WriteByte('\r')
		if col > 0 {
			s.paint.WriteString("\x1b[")
			s.paint.WriteString(strconv.Itoa(col))
			s.paint.WriteByte('C')
		}
	}
	s.cursorRow, s.cursorCol = row, col
}

func (s *Screen) composerRuleLocked(width int) string {
	if s.rule.width != width || s.rule.text == "" {
		s.rule = cachedRule{width: width, text: s.theme.Rule(width)}
	}
	return s.rule.text
}

// boxWidth is the narrowest live area that draws the composer as a card.
const boxWidth = 12

// cardPaddingCells is the width of the margin inside a card, on each side.
const cardPaddingCells = 2

// boxed draws rows inside a rounded card of width cells. The title sits in
// the top border and foot in the bottom one. Rows must already fit width-4
// cells.
func (s *Screen) boxed(rows []string, width int, border func(string) string, title, foot string) []string {
	inner := width - 4
	out := make([]string, 0, len(rows)+2)
	out = append(out, s.borderRow("╭", "╮", title, inner, width, border))
	left, right := border("│")+" ", " "+border("│")
	for _, row := range rows {
		pad := max(inner-cellWidth(withoutStyles(row)), 0)
		out = append(out, left+row+strings.Repeat(" ", pad)+right)
	}
	return append(out, s.borderRow("╰", "╯", foot, inner, width, border))
}

// borderRow draws one horizontal card edge with an optional label after the
// first dash. A label that does not leave two dashes on each side is omitted.
func (s *Screen) borderRow(open, close, label string, inner, width int, border func(string) string) string {
	row := border(open + "─")
	used := 2
	if plain := withoutStyles(label); plain != "" && cellWidth(plain)+4 <= inner {
		row += " " + label + " "
		used += cellWidth(plain) + 2
	}
	return row + border(strings.Repeat("─", max(width-1-used, 0))+close)
}

func (s *Screen) inputWidthLocked() int {
	width := max(s.cols-1, 2)
	if s.box {
		return width - 6
	}
	if width >= 4 {
		width -= 2
	}
	return width
}

// moveVerticalLocked uses terminal cells, not rune counts. Keep the column
// through short rows so the next long row restores the intended position.
func (s *Screen) moveVerticalLocked(down bool) bool {
	width := s.inputWidthLocked()
	row, col := inputPoint(s.text, s.position, width)
	if !s.verticalMove {
		s.verticalColumn = col
	}
	target := row - 1
	if down {
		target = row + 1
	}
	if target < 0 {
		return false
	}
	best, found := s.position, false
	r, c := 0, 0
	for pos := 0; pos <= len(s.text); {
		pr, pc := r, c
		if pos < len(s.text) && s.text[pos] != '\n' &&
			c+max(runeWidth(s.text[pos]), 1) > width {
			pr, pc = r+1, 0
		}
		if pr > target {
			break
		}
		if pr == target {
			if !found || pc <= s.verticalColumn {
				best = pos
			}
			found = true
		}
		if pos == len(s.text) {
			break
		}
		end := nextCluster(s.text, pos)
		for _, ch := range s.text[pos:end] {
			r, c = inputCell(ch, width, r, c)
		}
		pos = end
	}
	if found {
		s.position = best
		s.verticalMove = true
	}
	return found
}

func (s *Screen) recallLocked(down bool) {
	next := s.historyPosition - 1
	if down {
		next = s.historyPosition + 1
	}
	if next < 0 || next > len(s.history) {
		return
	}
	if s.historyPosition == len(s.history) {
		s.draft, s.draftPosition = string(s.text), s.position
	}
	s.historyPosition = next
	text := s.draft
	if next < len(s.history) {
		text = s.history[next]
	}
	s.text, s.inputBytes = []rune(text), len(text)
	s.position = len(s.text)
	if next == len(s.history) {
		s.position = s.draftPosition
	}
	s.verticalMove = false
}

func (s *Screen) deleteLocked(start, end int) {
	for _, r := range s.text[start:end] {
		s.inputBytes -= utf8.RuneLen(r)
	}
	s.text = append(s.text[:start], s.text[end:]...)
	s.position = start
}

// layoutLocked builds the rows that change only with input, mode, preview,
// or size: opening banner, preview, composer rule, input or choice rows. The
// activity and footer rows get a fixed place and are filled at each draw.
// While an effect plays, every frame lays out again.
func (s *Screen) layoutLocked() {
	now := time.Now()
	clear(s.frame)
	s.frame = s.frame[:0]
	s.activityRow, s.footerRow = -1, -1
	width := max(s.cols-1, 2)
	s.inputTop, s.inputRows = 0, 0
	s.activityRows, s.footerRows = 0, 0
	// The opening banner takes the top rows while it plays, if the composer
	// still fits under it.
	rows := s.rows
	if intro := s.introFrameLocked(now); intro != nil && s.rows-len(intro) >= minimumLiveRows {
		s.frame = append(s.frame, intro...)
		rows -= len(intro)
	}
	marker := s.mode != modeChoice && width >= 4
	var bodyRows []string
	editRow, editCol, start := 0, 0, 0
	// The composer sits in a rounded bar when there is room for its two
	// borders and the status bar; otherwise a hairline or nothing separates
	// it. The bar decides the footer, so it comes first.
	box := width >= boxWidth && rows >= 4
	s.box = box
	s.soft = box && s.theme.Soft()
	footer := styledRows(s.footerLocked(), width, s.theme.Colored())
	minimumBody := 1
	if s.mode == modeChoice {
		minimumBody = min(rows, len(styledRows("y a n", width, false)))
	}
	maxFooter := max(0, min(3, rows-minimumBody))
	if maxFooter == 0 {
		footer = nil
	} else {
		footer = visibleRows(footer, maxFooter, width)
	}
	inner := s.inputWidthLocked()
	rule := !box && s.mode != modeChoice && rows >= len(footer)+2 && width >= boxWidth
	maxBody := max(1, rows-len(footer))
	if rule {
		maxBody--
	}
	if box {
		maxBody -= 2
	}
	padding := !box && s.mode == modePrompt && rows >= 10
	if padding {
		maxBody -= 2
	}
	maxBody = max(1, min(maxBody, min(6, max(2, rows/3))))
	switch s.mode {
	case modePrompt, modeBusy:
		body := string(s.text)
		if body == "" {
			body = s.placeholder
			if s.mode == modeBusy {
				body = busyPlaceholder(rows, inner)
			}
		}
		bodyRows = styledRows(body, inner, false)
		editRow, editCol = inputPoint(s.text, s.position, inner)
		start = max(0, editRow-maxBody+1)
		s.inputTop, s.inputRows = start, len(bodyRows)
		bodyRows = bodyRows[start:min(start+maxBody, len(bodyRows))]
		if len(bodyRows) == 1 && maxBody >= 2 {
			bodyRows = append(bodyRows, "")
		}
	case modeChoice:
		// Without a tint the request sits in an amber box. With one it hangs
		// on the page in its own frame, with nothing around it.
		choiceWidth, choiceRows := width, max(1, rows-len(footer))
		if box && !s.soft {
			choiceWidth, choiceRows = width-4, max(1, rows-len(footer)-2)
		}
		bodyRows = styledRows(s.theme.ApprovalChoices(choiceWidth), choiceWidth, s.theme.Colored())
		if len(bodyRows) > choiceRows {
			bodyRows = styledRows("y a n", choiceWidth, false)
		}
		if len(bodyRows) > choiceRows {
			bodyRows = styledRows("yan", choiceWidth, false)
		}
		if s.request != nil {
			// The frame takes a row of air above and below the work when
			// the screen has room for it.
			piece := s.theme.ApprovalRows(*s.request, choiceWidth, true)
			if len(piece)+1+len(bodyRows) > choiceRows {
				piece = s.theme.ApprovalRows(*s.request, choiceWidth, false)
			}
			if len(piece)+1+len(bodyRows) <= choiceRows {
				bodyRows = append(append(piece, ""), bodyRows...)
			}
		}
		if s.soft && len(bodyRows) < choiceRows {
			// Air under the choices keeps them apart from the status bar.
			bodyRows = append(bodyRows, "")
		}
		bodyRows = visibleRows(bodyRows, choiceRows, choiceWidth)
	case modeView:
		panic("terminal: document view reached composer layout")
	default:
		panic(fmt.Sprintf("terminal: unknown input mode %d", s.mode))
	}
	// framed is true when the live rows sit inside the composer's borders.
	framed := box && (s.mode != modeChoice || !s.soft)
	activity := s.mode == modeBusy && s.status != ""
	room := rows - len(bodyRows) - len(footer)
	if rule {
		room--
	}
	if framed {
		room -= 2
	}
	var activityLines []string
	if activity && room > 0 {
		activityLines = styledRows(s.activityLocked(), width, s.theme.Colored())
		activityLines = visibleRows(activityLines, max(1, room), width)
		room -= len(activityLines)
	}
	if padding {
		room -= 2
	}
	room -= min(menuRowLimit, len(s.menuLocked()))
	queueRows := s.queueRowsLocked(width, room)
	room -= len(queueRows)
	if preview := s.previewLocked(now); preview != "" && room > 0 {
		// The preview wraps like a committed line: at words, with later rows
		// under the text after the sigil, so a streamed paragraph does not
		// break inside words and then jump when it commits.
		var previewRows []string
		for line := range strings.SplitSeq(preview, "\n") {
			for index, row := range WrapWords(line, max(width-2, 1)) {
				if index > 0 {
					row = "  " + row
				}
				previewRows = append(previewRows, row)
			}
		}
		s.frame = append(s.frame, previewRows[max(0, len(previewRows)-room):]...)
	}
	if len(activityLines) > 0 {
		s.activityRow, s.activityRows = len(s.frame), len(activityLines)
		s.frame = append(s.frame, activityLines...)
	}
	s.frame = append(s.frame, queueRows...)
	if rule {
		s.frame = append(s.frame, s.composerRuleLocked(width))
	}
	if padding {
		s.frame = append(s.frame, "")
	}
	bodyStart := len(s.frame)
	if framed {
		bodyStart++
	}
	live := make([]string, 0, len(bodyRows))
	for i, row := range bodyRows {
		if len(s.text) == 0 && (s.mode == modeBusy || s.mode == modePrompt) {
			// An invitation, not an instruction: quiet italic gray.
			row = s.theme.Graphite(s.theme.Italic(row))
		}
		if marker {
			prefix := "  "
			if start+i == 0 {
				glyph := "›"
				if s.soft {
					glyph = "❯"
				}
				if s.mode == modeBusy {
					prefix = s.theme.Graphite(glyph) + " "
				} else {
					prefix = s.theme.Bold(s.theme.Accent(glyph)) + " "
				}
			}
			if i == 0 && start > 0 {
				prefix = s.theme.Graphite("↑") + " "
			} else if i == len(bodyRows)-1 && start+len(bodyRows) < s.inputRows {
				prefix = s.theme.Graphite("↓") + " "
			}
			row = prefix + row
		}
		live = append(live, row)
	}
	foot, fits := s.cardFootLocked()
	if !fits || s.soft {
		// With a tint the status bar under the composer holds the keys.
		foot = ""
	}
	switch {
	case !framed:
		s.frame = append(s.frame, live...)
	case s.mode == modeChoice:
		s.frame = append(s.frame, s.boxed(live, width, s.theme.Amber, s.theme.Amber("Approval needed"), s.theme.Graphite(foot))...)
	case s.soft:
		// The composer is a rounded box whose edge fades from plum to blue:
		// the one surface that takes input.
		s.frame = append(s.frame, s.theme.GradientBox(live, width)...)
	default:
		s.frame = append(s.frame, s.boxed(live, width, s.theme.Hairline, "", s.theme.Graphite(foot))...)
	}
	if padding {
		s.frame = append(s.frame, "")
	}
	if menu := s.menuLocked(); len(menu) > 0 {
		s.frame = append(s.frame, s.menuRowsLocked(menu, width, min(menuRowLimit, s.rows-len(s.frame)-len(footer)))...)
	}
	if len(s.frame) < s.rows {
		footer = visibleRows(footer, s.rows-len(s.frame), width)
		s.footerRow, s.footerRows = len(s.frame), len(footer)
		s.frame = append(s.frame, footer...)
	}
	s.frameRow, s.frameCol = len(s.frame)-1, 0
	if s.mode == modePrompt || s.mode == modeBusy {
		s.frameRow = bodyStart + editRow - start
		s.frameCol = editCol
		if marker {
			s.frameCol += 2
		}
		if box {
			s.frameCol += 2
		}
		s.frameCol = min(s.frameCol, s.cols-1)
	}
	s.layoutDirty = false
}

// visibleRows marks text that cannot fit in the available terminal rows.
func visibleRows(rows []string, limit, width int) []string {
	limit = max(limit, 1)
	if len(rows) <= limit {
		return rows
	}
	rows = rows[:limit]
	last := styledRows(withoutStyles(rows[limit-1]), max(1, width-1), false)[0]
	rows[limit-1] = last + "…"
	return rows
}

// spinnerFrames is the activity mark while the agent works.
var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// activityLocked keeps the status ahead of the activity mark: a spinner in
// the model color while the agent works, amber while a tool runs. With a tint
// a hairline carries the row, so that working looks like part of the frame
// that holds the composer, and not a line of text above it.
func (s *Screen) activityLocked() string {
	now := time.Now()
	if s.status == "" {
		return s.theme.Graphite("·")
	}
	mark := "·"
	if s.effects.motion {
		mark = spinnerFrames[(s.tickLocked(now)/2)%len(spinnerFrames)]
	}
	if s.effects.toolRunning {
		mark = s.theme.Amber(mark)
	} else {
		mark = s.theme.Accent(mark)
	}
	row := mark + "  " + s.theme.Ink(s.status)
	if !s.activitySince.IsZero() {
		row += s.theme.Graphite("   " + strconv.Itoa(int(now.Sub(s.activitySince).Seconds())) + "s")
	}
	if !s.theme.Soft() {
		return row
	}
	used := 2 + cellWidth(withoutStyles(row)) + 3
	room := max(max(s.cols-1, 2)-used, 0)
	trace := s.traceLocked(now, room)
	if trace == "" {
		return "  " + row
	}
	// The trace sits at the right end of the row, as the display of an
	// instrument, so it keeps its place while the label changes.
	return "  " + row + strings.Repeat(" ", 3+room-traceColumns) + trace
}

// traceColumns is the width of the trace: about five seconds of the stream,
// small enough to sit beside a long label on a terminal of 100 columns.
const traceColumns = 48

// markLocked records count events of a kind on the trace. Without motion,
// without intermediate colors, or before the first frame it does nothing.
func (s *Screen) markLocked(kind presentation.TraceMark, count int) {
	if s.effects.trace != nil {
		s.effects.trace.Mark(kind, count)
	}
}

// traceLocked moves the trace to now and draws it, or returns "" when the
// row has no room for it or the screen draws no motion. The trace is made on
// the first frame that has room, so a narrow terminal never holds one.
func (s *Screen) traceLocked(now time.Time, room int) string {
	if !s.effects.motion || !s.theme.Blends() || room < traceColumns+8 {
		return ""
	}
	if s.effects.trace == nil {
		s.effects.trace, s.effects.traceAt = presentation.NewTrace(traceColumns), now
	}
	s.effects.trace.Step(now.Sub(s.effects.traceAt))
	s.effects.traceAt = now
	return s.effects.trace.Draw(s.theme)
}

// hintLocked names the keys of the current mode in the widest form that fits
// room cells. The safety key of each mode survives down to a few cells.
func (s *Screen) hintLocked(room int) string {
	hint := ""
	switch s.mode {
	case modePrompt:
		if len(s.menuLocked()) > 0 {
			hint = "↑↓ select · Tab complete · Enter run · Esc close"
			if room < cellWidth(hint) {
				hint = "↑↓ Tab Enter · Esc close"
			}
			if room < cellWidth(hint) {
				hint = "Tab · Esc"
			}
			return hint
		}
		hint = "Enter send · Ctrl-J new line"
		if room < cellWidth(hint) {
			hint = "Enter send · ^J line"
		}
		if room < cellWidth(hint) {
			hint = "Enter send / ^J"
		}
		if room < cellWidth(hint) {
			hint = "Enter / ^J"
		}
		if room < cellWidth(hint) {
			hint = "Enter"
		}
		if commands := " · /tools · /diff · /help"; cellWidth(hint)+cellWidth(commands) <= room {
			hint += commands
		}
	case modeChoice:
		hint = "Enter / Esc deny"
		if room < cellWidth(hint) {
			hint = "Enter/Esc deny"
		}
		if room < cellWidth(hint) {
			hint = "^M/Esc n"
		}
	case modeBusy:
		hint = busyHint(room, len(s.queue) > 0)
	case modeView:
		panic("terminal: document view reached composer hints")
	}
	return hint
}

// cardFootLocked is the key hint of the card's bottom border and whether the
// border has room for it; otherwise the status bar shows it.
func (s *Screen) cardFootLocked() (string, bool) {
	inner := s.inputWidthLocked()
	hint := s.hintLocked(inner - 4)
	return hint, cellWidth(hint)+4 <= inner
}

// footerLocked is the status bar under the composer. Inside a card the keys
// sit in the card's bottom border, so the bar holds only the session details,
// ending under the card's right edge. Without a card the keys come first.
func (s *Screen) footerLocked() string {
	now := time.Now()
	width := max(s.cols-1, 2)
	extra := s.footerTextLocked(now)
	if s.mode == modePrompt && s.background != "" {
		if extra != "" {
			extra += s.theme.Graphite(" · ")
		}
		extra += s.theme.Graphite(s.background)
	}
	if s.box && s.soft {
		if s.rows >= softFooterRows {
			return s.softFooterLocked(now, width)
		}
		hint := s.hintLocked(width - cardPaddingCells)
		lead := strings.Repeat(" ", cardPaddingCells) + s.theme.Graphite(hint)
		if extra == "" {
			return lead
		}
		if gap := width - cardPaddingCells - cellWidth(hint) - cellWidth(withoutStyles(extra)); gap >= 3 {
			return lead + strings.Repeat(" ", gap) + extra
		}
		return lead + "\n" + strings.Repeat(" ", max(width-cellWidth(withoutStyles(extra)), 0)) + extra
	}
	if s.box {
		foot, fits := s.cardFootLocked()
		if fits {
			if gap := width - cellWidth(withoutStyles(extra)); extra != "" && gap >= 2 {
				return strings.Repeat(" ", gap) + extra
			}
			return extra
		}
		line := s.theme.Graphite(foot)
		if extra != "" {
			line += "\n" + extra
		}
		return line
	}
	hint := s.hintLocked(width)
	line := s.theme.Graphite(hint)
	if extra != "" {
		if cellWidth(withoutStyles(extra))+cellWidth(hint)+3 <= width {
			line += "   " + extra
		} else {
			line += "\n" + extra
		}
	}
	return line
}

// softFooterRows is the least terminal height that has room for the two-row
// footer of a soft card; a shorter terminal keeps one row.
const softFooterRows = 16

// softFooterLocked is the status area under a soft card, in the margin of the
// card's text. The first row says where the work happens, the folder and the
// branch, and which model does it. The second holds the keys and the context
// use. A row that cannot hold both of its sides gives up the keys, or the
// branch, before the model or the context.
func (s *Screen) softFooterLocked(now time.Time, width int) string {
	inner := width - 2*cardPaddingCells
	margin := strings.Repeat(" ", cardPaddingCells)
	model, usage := s.footerPartsLocked(now)
	if s.mode == modePrompt && s.background != "" {
		if usage != "" {
			usage += s.theme.Graphite(" · ")
		}
		usage += s.theme.Graphite(s.background)
	}
	var rows []string
	if place := s.placeLocked(inner - cellWidth(withoutStyles(model)) - 3); place != "" || model != "" {
		row, _ := spread(place, model, inner)
		rows = append(rows, margin+row)
	}
	hint := s.hintLocked(inner - cellWidth(withoutStyles(usage)) - 3)
	if row, fits := spread(s.theme.Graphite(hint), usage, inner); fits {
		rows = append(rows, margin+row)
	} else {
		// The keys are the safety of the mode, so they stay; the usage moves down.
		rows = append(rows, margin+s.theme.Graphite(s.hintLocked(inner)))
		row, _ := spread("", usage, inner)
		rows = append(rows, margin+row)
	}
	return strings.Join(rows, "\n")
}

// spread puts left at the start of a row of inner cells and right at its end.
// It reports whether both fit with at least three cells between them; when
// either side is empty the other takes its place and it always fits.
func spread(left, right string, inner int) (string, bool) {
	leftWidth, rightWidth := cellWidth(withoutStyles(left)), cellWidth(withoutStyles(right))
	switch {
	case right == "":
		return left, true
	case left == "":
		return strings.Repeat(" ", max(inner-rightWidth, 0)) + right, true
	}
	gap := inner - leftWidth - rightWidth
	if gap < 3 {
		return "", false
	}
	return left + strings.Repeat(" ", gap) + right, true
}

// SetPlace names the folder and the branch that the footer shows. The folder
// is text that fits a path, such as "~/code/harbor"; an empty branch shows no
// branch.
func (s *Screen) SetPlace(folder, branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if folder == s.place.folder && branch == s.place.branch {
		return
	}
	s.place.folder, s.place.branch = folder, branch
	s.layoutDirty = true
	s.drawLocked()
}

// placeLocked is the folder and branch in room cells, or the empty string when
// there is no folder or the room is too small to say anything. The branch goes
// first when room is short, and then the front of the folder.
func (s *Screen) placeLocked(room int) string {
	folder, branch := presentation.Safe(s.place.folder), presentation.Safe(s.place.branch)
	if folder == "" || room < 6 {
		return ""
	}
	if branch != "" && cellWidth(folder)+2+cellWidth(branch) <= room {
		return s.theme.Graphite(folder) + "  " + s.theme.Accent(branch)
	}
	return s.theme.Graphite(presentation.EllipsizeStart(folder, room))
}

func (s *Screen) drawLocked() {
	if s.closed || s.fault != nil {
		return
	}
	if s.view == nil && s.introDoneLocked(time.Now()) {
		s.commitIntroLocked()
		return
	}
	s.beginPaint()
	s.renderLocked()
	s.flushPaintLocked()
}

// renderLocked appends the changes from the live rows to the frame to paint.
// A render that lays out shows every effect at the current time, so it
// counts as an animation frame: streamed tokens then replace frames instead
// of adding to them.
func (s *Screen) renderLocked() {
	if s.view != nil {
		if s.layoutDirty || len(s.frame) == 0 {
			s.layoutViewLocked()
		}
	} else {
		if s.reasoningDirty {
			s.preview = "  " + s.theme.Graphite(s.theme.Italic(presentation.Safe(s.reasoningText)))
			s.reasoningText, s.reasoningDirty = "", false
			s.effects.previewPrefix, s.effects.previewBody = "", ""
			s.effects.wet = s.effects.wet[:0]
			s.layoutDirty = true
		}
		if s.layoutDirty || len(s.frame) == 0 {
			s.layoutLocked()
			s.lastFrame = time.Now()
		}
		if s.activityRow >= 0 {
			rows := visibleRows(styledRows(s.activityLocked(), max(s.cols-1, 2), s.theme.Colored()), s.activityRows, max(s.cols-1, 2))
			clear(s.frame[s.activityRow : s.activityRow+s.activityRows])
			copy(s.frame[s.activityRow:s.activityRow+s.activityRows], rows)
		}
		if s.footerRow >= 0 {
			rows := visibleRows(styledRows(s.footerLocked(), max(s.cols-1, 2), s.theme.Colored()), s.footerRows, max(s.cols-1, 2))
			clear(s.frame[s.footerRow : s.footerRow+s.footerRows])
			copy(s.frame[s.footerRow:s.footerRow+s.footerRows], rows)
		}
	}
	visible := s.mode == modePrompt || s.mode == modeBusy
	dirty := len(s.frame) != len(s.live)
	for i, row := range s.frame {
		if i >= len(s.live) || row != s.live[i] {
			dirty = true
			break
		}
	}
	if dirty && s.cursorVisible {
		s.paint.WriteString("\x1b[?25l")
		s.cursorVisible = false
	}
	for i, row := range s.frame {
		if i < len(s.live) && row == s.live[i] {
			continue
		}
		if i > 0 && i >= len(s.live) {
			s.moveLocked(i-1, 0)
			s.paint.WriteString("\r\n")
			s.cursorRow = i
		} else {
			s.moveLocked(i, 0)
		}
		s.paint.WriteString("\x1b[2K")
		s.paintRows++
		s.paint.WriteString(row)
		if s.theme.Colored() {
			s.paint.WriteString("\x1b[0m")
		}
		// A row may contain SGR sequences. Return directly to column zero
		// instead of measuring it again just to move the cursor.
		s.paint.WriteByte('\r')
		s.cursorCol = 0
	}
	if len(s.frame) < len(s.live) {
		s.moveLocked(len(s.frame), 0)
		s.paint.WriteString("\x1b[J")
		s.paintMulti = true
	}
	s.moveLocked(s.frameRow, s.frameCol)
	if visible != s.cursorVisible {
		if visible {
			s.paint.WriteString("\x1b[?25h")
		} else {
			s.paint.WriteString("\x1b[?25l")
		}
		s.cursorVisible = visible
	}
	if dirty {
		clear(s.live)
		s.live = append(s.live[:0], s.frame...)
	}
}

// Write adds complete lines to the scrollback above the live rows. The erase,
// the lines, and the redraw leave in one synchronized write.
func (s *Screen) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	if s.fault != nil {
		return 0, s.fault
	}
	// Output ends the opening banner first, so the transcript keeps the order
	// of the calls.
	s.commitIntroLocked()
	s.writeLocked(data)
	return len(data), s.fault
}

func (s *Screen) writeLocked(data []byte) {
	s.pending.Write(data)
	if s.view != nil {
		return
	}
	if end := bytes.LastIndexByte(s.pending.Bytes(), '\n'); end >= 0 {
		s.beginPaint()
		s.eraseIntoPaintLocked()
		text := string(s.pending.Next(end + 1))
		if !s.theme.Colored() {
			text = withoutStyles(text)
		}
		s.paint.WriteString(strings.ReplaceAll(text, "\n", "\r\n"))
		s.paintMulti = true
		s.renderLocked()
		s.flushPaintLocked()
	}
}

// Width is the terminal width in cells.
func (s *Screen) Width() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols
}

// Height is the terminal height in rows.
func (s *Screen) Height() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

// oscText makes text safe inside an operating system command: no controls,
// one line, and a bounded length.
func oscText(text string) string {
	text = strings.Join(strings.Fields(presentation.Safe(text)), " ")
	if utf8.RuneCountInString(text) > 200 {
		runes := []rune(text)
		text = string(runes[:199]) + "…"
	}
	return text
}

// SetTitle sets the window title. Tab bars, such as the one in cmux, show it,
// so the state of the agent is visible from another tab. Close restores the
// title that was there before New.
func (s *Screen) SetTitle(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text = oscText(text)
	if s.closed || text == s.title {
		return
	}
	s.title = text
	s.beginPaint()
	s.paint.WriteString("\x1b]2;" + text + "\x07")
	s.flushPaintLocked()
}

// Notify sends a desktop notification with OSC 9. Terminals such as iTerm2,
// Ghostty, WezTerm, kitty, and cmux show it; cmux also rings the pane. Other
// terminals ignore the sequence.
func (s *Screen) Notify(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text = oscText(text)
	if s.closed || text == "" {
		return
	}
	s.beginPaint()
	s.paint.WriteString("\x1b]9;" + text + "\x07")
	s.flushPaintLocked()
}

// SetStatus shows current work. Empty text removes the activity row.
func (s *Screen) SetStatus(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStatusLocked(text, false)
}

// SetToolStatus shows a running tool with the model accent.
func (s *Screen) SetToolStatus(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStatusLocked(text, true)
}

func (s *Screen) setStatusLocked(text string, tool bool) {
	if text == s.status && tool == s.effects.toolRunning {
		return
	}
	now := time.Now()
	if text != s.status {
		s.activitySince = now
		if tool {
			s.markLocked(presentation.TraceTool, 1)
		}
	}
	s.effects.toolRunning = tool
	s.status = text
	s.layoutDirty = true
	s.drawLocked()
	s.wake.signal()
}

// Mark records count events of a kind on the trace of the working row, for
// an event that the screen does not see itself, such as reasoning that is
// not shown or a failed tool. Without a trace it does nothing.
func (s *Screen) Mark(kind presentation.TraceMark, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markLocked(kind, count)
}

// SetBackgroundWork shows work that continues while the user types.
// Empty text removes it from the footer.
func (s *Screen) SetBackgroundWork(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == s.background {
		return
	}
	s.background = text
	s.layoutDirty = true
	s.drawLocked()
	s.wake.signal()
}

// ClearPreview removes visible and pending previews before text is committed.
func (s *Screen) ClearPreview() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasoningText, s.reasoningDirty = "", false
	if s.preview == "" && s.effects.previewBody == "" {
		return
	}
	s.preview = ""
	s.effects.previewPrefix, s.effects.previewBody = "", ""
	s.effects.wet = s.effects.wet[:0]
	s.layoutDirty = true
	s.drawLocked()
}

func (s *Screen) finishLocked(value answer) {
	result := s.result
	s.result = nil
	s.mode = modeBusy
	// The draft stays: it belongs to the user, not to the prompt that ended.
	s.placeholder = ""
	s.layoutDirty = true
	if s.view != nil {
		s.leaveViewLocked()
		if !s.closed && s.fault == nil {
			s.writeLocked(nil)
		}
	}
	if result != nil {
		if s.fault != nil {
			value.err = errors.Join(value.err, s.fault)
		}
		result <- value
	}
	s.wake.signal()
}

// Drain before a choice or a view takes the keys. The reader uses the same
// lock, so no byte read before this boundary can answer the new owner. The
// bytes were typed for the draft, so they edit it.
func (s *Screen) drainLocked() error {
	var data [4096]byte
	for total := 0; total < maxInputBytes; {
		ready, err := inputReady(int(s.input.Fd()), 0)
		if err != nil {
			return fmt.Errorf("terminal input drain: %w", err)
		}
		if !ready {
			// Retain paste state: a paste begun while busy must not approve a
			// choice even if its tail arrives after the choice is drawn.
			s.parser.stalePending = len(s.parser.pending) > 0
			return nil
		}
		n, err := syscall.Read(int(s.input.Fd()), data[:])
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("terminal input drain: %w", err)
		}
		if n == 0 {
			return io.EOF
		}
		total += n
		for _, k := range s.parser.feed(data[:n], time.Now()) {
			if k.kind == keyInterrupt && !k.paste {
				go s.interrupted(nil)
				return context.Canceled
			}
			s.keyLocked(k)
		}
	}
	return errors.New("terminal queued input exceeds 1 MiB")
}

func (s *Screen) await(ctx context.Context, mode inputMode, placeholder string, view *documentView) (answer, error) {
	if err := ctx.Err(); err != nil {
		return answer{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return answer{}, os.ErrClosed
	}
	if s.fault != nil {
		err := s.fault
		s.mu.Unlock()
		return answer{}, err
	}
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		return answer{}, err
	}
	if s.result != nil {
		s.mu.Unlock()
		return answer{}, errors.New("terminal already has an active prompt")
	}
	// A prompt edits the same draft as busy mode, so the read loop gives it
	// the waiting bytes as typed input; only a new owner of other keys drains.
	if mode != modePrompt {
		if err := s.drainLocked(); err != nil {
			if !errors.Is(err, context.Canceled) {
				s.failLocked(err)
			}
			s.mu.Unlock()
			return answer{}, err
		}
	}
	if view != nil {
		s.commitIntroLocked()
		if s.fault != nil {
			err := s.fault
			s.mu.Unlock()
			return answer{}, err
		}
	}
	result := make(chan answer, 1)
	s.result, s.mode, s.placeholder = result, mode, placeholder
	s.menuIndex, s.menuDismissed = 0, false
	s.verticalColumn, s.verticalMove = 0, false
	s.layoutDirty = true
	if view != nil {
		s.enterViewLocked(view)
	}
	s.drawLocked()
	if mode == modeChoice {
		s.choiceSince = time.Now()
	}
	// The new mode may animate, such as the breathing approval bar; the read
	// loop must leave a wait that it began without a deadline.
	s.wake.signal()
	s.mu.Unlock()
	select {
	case value := <-result:
		return value, value.err
	case <-ctx.Done():
		s.mu.Lock()
		if s.result == result {
			s.finishLocked(answer{err: ctx.Err()})
			s.drawLocked()
		}
		s.mu.Unlock()
		return answer{}, ctx.Err()
	case <-s.ctx.Done():
		s.mu.Lock()
		if s.result == result {
			s.finishLocked(answer{err: s.ctx.Err()})
			s.drawLocked()
		}
		s.mu.Unlock()
		return answer{}, s.ctx.Err()
	}
}

func (s *Screen) Prompt(ctx context.Context, placeholder string) (string, error) {
	value, err := s.await(ctx, modePrompt, placeholder, nil)
	return value.text, err
}

func (s *Screen) Choose(ctx context.Context) (Choice, error) {
	value, err := s.await(ctx, modeChoice, "", nil)
	return value.choice, err
}

// RequestFits reports whether ChooseRequest can show request above the
// choices at the current terminal size, so the request leaves no trace in
// the scrollback once it is answered.
func (s *Screen) RequestFits(request presentation.ApprovalRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	width := max(s.cols-1, 2)
	if width < 52 {
		return false
	}
	// A gap, the choice row, and up to three footer rows. Without a tint the
	// request also sits inside a box: two borders, four cells narrower.
	reserved, inner := 5, width
	if !s.theme.Soft() {
		reserved, inner = 7, width-4
	}
	return len(s.theme.ApprovalRows(request, inner, false))+reserved <= s.rows
}

// ChooseRequest asks for an approval with request shown above the choices.
// The caller checks RequestFits first; a request that stops fitting after a
// resize is left out and the choices remain.
func (s *Screen) ChooseRequest(ctx context.Context, request presentation.ApprovalRequest) (Choice, error) {
	s.mu.Lock()
	s.request = &request
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.request = nil
		s.layoutDirty = true
		s.mu.Unlock()
	}()
	return s.Choose(ctx)
}

// ArmTyping returns a channel that closes at the next prompt key other than
// Enter, including a paste. Arming again replaces the earlier channel, which
// then never closes. The chat uses it to start work only when the user begins
// to type.
func (s *Screen) ArmTyping() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	typing := make(chan struct{})
	s.typing = typing
	return typing
}

func (s *Screen) insertLocked(text string) {
	if s.inputBytes+len(text) > maxInputBytes {
		// While the agent works no prompt can fail, so the editor keeps its
		// text and refuses the key.
		if s.mode == modePrompt {
			s.text, s.position, s.inputBytes = nil, 0, 0
			s.finishLocked(answer{err: errors.New("terminal input exceeds 1 MiB")})
		}
		return
	}
	insert := []rune(text)
	oldLen := len(s.text)
	s.text = append(s.text, insert...)
	copy(s.text[s.position+len(insert):], s.text[s.position:oldLen])
	copy(s.text[s.position:], insert)
	s.position += len(insert)
	s.verticalMove = false
	s.inputBytes += len(text)
	s.layoutDirty = true
}

func (s *Screen) keyLocked(k key) bool {
	if k.kind == keyInterrupt && !k.paste {
		if s.view != nil {
			s.finishLocked(answer{})
			return false
		}
		s.mode = modeBusy
		s.layoutDirty = true
		return true
	}
	if k.stale {
		return false
	}
	if s.view != nil {
		s.viewKeyLocked(k)
		return false
	}
	switch s.mode {
	case modeChoice:
		if time.Since(s.choiceSince) >= choiceGrace {
			s.choiceKeyLocked(k)
			return false
		}
		// The approval appeared while the user typed, so the key edits the
		// draft below it.
	case modePrompt:
		if k.kind == keyAltEnter {
			k.kind = keyEnter
		}
		if s.typing != nil && k.kind != keyEnter {
			close(s.typing)
			s.typing = nil
		}
	case modeBusy:
	case modeView:
		panic("terminal: document view mode without a view")
	}
	s.editKeyLocked(k)
	return false
}

func (s *Screen) choiceKeyLocked(k key) {
	if k.paste {
		return
	}
	choice := Deny
	switch {
	case k.kind == keyEnter || k.kind == keyEscape:
	case k.kind == keyText && (k.text == "n" || k.text == "N"):
	case k.kind == keyText && (k.text == "y" || k.text == "Y"):
		choice = AllowOnce
	case k.kind == keyText && (k.text == "a" || k.text == "A"):
		choice = AllowSession
	default:
		return
	}
	s.finishLocked(answer{choice: choice})
}

// editKeyLocked applies a key to the draft. A prompt and busy mode share the
// editor; only Enter and Alt+Enter differ, in sendLocked.
func (s *Screen) editKeyLocked(k key) {
	s.layoutDirty = true
	if k.kind != keyUp && k.kind != keyDown {
		s.verticalMove = false
	}
	before := string(s.text)
	if matches := s.menuLocked(); len(matches) > 0 && s.menuKeyLocked(k, matches) {
		if string(s.text) != before {
			s.menuDismissed = false
		}
		return
	}
	defer func() {
		if string(s.text) != before {
			s.menuIndex, s.menuDismissed = 0, false
		}
	}()
	switch k.kind {
	case keyText:
		s.insertLocked(k.text)
	case keyNewline:
		s.insertLocked("\n")
	case keyEnter:
		s.sendLocked(false)
	case keyAltEnter:
		s.sendLocked(true)
	case keyAltUp:
		s.restoreQueueLocked()
	case keyLeft:
		s.position = previousCluster(s.text, s.position)
	case keyRight:
		s.position = nextCluster(s.text, s.position)
	case keyWordLeft:
		s.position = previousWord(s.text, s.position)
	case keyWordRight:
		s.position = nextWord(s.text, s.position)
	case keyWordBackspace:
		s.deleteLocked(previousWord(s.text, s.position), s.position)
	case keyWordDelete:
		s.deleteLocked(s.position, nextWord(s.text, s.position))
	case keyHome:
		for s.position > 0 && s.text[s.position-1] != '\n' {
			s.position--
		}
	case keyEnd:
		for s.position < len(s.text) && s.text[s.position] != '\n' {
			s.position++
		}
	case keyBackspace:
		s.deleteLocked(previousCluster(s.text, s.position), s.position)
	case keyDelete:
		s.deleteLocked(s.position, nextCluster(s.text, s.position))
	case keyClear:
		s.text = nil
		s.position = 0
		s.inputBytes = 0
	case keyHistoryPrevious, keyHistoryNext:
		s.recallLocked(k.kind == keyHistoryNext)
	case keyUp, keyDown:
		if !s.moveVerticalLocked(k.kind == keyDown) {
			s.recallLocked(k.kind == keyDown)
		}
	case keyEscape, keyInterrupt, keyPageUp, keyPageDown:
	default:
		panic(fmt.Sprintf("terminal: unknown key kind %d", k.kind))
	}
}

func (s *Screen) resizeLocked(rows, cols int) {
	if rows == s.rows && cols == s.cols {
		return
	}
	s.verticalMove = false
	if s.view != nil {
		s.rows, s.cols = rows, cols
		s.beginPaint()
		s.paint.WriteString("\x1b[H\x1b[2J")
		s.paintMulti = true
		clear(s.live)
		s.live = s.live[:0]
		s.cursorRow, s.cursorCol = 0, 0
		s.layoutDirty = true
		s.renderLocked()
		s.flushPaintLocked()
		return
	}
	s.reflowCursorLocked(rows, cols)
	s.beginPaint()
	s.eraseIntoPaintLocked()
	s.rows, s.cols = rows, cols
	s.layoutDirty = true
	if !s.closed && s.fault == nil {
		s.renderLocked()
	}
	s.flushPaintLocked()
}

func (s *Screen) reflowCursorLocked(rows, cols int) {
	// Explicit live rows can wrap again when the terminal narrows. Their
	// previous row coordinates are no longer suitable for a dirty-row diff.
	up := 0
	for i := range min(s.cursorRow, len(s.live)) {
		up += len(styledRows(s.live[i], cols, false))
	}
	up += s.cursorCol / cols
	s.cursorRow = min(up, rows-1)
	s.cursorCol %= cols
}

// waitLocked returns how long the read loop may block: until the next
// frame at interval, until a lone Escape becomes a key, or without
// a limit. A negative duration means no limit; the wake pipe ends the wait
// early.
func (s *Screen) waitLocked(now time.Time, interval time.Duration) time.Duration {
	timeout := time.Duration(-1)
	if interval > 0 {
		timeout = max(0, interval-now.Sub(s.lastFrame))
	}
	if deadline, pending := s.parser.escapeDeadline(); pending {
		if wait := max(0, deadline.Sub(now)); timeout < 0 || wait < timeout {
			timeout = wait
		}
	}
	return timeout
}

func (s *Screen) readLoop() {
	defer close(s.done)
	var data [4096]byte
	input := int(s.input.Fd())
	for {
		s.mu.Lock()
		closed := s.closed
		// The interval is fixed before the wait, so the frame after an effect
		// ends still comes and removes it.
		now := time.Now()
		interval := s.frameIntervalLocked(now)
		timeout := s.waitLocked(now, interval)
		s.mu.Unlock()
		if closed {
			return
		}
		if s.ctx.Err() != nil {
			s.mu.Lock()
			if s.result != nil {
				s.finishLocked(answer{err: s.ctx.Err()})
			}
			s.mu.Unlock()
			return
		}
		ready, woken, err := waitReadable(input, s.wake.read, timeout)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if woken {
			s.wake.drain()
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if err != nil {
			s.failLocked(fmt.Errorf("terminal read: %w", err))
			s.mu.Unlock()
			return
		}
		var keys []key
		if ready {
			// A prompt may have drained input while select was waiting.
			ready, err = inputReady(input, 0)
			if err == nil && ready {
				var n int
				n, err = syscall.Read(input, data[:])
				if err == nil && n == 0 {
					err = io.EOF
				}
				if err == nil {
					keys = s.parser.feed(data[:n], time.Now())
				}
			}
			if err != nil && !errors.Is(err, syscall.EINTR) {
				s.failLocked(fmt.Errorf("terminal read: %w", err))
				s.mu.Unlock()
				return
			}
		} else {
			keys = s.parser.expire(time.Now())
		}
		interrupt := false
		interruptedResult := s.result
		for _, k := range keys {
			if s.keyLocked(k) {
				interrupt = true
			}
		}
		// A due frame lays out again; the render records its time.
		frame := interval > 0 && time.Since(s.lastFrame) >= interval
		if frame {
			s.layoutDirty = true
		}
		if len(keys) > 0 || frame {
			s.drawLocked()
		}
		s.mu.Unlock()
		if interrupt {
			go s.interrupted(interruptedResult)
		}
	}
}

func (s *Screen) interrupted(result chan answer) {
	if s.onInterrupt != nil {
		s.onInterrupt()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if result != nil && s.result == result {
		s.finishLocked(answer{err: context.Canceled})
		s.drawLocked()
	}
}

func (s *Screen) Close() error {
	s.mu.Lock()
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	// A banner that still plays goes to scrollback, where Write would have
	// put it.
	s.commitIntroLocked()
	s.closed = true
	close(s.stop)
	signal.Stop(s.resizes)
	if s.result != nil {
		s.finishLocked(answer{err: os.ErrClosed})
	}
	s.paint.Reset()
	s.eraseIntoPaintLocked()
	// Attempt cleanup even after an earlier write fault. The saved terminal
	// state must be restored whether cursor cleanup succeeds or not.
	var cleanup strings.Builder
	cleanup.Write(s.paint.Bytes())
	s.paint.Reset()
	if s.pending.Len() > 0 {
		text := s.pending.String()
		if !s.theme.Colored() {
			text = withoutStyles(text)
		}
		cleanup.WriteString(strings.ReplaceAll(text, "\n", "\r\n"))
		s.pending.Reset()
	}
	if s.theme.Colored() {
		cleanup.WriteString("\x1b[0m")
	}
	// Paste mode off, cursor shown in the user's own shape, title restored.
	cleanup.WriteString("\x1b[?2004l\x1b[?25h\x1b[0 q\x1b[23;0t")
	n, outputErr := io.WriteString(s.output, cleanup.String())
	if outputErr == nil && n != cleanup.Len() {
		outputErr = io.ErrShortWrite
	}
	restoreErr := setState(s.input, &s.state)
	s.closeErr = errors.Join(s.fault, outputErr, restoreErr)
	err := s.closeErr
	s.wake.signal()
	s.mu.Unlock()
	<-s.done
	s.wake.close()
	return err
}
