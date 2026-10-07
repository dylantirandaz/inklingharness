package terminal

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParserSplitUTF8AndPaste(t *testing.T) {
	var p parser
	var keys []key
	data := "\x1b[200~hé界\r\nsecond\n\x1b[201~\r"
	for _, b := range []byte(data) {
		keys = append(keys, p.feed([]byte{b}, time.Now())...)
	}
	var text strings.Builder
	for i, k := range keys {
		if i == len(keys)-1 {
			if k.kind != keyEnter || k.paste {
				t.Fatalf("final key = %#v", k)
			}
			continue
		}
		if k.kind != keyText || !k.paste {
			t.Fatalf("paste key = %#v", k)
		}
		text.WriteString(k.text)
	}
	if got := text.String(); got != "hé界\nsecond\n" {
		t.Fatalf("paste = %q", got)
	}
}

// A reply to the background query that arrives after the wait, split at any
// byte, must not become keys; the text typed after it must.
func TestParserDropsLateTerminalReplies(t *testing.T) {
	data := "\x1b]11;rgb:1e1e/1c1c/1a1a\x1b\\\x1b[?62;22c\x1b]11;rgb:ff/ff/ff\ax"
	for split := range len(data) {
		var p parser
		keys := append(p.feed([]byte(data[:split]), time.Now()), p.feed([]byte(data[split:]), time.Now())...)
		if len(keys) != 1 || keys[0].kind != keyText || keys[0].text != "x" {
			t.Fatalf("split %d: keys = %#v", split, keys)
		}
	}
	var p parser
	if keys := p.feed([]byte("\x1b]"+strings.Repeat("a", maxReplyBytes)+"x"), time.Now()); len(keys) != 0 || len(p.pending) != 0 {
		t.Fatalf("an unterminated reply was kept or typed: %#v, %d pending", keys, len(p.pending))
	}
}

func TestParserEscapeAndArrow(t *testing.T) {
	now := time.Now()
	var p parser
	if keys := p.feed([]byte("\x1b"), now); len(keys) != 0 {
		t.Fatal(keys)
	}
	if keys := p.expire(now.Add(10 * time.Millisecond)); len(keys) != 0 {
		t.Fatal(keys)
	}
	keys := p.feed([]byte("[D"), now.Add(15*time.Millisecond))
	if len(keys) != 1 || keys[0].kind != keyLeft {
		t.Fatalf("arrow = %#v", keys)
	}
	p.feed([]byte("\x1b"), now)
	keys = p.expire(now.Add(time.Second))
	if len(keys) != 1 || keys[0].kind != keyEscape {
		t.Fatalf("escape = %#v", keys)
	}
}

func TestStalePartialPasteCannotApprove(t *testing.T) {
	now := time.Now()
	var p parser
	p.feed([]byte("\x1b[20"), now)
	p.stalePending = true
	if keys := p.expire(now.Add(time.Second)); len(keys) != 0 {
		t.Fatalf("partial CSI expired: %#v", keys)
	}
	s := Screen{mode: modeChoice, result: make(chan answer, 1)}
	for _, k := range p.feed([]byte("0~y\r\x1b[201~"), now.Add(time.Second)) {
		s.keyLocked(k)
	}
	if s.mode != modeChoice {
		t.Fatal("paste answered approval")
	}
	for _, k := range p.feed([]byte("y"), now.Add(2*time.Second)) {
		s.keyLocked(k)
	}
	if s.mode != modeBusy {
		t.Fatal("fresh input did not answer approval")
	}
}

func TestStaleEscapeCannotDeny(t *testing.T) {
	now := time.Now()
	var p parser
	p.feed([]byte("\x1b"), now)
	p.stalePending = true
	s := Screen{mode: modeChoice, result: make(chan answer, 1)}
	for _, k := range p.expire(now.Add(time.Second)) {
		s.keyLocked(k)
	}
	if s.mode != modeChoice {
		t.Fatal("stale escape answered approval")
	}
}

func TestEditingCombiningWideAndMultiline(t *testing.T) {
	s := Screen{mode: modePrompt, result: make(chan answer, 1)}
	s.insertLocked("ae\u0301界")
	s.keyLocked(key{kind: keyLeft})
	if s.position != 3 {
		t.Fatalf("position = %d", s.position)
	}
	s.keyLocked(key{kind: keyBackspace})
	if got := string(s.text); got != "a界" {
		t.Fatalf("backspace = %q", got)
	}
	s.keyLocked(key{kind: keyNewline})
	s.insertLocked("bc")
	s.keyLocked(key{kind: keyHome})
	s.keyLocked(key{kind: keyDelete})
	if got := string(s.text); got != "a\nc界" {
		t.Fatalf("delete = %q", got)
	}
	s.keyLocked(key{kind: keyEnd})
	if s.position != len(s.text) {
		t.Fatal("end did not reach line end")
	}
	if s.inputBytes != len(string(s.text)) {
		t.Fatalf("byte count = %d", s.inputBytes)
	}
}

