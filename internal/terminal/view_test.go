package terminal

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

func TestViewInterruptDoesNotInterruptChat(t *testing.T) {
	s, take := renderScreen(t, modeBusy, 12, 60)
	s.drawLocked()
	result := make(chan answer, 1)
	s.result, s.mode = result, modeView
	view := newDocumentView("Latest tools", []Document{{Title: "Output", Text: "full tool output", Format: PlainDocument}}, 0)
	s.enterViewLocked(view)
	s.layoutDirty = true
	s.drawLocked()
	take()
	for _, k := range []key{
		{kind: keyText, text: "q", paste: true},
		{kind: keyEnter, paste: true},
		{kind: keyEscape, paste: true},
		{kind: keyInterrupt, paste: true},
		{kind: keyEscape, stale: true},
		{kind: keyEnter, stale: true},
		{kind: keyText, text: "q", stale: true},
	} {
		if s.keyLocked(k) || s.view != view {
			t.Fatalf("pasted or stale key closed the view: %+v", k)
		}
	}
	if s.keyLocked(key{kind: keyInterrupt}) {
		t.Fatal("closing the view would also interrupt the chat")
	}
	select {
	case completed := <-result:
		if completed.err != nil {
			t.Fatalf("closing the view returned %v", completed.err)
		}
	default:
		t.Fatal("closing the view did not release its caller")
	}
	if s.view != nil || !strings.Contains(take(), "\x1b[?1049l") {
		t.Fatal("closing the view did not restore the main screen")
	}
}

func TestViewDefersTranscriptUntilClose(t *testing.T) {
	s, take := renderScreen(t, modeBusy, 12, 60)
	s.drawLocked()
	s.mode = modeView
	s.enterViewLocked(newDocumentView("Latest tools", []Document{{Title: "Output", Text: "full tool output", Format: PlainDocument}}, 0))
	s.layoutDirty = true
	s.drawLocked()
	take()
	if _, err := s.Write([]byte("deferred transcript\n")); err != nil {
		t.Fatal(err)
	}
	if got := take(); got != "" {
		t.Fatalf("transcript was written to the document view: %q", got)
	}
	s.keyLocked(key{kind: keyEnter})
	got := take()
	_, afterRestore, restored := strings.Cut(got, "\x1b[?1049l")
	if !restored || !strings.Contains(afterRestore, "deferred transcript") || strings.Count(got, "deferred transcript") != 1 {
		t.Fatalf("transcript was not written once after screen restoration: %q", got)
	}
}

func TestDocumentWrapPreservesTextAndPosition(t *testing.T) {
	source := Document{
		Title:  "Output\x1b]52;c;Zm9v\a",
		Text:   "\x1b[2Ja\t界e\u0301\n\nfirst middle last\x1b]0;title\a",
		Format: PlainDocument,
	}
	const expected = "a   界e\u0301\n\nfirst middle last"
	var document indexedDocument
	for _, width := range []int{3, 7, 19} {
		anchor := 0
		if document.ready {
			anchor = document.rows[document.top].start
		}
		document.prepare(source, width)
		if document.title != "Output" {
			t.Fatalf("document title contains terminal controls: %q", document.title)
		}
		first := document.rows[document.top]
		if first.start > anchor || first.end < anchor {
			t.Fatalf("resize moved away from byte %d to %+v", anchor, first)
		}
		var text strings.Builder
		for index, row := range document.rows {
			if index > 0 && row.line != document.rows[index-1].line {
				text.WriteByte('\n')
			}
			part := document.text[row.start:row.end]
			if !utf8.ValidString(part) {
				t.Fatalf("width %d split a UTF-8 character: %q", width, part)
			}
			cells := 0
			for _, character := range part {
				cells += runeWidth(character)
			}
			if cells > width {
				t.Fatalf("row %q exceeds width %d", part, width)
			}
			text.WriteString(part)
		}
		if text.String() != expected {
			t.Fatalf("width %d changed the document: %q", width, text.String())
		}
		document.top = len(document.rows) / 2
	}
}

