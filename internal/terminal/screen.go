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
)

type answer struct {
	text   string
	choice Choice
	err    error
}

// Screen owns one terminal input reader and serializes every output operation.
// Call Close before using either descriptor outside the screen again.
type Screen struct {
	mu            sync.Mutex
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
	background               string
	title                    string
	rule                     cachedRule
	activitySince, lastFrame time.Time
	effects                  effects
	mode                     inputMode
	result                   chan answer
	placeholder              string
	text                     []rune
	position, inputBytes     int
	history                  []string
	historyPosition          int
	draft                    string
	parser                   parser
	// typing closes at the next prompt key other than Enter; see ArmTyping.
	typing chan struct{}
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
	shade, typed := presentation.DarkBackground, []byte(nil)
	if style.Depth != presentation.NoColor {
		shade, typed, err = queryShade(input, output, wake.read)
		if err != nil {
			wake.close()
			restoreErr := setState(input, &state)
			return nil, errors.Join(err, restoreErr)
		}
	}
	s := &Screen{
		input: input, output: output, wake: wake, state: state, onInterrupt: onInterrupt,
		ctx: ctx, stop: make(chan struct{}), done: make(chan struct{}),
		resizes: make(chan os.Signal, 1), rows: rows, cols: cols, layoutDirty: true,
		theme: presentation.NewTheme(style.Depth, shade, style.Accent), activityRow: -1, footerRow: -1,
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
	// The opening banner takes the top rows while it plays, if the composer
	// still fits under it.
	rows := s.rows
	if intro := s.introFrameLocked(now); intro != nil && s.rows-len(intro) >= minimumLiveRows {
		s.frame = append(s.frame, intro...)
		rows -= len(intro)
	}
	rule := s.mode == modePrompt && rows >= 3 && width >= 12
	marker := s.mode == modePrompt && width >= 4
	inner := width
	if marker {
		inner -= 2
	}
	var bodyRows []string
	var choiceBar string
	editRow, editCol, start := 0, 0, 0
	maxBody := rows
	if rule {
		maxBody--
	}
	// Keep one footer row unless it would displace the only input row.
	if maxBody > 1 {
		maxBody--
	}
	switch s.mode {
	case modePrompt:
		body := string(s.text)
		if body == "" {
			body = s.placeholder
		}
		bodyRows = styledRows(body, inner, false)
		prefixRows := styledRows(string(s.text[:s.position]), inner, false)
		editRow = len(prefixRows) - 1
		editCol = cellWidth(prefixRows[editRow])
		if s.position < len(s.text) && s.text[s.position] != '\n' &&
			editCol+runeWidth(s.text[s.position]) > inner {
			editRow++
			editCol = 0
		}
		start = max(0, editRow-maxBody+1)
		bodyRows = bodyRows[start:min(start+maxBody, len(bodyRows))]
	case modeChoice:
		// The bar continues the approval card above and breathes while it
		// waits. Shorten labels, then drop their styles, before hiding an
		// option.
		choiceBar = s.theme.Glow("▌", s.breathLocked(now)) + " "
		key := s.theme.Key
		for _, labels := range [...]string{
			key("y") + " allow once   " + key("a") + " allow session   " + key("n") + " deny",
			key("y") + " once  " + key("a") + " session  " + key("n") + " deny",
			"y / a / n",
			"y a n",
			"yan",
		} {
			bodyRows = styledRows(labels, max(width-2, 1), s.theme.Colored())
			if len(bodyRows) <= rows {
				break
			}
		}
		bodyRows = bodyRows[:min(len(bodyRows), rows)]
	case modeBusy:
	}
	activity := (s.mode == modeBusy && s.status != "") || s.handoffActiveLocked(now)
	room := rows - len(bodyRows) - 1
	if rule {
		room--
	}
	if activity {
		room--
	}
	if preview := s.previewLocked(now); preview != "" && room > 0 {
		previewRows := styledRows(preview, width, s.theme.Colored())
		s.frame = append(s.frame, previewRows[max(0, len(previewRows)-room):]...)
	}
	if activity && len(s.frame) < s.rows-1 {
		s.activityRow = len(s.frame)
		s.frame = append(s.frame, "")
	}
	if rule {
		s.frame = append(s.frame, s.composerRuleLocked(width))
	}
	bodyStart := len(s.frame)
	for i, row := range bodyRows {
		if s.mode == modePrompt && len(s.text) == 0 {
			row = s.theme.Graphite(s.theme.Italic(row))
		}
		if marker {
			prefix := "  "
			if start+i == 0 {
				prefix = s.theme.Bold(s.theme.Accent("›")) + " "
			}
			row = prefix + row
		}
		s.frame = append(s.frame, choiceBar+row)
	}
	if len(s.frame) < s.rows {
		s.footerRow = len(s.frame)
		s.frame = append(s.frame, "")
	}
	s.frameRow, s.frameCol = len(s.frame)-1, 0
	if s.mode == modePrompt {
		s.frameRow = bodyStart + editRow - start
		s.frameCol = editCol
		if marker {
			s.frameCol += 2
		}
		s.frameCol = min(s.frameCol, s.cols-1)
	}
	s.layoutDirty = false
}

// activityLocked is the animated row while the model or a tool works: the
// expert grid, the status, and the elapsed seconds. After a turn it is the
// hand-off of the grid into the reply dot.
func (s *Screen) activityLocked() string {
	now := time.Now()
	width := max(s.cols-1, 2)
	if s.status == "" {
		return styledRows(s.theme.Handoff(s.handoffProgressLocked(now), expertCells), width, s.theme.Colored())[0]
	}
	status := s.theme.Ink(s.status)
	if s.effects.toolRunning {
		status = s.theme.Fill(s.fillStepLocked(now)) + " " + status
	}
	row := s.theme.Experts(s.tickLocked(now), expertCells, expertsFiring) + "  " + status
	if !s.activitySince.IsZero() {
		row += s.theme.Graphite(" · " + strconv.Itoa(int(now.Sub(s.activitySince).Seconds())) + "s")
	}
	return styledRows(row, width, s.theme.Colored())[0]
}

// footerLocked is the status row: background work, the context footer, and
// the key hints of the current mode when they fit.
func (s *Screen) footerLocked() string {
	now := time.Now()
	width := max(s.cols-1, 2)
	var lead, hint string
	switch s.mode {
	case modePrompt:
		if s.background != "" {
			lead = s.theme.Experts(s.tickLocked(now), 4, 1) + " " + s.theme.Graphite(s.theme.Italic(s.background)) + s.theme.Hairline(" · ")
		}
		hint = "enter send · ctrl-j newline"
	case modeChoice:
		lead = s.theme.Glow("●", s.breathLocked(now)) + " "
		hint = "enter / esc deny"
	case modeBusy:
		hint = "ctrl-c cancel"
	}
	footer := s.footerTextLocked(now)
	line := lead + footer
	visible := cellWidth(withoutStyles(line))
	if footer != "" {
		hint = "   " + hint
	}
	if visible+cellWidth(hint) <= width {
		line += s.theme.Graphite(hint)
	}
	return styledRows(line, width, s.theme.Colored())[0]
}

func (s *Screen) drawLocked() {
	if s.closed || s.fault != nil {
		return
	}
	if s.introDoneLocked(time.Now()) {
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
	if s.layoutDirty || len(s.frame) == 0 {
		s.layoutLocked()
		s.lastFrame = time.Now()
	}
	if s.activityRow >= 0 {
		s.frame[s.activityRow] = s.activityLocked()
	}
	if s.footerRow >= 0 {
		s.frame[s.footerRow] = s.footerLocked()
	}
	visible := s.mode == modePrompt
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

// SetStatus shows what the model works on. Empty text ends the activity row
// with the hand-off into the reply dot.
func (s *Screen) SetStatus(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStatusLocked(text, false)
}

// SetToolStatus shows a running tool: its title after a drop that fills.
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
	switch {
	case text == "" && s.status != "" && s.effects.motion:
		s.effects.handoffAt = now
	case text != "":
		s.effects.handoffAt = time.Time{}
	}
	if tool && (!s.effects.toolRunning || text != s.status) {
		s.effects.toolSince = now
	}
	if text != s.status {
		s.activitySince = now
	}
	s.effects.toolRunning = tool
	s.status = text
	s.layoutDirty = true
	s.drawLocked()
	s.wake.signal()
}

// SetBackgroundWork shows work that runs while the user types, such as a
// background compaction, as an animated mark in the footer. Empty text
// removes it.
func (s *Screen) SetBackgroundWork(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == s.background {
		return
	}
	s.background = text
	s.drawLocked()
	s.wake.signal()
}

// SetPreview shows a line that is not streamed, such as reasoning text, or
// clears the preview with empty text.
func (s *Screen) SetPreview(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == s.preview && s.effects.previewBody == "" {
		return
	}
	s.preview = text
	s.effects.previewPrefix, s.effects.previewBody = "", ""
	s.effects.wet = s.effects.wet[:0]
	s.layoutDirty = true
	s.drawLocked()
}

func (s *Screen) finishLocked(value answer) {
	if s.result != nil {
		s.result <- value
		s.result = nil
	}
	s.mode = modeBusy
	s.text = nil
	s.position, s.inputBytes = 0, 0
	s.placeholder = ""
	s.layoutDirty = true
	s.wake.signal()
}

// Drain before installing a new owner. The reader uses the same lock, so no
// byte read for an earlier mode can be delivered after this boundary.
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
		}
	}
	return errors.New("terminal queued input exceeds 1 MiB")
}

