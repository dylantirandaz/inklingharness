package terminal

import (
	"strings"
	"testing"
)

func menuScreen(t *testing.T) (*Screen, func() string) {
	t.Helper()
	s, take := renderScreen(t, modePrompt, 16, 70)
	s.commands = []Command{
		{Name: "help", Usage: "help", Description: "show these commands"},
		{Name: "tools", Usage: "tools [N]", Description: "open full output of this turn's tools"},
		{Name: "title", Usage: "title [TEXT]", Description: "show or set the session title"},
		{Name: "quit", Usage: "quit", Description: "save and exit"},
	}
	s.result = make(chan answer, 1)
	return s, take
}

func typeKeys(s *Screen, text string) {
	for _, r := range text {
		s.keyLocked(key{kind: keyText, text: string(r)})
	}
}

func TestMenuFiltersSelectsAndCompletes(t *testing.T) {
	s, take := menuScreen(t)
	typeKeys(s, "/t")
	s.drawLocked()
	take()
	frame := withoutStyles(strings.Join(s.live, "\n"))
	if !strings.Contains(frame, "❯ /tools [N]") || !strings.Contains(frame, "/title [TEXT]") || strings.Contains(frame, "/help") {
		t.Fatalf("menu did not filter on the prefix: %q", frame)
	}
	s.keyLocked(key{kind: keyDown})
	s.keyLocked(key{kind: keyText, text: "\t"})
	if got := string(s.text); got != "/title " || s.position != len(s.text) {
		t.Fatalf("Tab completed %q at %d", got, s.position)
	}
	if len(s.menuLocked()) != 0 {
		t.Fatal("menu stayed open after a completed command with a space")
	}
	s.keyLocked(key{kind: keyClear})
	typeKeys(s, "/qu")
	result := s.result
	s.keyLocked(key{kind: keyEnter})
	select {
	case sent := <-result:
		if sent.text != "/quit" {
			t.Fatalf("Enter sent %q", sent.text)
		}
	default:
		t.Fatal("Enter on a command without arguments did not send it")
	}
}

func TestMenuEscapeClosesUntilInputChanges(t *testing.T) {
	s, _ := menuScreen(t)
	typeKeys(s, "/")
	if len(s.menuLocked()) != 4 {
		t.Fatalf("slash alone lists %d commands", len(s.menuLocked()))
	}
	s.keyLocked(key{kind: keyEscape})
	if len(s.menuLocked()) != 0 {
		t.Fatal("Escape did not close the menu")
	}
	s.keyLocked(key{kind: keyUp})
	if len(s.menuLocked()) != 0 || string(s.text) != "/" {
		t.Fatal("a closed menu must not reopen or change the input on Up")
	}
	typeKeys(s, "h")
	if len(s.menuLocked()) != 1 {
		t.Fatal("new input did not reopen the menu")
	}
	s.keyLocked(key{kind: keyText, text: "\t", paste: true})
	if string(s.text) != "/h\t" {
		t.Fatalf("pasted tab was treated as completion: %q", s.text)
	}
	s.keyLocked(key{kind: keyClear})
	typeKeys(s, "/help now")
	if len(s.menuLocked()) != 0 {
		t.Fatal("menu stayed open after a space")
	}
}

func TestMenuFitsSmallTerminals(t *testing.T) {
	for _, size := range [][2]int{{8, 20}, {8, 40}, {12, 70}, {4, 20}, {10, 10}} {
		s, _ := menuScreen(t)
		s.rows, s.cols = size[0], size[1]
		typeKeys(s, "/")
		s.layoutLocked()
		if len(s.frame) > s.rows {
			t.Fatalf("%v: %d rows exceed the screen", size, len(s.frame))
		}
		for _, row := range s.frame {
			if width := cellWidth(withoutStyles(row)); width >= s.cols {
				t.Fatalf("%v: row uses %d cells: %q", size, width, row)
			}
		}
		if len(s.menuLocked()) == 0 {
			result := s.result
			typeKeys(s, "h")
			s.keyLocked(key{kind: keyEnter})
			if sent := <-result; sent.text != "/h" {
				t.Fatalf("hidden menu changed input to %q", sent.text)
			}
		}
	}
}

// A description that does not fit ends with an ellipsis, so a cut shows as a
// cut and not as a word that stops in the middle.
func TestMenuDescriptionEndsWithEllipsisWhenCut(t *testing.T) {
	s, _ := menuScreen(t)
	s.commands = []Command{{Name: "tinker", Usage: "tinker [MODEL]", Description: "fine-tune MODEL on your sessions with Tinker and switch to it"}}
	s.rows, s.cols = 16, 40
	typeKeys(s, "/")
	s.layoutLocked()
	var menuRow string
	for _, row := range s.frame {
		if text := withoutStyles(row); strings.Contains(text, "/tinker") {
			menuRow = strings.TrimRight(text, " ")
		}
	}
	if !strings.Contains(menuRow, "fine-tune MODEL") || !strings.HasSuffix(menuRow, "…") {
		t.Fatalf("a cut description does not end with an ellipsis: %q", menuRow)
	}
	for _, row := range s.frame {
		if width := cellWidth(withoutStyles(row)); width >= s.cols {
			t.Fatalf("row uses %d cells: %q", width, row)
		}
	}
}

func TestMenuEnterLeavesArgumentsForInput(t *testing.T) {
	s, _ := menuScreen(t)
	typeKeys(s, "/ti")
	s.keyLocked(key{kind: keyEnter})
	if s.result == nil || string(s.text) != "/title " {
		t.Fatal("Enter must complete an argument command without sending it")
	}
	typeKeys(s, "A new title")
	result := s.result
	s.keyLocked(key{kind: keyEnter})
	if sent := <-result; sent.text != "/title A new title" {
		t.Fatalf("argument changed to %q", sent.text)
	}
}