func TestHistoryRestoresDraft(t *testing.T) {
	s := Screen{mode: modePrompt, history: []string{"one", "two\nlines"}, historyPosition: 2}
	s.insertLocked("draft")
	s.keyLocked(key{kind: keyUp})
	if got := string(s.text); got != "two\nlines" {
		t.Fatal(got)
	}
	s.keyLocked(key{kind: keyUp})
	if got := string(s.text); got != "one" {
		t.Fatal(got)
	}
	s.keyLocked(key{kind: keyDown})
	s.keyLocked(key{kind: keyDown})
	if got := string(s.text); got != "draft" {
		t.Fatal(got)
	}
}

// A resumed session starts paid background work at the first typed key, so
// Enter on an empty prompt and approval keys must not count as typing.
func TestTypingSignalStartsOnlyAtPromptEdits(t *testing.T) {
	closed := func(channel <-chan struct{}) bool {
		select {
		case <-channel:
			return true
		default:
			return false
		}
	}
	s := Screen{mode: modePrompt, result: make(chan answer, 1)}
	typing := s.ArmTyping()
	s.keyLocked(key{kind: keyEnter})
	if closed(typing) {
		t.Fatal("Enter counted as typing")
	}
	s = Screen{mode: modeChoice, result: make(chan answer, 1)}
	typing = s.ArmTyping()
	s.keyLocked(key{kind: keyText, text: "y"})
	if closed(typing) {
		t.Fatal("an approval key counted as typing")
	}
	for _, k := range []key{{kind: keyText, text: "f"}, {kind: keyText, text: "pasted text", paste: true}, {kind: keyBackspace}, {kind: keyUp}} {
		s = Screen{mode: modePrompt, result: make(chan answer, 1)}
		typing = s.ArmTyping()
		s.keyLocked(k)
		if !closed(typing) {
			t.Fatalf("%+v did not count as typing", k)
		}
		s.keyLocked(key{kind: keyText, text: "g"})
	}
}

func TestInputLimitReturnsErrorAndLeavesBusy(t *testing.T) {
	result := make(chan answer, 1)
	s := Screen{mode: modePrompt, result: result}
	s.insertLocked(strings.Repeat("x", maxInputBytes))
	s.insertLocked("界")
	value := <-result
	if value.err == nil {
		t.Fatal("expected input limit error")
	}
	if s.mode != modeBusy || s.result != nil {
		t.Fatal("oversized input left active prompt")
	}
	s.keyLocked(key{kind: keyText, text: "y"})
	if len(s.text) != 0 {
		t.Fatal("busy input was retained")
	}
}

func TestChoicesAndInterrupt(t *testing.T) {
	for _, test := range []struct {
		key  key
		want Choice
	}{
		{key{kind: keyText, text: "y"}, AllowOnce},
		{key{kind: keyText, text: "a"}, AllowSession},
		{key{kind: keyText, text: "n"}, Deny},
		{key{kind: keyEnter}, Deny},
		{key{kind: keyEscape}, Deny},
	} {
		result := make(chan answer, 1)
		s := Screen{mode: modeChoice, result: result}
		s.keyLocked(test.key)
		if got := (<-result).choice; got != test.want {
			t.Fatalf("choice = %d, want %d", got, test.want)
		}
	}
	result := make(chan answer, 1)
	called := false
	s := Screen{mode: modeChoice, result: result, onInterrupt: func() { called = true }}
	if !s.keyLocked(key{kind: keyInterrupt}) {
		t.Fatal("interrupt was ignored")
	}
	// Prevent drawing: this test exercises callback and ownership, not output.
	s.closed = true
	s.interrupted(result)
	if !called || (<-result).err != context.Canceled {
		t.Fatal("interrupt did not cancel approval")
	}
}

func TestStyledRowsUseCellsAndPreserveStyle(t *testing.T) {
	rows := styledRows("\x1b[31me\u0301界ab\x1b[0m", 3, true)
	if len(rows) != 2 {
		t.Fatalf("rows = %#v", rows)
	}
	if withoutStyles(rows[0]) != "e\u0301界" || withoutStyles(rows[1]) != "ab" {
		t.Fatalf("rows = %#v", rows)
	}
	if !strings.HasPrefix(rows[1], "\x1b[31m") {
		t.Fatal("wrapped style was lost")
	}
	plain := styledRows("\x1b[31mred\x1b[0m", 10, false)
	if len(plain) != 1 || plain[0] != "red" {
		t.Fatalf("no-color rows = %#v", plain)
	}
}
