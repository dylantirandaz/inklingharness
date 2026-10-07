package terminal

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func renderScreen(t *testing.T, mode inputMode, rows, cols int) (*Screen, func() string) {
	t.Helper()
	output, err := os.Create(filepath.Join(t.TempDir(), "terminal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { output.Close() })
	s := &Screen{
		output: output, mode: mode, rows: rows, cols: cols,
		layoutDirty: true, cursorVisible: true,
	}
	var offset int64
	return s, func() string {
		t.Helper()
		if s.fault != nil {
			t.Fatal(s.fault)
		}
		info, err := output.Stat()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(io.NewSectionReader(output, offset, info.Size()-offset))
		if err != nil {
			t.Fatal(err)
		}
		offset = info.Size()
		return string(data)
	}
}

func TestDrawUnchangedAndCursorOnly(t *testing.T) {
	s, take := renderScreen(t, modePrompt, 8, 40)
	s.insertLocked("ab界e\u0301")
	s.drawLocked()
	take()
	s.drawLocked()
	if got := take(); got != "" {
		t.Fatalf("unchanged frame wrote %q", got)
	}
	s.keyLocked(key{kind: keyLeft})
	s.drawLocked()
	if got := take(); got == "" || strings.Contains(got, "\x1b[2K") || strings.Contains(got, "ab界") {
		t.Fatalf("cursor-only move repainted text: %q", got)
	}
	if s.cursorCol != 6 {
		t.Fatalf("cursor column = %d, want marker offset plus four cells", s.cursorCol)
	}
}

func TestBackgroundWorkRepaintsOnlyFooter(t *testing.T) {
	s, take := renderScreen(t, modePrompt, 8, 60)
	s.effects = effects{motion: true, epoch: time.Now()}
	s.insertLocked("composer contents")
	s.drawLocked()
	take()
	s.SetBackgroundWork("compacting")
	for range 2 {
		got := take()
		if strings.Count(got, "\x1b[2K") != 1 || strings.Contains(got, "composer contents") {
			t.Fatalf("background work repainted composer: %q", got)
		}
		if s.cursorRow != s.frameRow || s.cursorCol != s.frameCol || !s.cursorVisible {
			t.Fatal("background work did not restore the input cursor")
		}
		s.effects.epoch = s.effects.epoch.Add(-expertTick)
		s.drawLocked()
	}
}

func TestDrawExpertGridKeepsPreview(t *testing.T) {
	s, take := renderScreen(t, modeBusy, 8, 60)
	s.effects = effects{motion: true, epoch: time.Now()}
	s.preview = "first preview line\nsecond preview line"
	s.status = "working"
	s.activitySince = time.Now()
	s.drawLocked()
	initial := take()
	if len(s.live) != 4 {
		t.Fatalf("busy view contains %d rows, want preview, activity, and footer rows", len(s.live))
	}
	s.effects.epoch = s.effects.epoch.Add(-expertTick)
	s.drawLocked()
	got := take()
	if strings.Count(got, "\x1b[2K") != 1 || strings.Contains(got, "preview") || len(got) >= len(initial) {
		t.Fatalf("expert grid repainted preview: %q", got)
	}
	if s.cursorVisible {
		t.Fatal("busy cursor is visible")
	}
}

func TestDrawGrowthShrinkAndTranscript(t *testing.T) {
	s, take := renderScreen(t, modeBusy, 8, 40)
	s.drawLocked()
	take()
	s.SetPreview("one\ntwo\nthree")
	if got := take(); strings.Count(got, "\r\n") != 3 {
		t.Fatalf("growth did not create exactly three rows: %q", got)
	}
	s.SetPreview("one")
	if got := take(); !strings.Contains(got, "\x1b[J") || strings.Contains(got, "\r\n") {
		t.Fatalf("shrink did not clear old rows in place: %q", got)
	}
	if len(s.live) != 2 || s.cursorRow != 1 {
		t.Fatalf("shrunken live state: rows=%d cursor=%d", len(s.live), s.cursorRow)
	}
	if _, err := s.Write([]byte("saved transcript\n")); err != nil {
		t.Fatal(err)
	}
	got := take()
	if !strings.Contains(got, "saved transcript\r\n") || !strings.Contains(got, "one") {
		t.Fatalf("transcript insertion did not redraw live rows: %q", got)
	}
	s.drawLocked()
	if got := take(); got != "" {
		t.Fatalf("redrawn frame is not stable: %q", got)
	}
}

func TestResizeInvalidatesRowsOnce(t *testing.T) {
	s, take := renderScreen(t, modePrompt, 8, 40)
	s.insertLocked("a long input containing 界 and wrapping text")
	s.drawLocked()
	take()
	s.resizeLocked(8, 40)
	if got := take(); got != "" {
		t.Fatalf("unchanged size repainted: %q", got)
	}
	s.resizeLocked(5, 18)
	if got := take(); !strings.Contains(got, "\x1b[J") {
		t.Fatalf("resize did not discard old row coordinates: %q", got)
	}
	if len(s.live) > 5 || s.cursorRow >= 5 || s.cursorCol >= 18 {
		t.Fatalf("resized frame exceeds terminal: rows=%d cursor=%d,%d", len(s.live), s.cursorRow, s.cursorCol)
	}
	s.drawLocked()
	if got := take(); got != "" {
		t.Fatalf("resized frame is not stable: %q", got)
	}
}

func TestComposerCursorOffsetsAndClipping(t *testing.T) {
	for _, test := range []struct {
		name                           string
		text                           string
		position, rows, cols, row, col int
	}{
		{"wide and combining", "ab界e\u0301", 5, 6, 20, 1, 7},
		{"wrapped boundary", "abcdefghijklmZ", 13, 6, 16, 2, 2},
		{"clipped boundary", "abcdefghijklmZ", 13, 3, 16, 1, 2},
		{"narrow without rule", "界x", 1, 2, 8, 0, 4},
		{"tiny without marker", "界", 1, 1, 3, 0, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, take := renderScreen(t, modePrompt, test.rows, test.cols)
			s.text = []rune(test.text)
			s.position = test.position
			s.drawLocked()
			take()
			if s.cursorRow != test.row || s.cursorCol != test.col {
				t.Fatalf("cursor = %d,%d, want %d,%d", s.cursorRow, s.cursorCol, test.row, test.col)
			}
			if len(s.live) > test.rows {
				t.Fatal("composer exceeds available rows")
			}
		})
	}
}

func TestChoiceNarrowAndNoColor(t *testing.T) {
	for _, size := range [][2]int{{4, 18}, {2, 10}, {1, 20}, {3, 3}} {
		s, take := renderScreen(t, modeChoice, size[0], size[1])
		s.drawLocked()
		got := take()
		if strings.Contains(got, "\x1b[33m") || strings.Contains(got, "\x1b[2m") {
			t.Fatal("no-color view contains style sequences")
		}
		if len(s.live) > s.rows {
			t.Fatal("choice view exceeds available rows")
		}
		body := strings.Join(s.live, "")
		for _, key := range []string{"y", "a", "n"} {
			if !strings.Contains(body, key) {
				t.Fatalf("choice %q missing at %dx%d: %q", key, s.rows, s.cols, body)
			}
		}
	}
}

func TestPromptToBusyClearsComposer(t *testing.T) {
	s, take := renderScreen(t, modePrompt, 8, 40)
	s.insertLocked("finished input")
	s.drawLocked()
	take()
	s.finishLocked(answer{text: "finished input"})
	s.drawLocked()
	if got := take(); !strings.Contains(got, "\x1b[J") {
		t.Fatalf("mode change did not clear old composer rows: %q", got)
	}
	if len(s.live) != 1 || s.cursorVisible {
		t.Fatal("busy view retained the composer or visible cursor")
	}
}

// An idle prompt must not wake the process: the loop blocks until a key, the
// wake pipe, or a deadline that an animation or a lone Escape needs. Without
// motion a turn wakes only once a second, for its elapsed seconds.
func TestReadLoopBlocksUnlessSomethingIsDue(t *testing.T) {
	now := time.Now()
	s := &Screen{mode: modePrompt, effects: effects{motion: true}}
	if got := s.waitLocked(now, s.frameIntervalLocked(now)); got >= 0 {
		t.Fatalf("idle prompt waits %v, want no limit", got)
	}
	s.background = "compacting"
	s.lastFrame = now.Add(-30 * time.Millisecond)
	if got := s.waitLocked(now, s.frameIntervalLocked(now)); got != steadyInterval-30*time.Millisecond {
		t.Fatalf("background work waits %v", got)
	}
	s = &Screen{mode: modeBusy, status: "working", lastFrame: now.Add(-time.Second), effects: effects{motion: true}}
	if got := s.waitLocked(now, s.frameIntervalLocked(now)); got != 0 {
		t.Fatalf("overdue frame waits %v", got)
	}
	s = &Screen{mode: modeBusy, status: "working", lastFrame: now}
	if got := s.waitLocked(now, s.frameIntervalLocked(now)); got != stillInterval {
		t.Fatalf("a turn without motion waits %v", got)
	}
	s = &Screen{mode: modeChoice}
	if got := s.frameIntervalLocked(now); got != 0 {
		t.Fatalf("an approval without motion animates every %v", got)
	}
	s = &Screen{mode: modeBusy}
	s.parser.feed([]byte{27}, now.Add(-10*time.Millisecond))
	if got := s.waitLocked(now, s.frameIntervalLocked(now)); got != escapeTimeout-10*time.Millisecond {
		t.Fatalf("pending Escape waits %v", got)
	}
}

func TestWakePipeEndsABlockedWait(t *testing.T) {
	input, err := newWakePipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.close()
	wake, err := newWakePipe()
	if err != nil {
		t.Fatal(err)
	}
	defer wake.close()
	if ready, woken, err := waitReadable(input.read, wake.read, 0); err != nil || ready || woken {
		t.Fatalf("empty wait = %t %t %v", ready, woken, err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		wake.signal()
		wake.signal()
	}()
	started := time.Now()
	ready, woken, err := waitReadable(input.read, wake.read, -1)
	if err != nil || ready || !woken || time.Since(started) > 2*time.Second {
		t.Fatalf("wake = %t %t %v after %v", ready, woken, err, time.Since(started))
	}
	wake.drain()
	if _, woken, err := waitReadable(input.read, wake.read, 0); err != nil || woken {
		t.Fatalf("drain left a wake: %t %v", woken, err)
	}
	input.signal()
	if ready, _, err := waitReadable(input.read, wake.read, -1); err != nil || !ready {
		t.Fatalf("input readiness = %t %v", ready, err)
	}
}

// A multi-row frame must arrive inside one synchronized update; a one-row
// update or a lone escape sequence must not pay for the markers.
func TestFramesAreSynchronizedAndOSCTextIsSafe(t *testing.T) {
	s, take := renderScreen(t, modePrompt, 8, 40)
	s.drawLocked()
	if got := take(); !strings.HasPrefix(got, syncBegin) || !strings.HasSuffix(got, syncEnd) {
		t.Fatalf("frame is not synchronized: %q", got)
	}
	s.insertLocked("x")
	s.drawLocked()
	if got := take(); got == "" || strings.Contains(got, syncBegin) || strings.Contains(got, syncEnd) {
		t.Fatalf("one-row update = %q, want no synchronization markers", got)
	}
	s.SetTitle("◈ inkling\x07\x1b]0;spoof\x07 · working\nnext")
	got := take()
	if got != "\x1b]2;◈ inkling · working next\x07" {
		t.Fatalf("unsafe or synchronized title: %q", got)
	}
	s.SetTitle("◈ inkling · working next")
	if got := take(); got != "" {
		t.Fatalf("unchanged title wrote %q", got)
	}
	s.Notify("Inkling needs approval: rm\x1b[2J -rf")
	if got := take(); got != "\x1b]9;Inkling needs approval: rm -rf\x07" {
		t.Fatalf("unsafe notification: %q", got)
	}
	if _, err := s.Write([]byte("transcript\n")); err != nil {
		t.Fatal(err)
	}
	if got := take(); !strings.HasPrefix(got, syncBegin) || !strings.HasSuffix(got, syncEnd) {
		t.Fatalf("scrollback insertion is not synchronized: %q", got)
	}
}