func (s *Screen) await(ctx context.Context, mode inputMode, placeholder string) (answer, error) {
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
	if err := s.drainLocked(); err != nil {
		if !errors.Is(err, context.Canceled) {
			s.failLocked(err)
		}
		s.mu.Unlock()
		return answer{}, err
	}
	result := make(chan answer, 1)
	s.result, s.mode, s.placeholder = result, mode, placeholder
	s.text = nil
	s.position, s.inputBytes = 0, 0
	s.historyPosition = len(s.history)
	s.draft = ""
	s.layoutDirty = true
	s.drawLocked()
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
	value, err := s.await(ctx, modePrompt, placeholder)
	return value.text, err
}

func (s *Screen) Choose(ctx context.Context) (Choice, error) {
	value, err := s.await(ctx, modeChoice, "")
	return value.choice, err
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
		s.finishLocked(answer{err: errors.New("terminal input exceeds 1 MiB")})
		return
	}
	insert := []rune(text)
	oldLen := len(s.text)
	s.text = append(s.text, insert...)
	copy(s.text[s.position+len(insert):], s.text[s.position:oldLen])
	copy(s.text[s.position:], insert)
	s.position += len(insert)
	s.inputBytes += len(text)
	s.layoutDirty = true
}

func (s *Screen) keyLocked(k key) bool {
	if k.kind == keyInterrupt && !k.paste {
		s.mode = modeBusy
		s.layoutDirty = true
		return true
	}
	if s.mode == modeBusy || k.stale {
		return false
	}
	if s.mode == modeChoice {
		if k.paste {
			return false
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
			return false
		}
		s.finishLocked(answer{choice: choice})
		return false
	}
	if s.typing != nil && k.kind != keyEnter {
		close(s.typing)
		s.typing = nil
	}
	s.layoutDirty = true
	switch k.kind {
	case keyText:
		s.insertLocked(k.text)
	case keyNewline:
		s.insertLocked("\n")
	case keyEnter:
		text := string(s.text)
		if text != "" && (len(s.history) == 0 || s.history[len(s.history)-1] != text) {
			s.history = append(s.history, text)
			if len(s.history) > 100 {
				copy(s.history, s.history[1:])
				s.history = s.history[:100]
			}
		}
		s.finishLocked(answer{text: text})
	case keyLeft:
		s.position = previousCluster(s.text, s.position)
	case keyRight:
		s.position = nextCluster(s.text, s.position)
	case keyHome:
		for s.position > 0 && s.text[s.position-1] != '\n' {
			s.position--
		}
	case keyEnd:
		for s.position < len(s.text) && s.text[s.position] != '\n' {
			s.position++
		}
	case keyBackspace:
		start := previousCluster(s.text, s.position)
		for _, r := range s.text[start:s.position] {
			s.inputBytes -= utf8.RuneLen(r)
		}
		s.text = append(s.text[:start], s.text[s.position:]...)
		s.position = start
	case keyDelete:
		end := nextCluster(s.text, s.position)
		for _, r := range s.text[s.position:end] {
			s.inputBytes -= utf8.RuneLen(r)
		}
		s.text = append(s.text[:s.position], s.text[end:]...)
	case keyClear:
		s.text = nil
		s.position = 0
		s.inputBytes = 0
	case keyUp, keyDown:
		if s.historyPosition == len(s.history) {
			s.draft = string(s.text)
		}
		if k.kind == keyUp && s.historyPosition > 0 {
			s.historyPosition--
		}
		if k.kind == keyDown && s.historyPosition < len(s.history) {
			s.historyPosition++
		}
		text := s.draft
		if s.historyPosition < len(s.history) {
			text = s.history[s.historyPosition]
		}
		s.text = []rune(text)
		s.inputBytes = len(text)
		s.position = len(s.text)
	}
	return false
}

func (s *Screen) resizeLocked(rows, cols int) {
	if rows == s.rows && cols == s.cols {
		return
	}
	// Explicit live rows can wrap again when the terminal narrows. Their
	// previous row coordinates are no longer suitable for a dirty-row diff.
	up := 0
	for i := range min(s.cursorRow, len(s.live)) {
		up += len(styledRows(s.live[i], cols, false))
	}
	up += s.cursorCol / cols
	s.cursorRow = min(up, rows-1)
	s.cursorCol %= cols
	s.beginPaint()
	s.eraseIntoPaintLocked()
	s.rows, s.cols = rows, cols
	s.layoutDirty = true
	if !s.closed && s.fault == nil {
		s.renderLocked()
	}
	s.flushPaintLocked()
}

// waitLocked returns how long the read loop may block: until the next
// animation frame at interval, until a lone Escape becomes a key, or without
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