func TestViewTabsScrollbarAndKeys(t *testing.T) {
	s, take := renderScreen(t, modeBusy, 12, 60)
	s.theme = presentation.NewTheme(presentation.TrueColor, presentation.DarkBackground, presentation.AccentPlum)
	s.drawLocked()
	s.mode = modeView
	documents := []Document{
		{Title: "Read config.py", Text: strings.Repeat("line\n", 40), Format: PlainDocument},
		{Title: "Run go test", Text: "ok", Format: PlainDocument},
		{Title: "diff.txt · +1 −1", Text: "diff --git a/x b/x\n-old\n+new", Format: DiffDocument},
	}
	s.enterViewLocked(newDocumentView("Latest tools", documents, 0))
	s.layoutDirty = true
	s.drawLocked()
	take()
	tabs := withoutStyles(s.live[0])
	if !strings.Contains(tabs, "1 Read config.py") || !strings.Contains(tabs, "2 Run go test") || strings.Contains(tabs, "Latest") {
		t.Fatalf("tab row = %q", tabs)
	}
	if !strings.Contains(s.live[0], "\x1b[1m") || !strings.HasPrefix(withoutStyles(s.live[1]), "────") {
		t.Fatalf("selected tab is not emphasized or the rule is missing: %q", s.live[:2])
	}
	body := s.live[2 : len(s.live)-1]
	thumbs := 0
	for _, row := range body {
		plain := withoutStyles(row)
		if got := cellWidth(plain); got != 59 {
			t.Fatalf("scrollbar row is %d cells, want 59: %q", got, plain)
		}
		if strings.HasSuffix(plain, "┃") {
			thumbs++
		} else if !strings.HasSuffix(plain, "│") {
			t.Fatalf("row has no scrollbar cell: %q", plain)
		}
	}
	if thumbs == 0 || thumbs == len(body) {
		t.Fatalf("thumb covers %d of %d rows", thumbs, len(body))
	}
	footer := withoutStyles(s.live[len(s.live)-1])
	if !strings.Contains(footer, "1/3 · 1-9/41") {
		t.Fatalf("footer = %q", footer)
	}
	s.keyLocked(key{kind: keyText, text: "G"})
	s.drawLocked()
	take()
	if footer = withoutStyles(s.live[len(s.live)-1]); !strings.Contains(footer, "33-41/41") {
		t.Fatalf("G did not reach the end: %q", footer)
	}
	if plain := withoutStyles(s.live[len(s.live)-2]); !strings.HasSuffix(plain, "┃") {
		t.Fatalf("thumb did not reach the bottom: %q", plain)
	}
	s.keyLocked(key{kind: keyText, text: "\t"})
	s.keyLocked(key{kind: keyText, text: "\t"})
	s.drawLocked()
	take()
	if !strings.Contains(withoutStyles(s.live[2]), "diff --git") || strings.HasSuffix(withoutStyles(s.live[2]), "│") {
		t.Fatalf("Tab did not select the short diff or drew a needless scrollbar: %q", s.live[2])
	}
	if !strings.Contains(s.live[4], "\x1b[") {
		t.Fatal("diff added line is not styled")
	}
	s.keyLocked(key{kind: keyText, text: "9"})
	s.keyLocked(key{kind: keyText, text: "2"})
	s.drawLocked()
	take()
	if withoutStyles(s.live[2]) != "ok" || s.view.selected != 1 {
		t.Fatalf("digit selection failed: selected %d, body %q", s.view.selected, s.live[2])
	}
	s.keyLocked(key{kind: keyText, text: "q"})
	if s.view != nil {
		t.Fatal("q did not close the view")
	}
}

func TestScrollThumb(t *testing.T) {
	if start, end := scrollThumb(0, 5, 10); start != 0 || end != 0 {
		t.Fatalf("fitting document has a thumb %d-%d", start, end)
	}
	if start, end := scrollThumb(0, 100, 10); start != 0 || end != 1 {
		t.Fatalf("top thumb = %d-%d", start, end)
	}
	if start, end := scrollThumb(90, 100, 10); start != 9 || end != 10 {
		t.Fatalf("bottom thumb = %d-%d", start, end)
	}
	if start, end := scrollThumb(10, 20, 10); end-start != 5 || start != 5 {
		t.Fatalf("half thumb = %d-%d", start, end)
	}
}
