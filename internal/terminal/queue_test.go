package terminal

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// While the agent works, Enter queues the draft as steering, Alt+Enter
// queues it for later, a command always waits for later, and an empty Enter
// does nothing. Each queued message shows above the composer.
func TestBusyKeysQueueSteeringAndFollowUps(t *testing.T) {
	s, take := renderScreen(t, modeBusy, 20, 40)
	typeKeys(s, "make the fix minimal")
	s.keyLocked(key{kind: keyEnter})
	typeKeys(s, "also update the README and the changelog")
	s.keyLocked(key{kind: keyAltEnter})
	typeKeys(s, "/status")
	s.keyLocked(key{kind: keyEnter})
	s.keyLocked(key{kind: keyEnter})
	if len(s.text) != 0 || s.mode != modeBusy {
		t.Fatalf("draft after queueing = %q in mode %d", string(s.text), s.mode)
	}
	s.drawLocked()
	take()
	frame := strings.Join(s.live, "\n")
	for _, text := range []string{"make the fix minimal", "also update the README", "/status"} {
		if !strings.Contains(frame, text) {
			t.Fatalf("queued %q is not shown: %q", text, s.live)
		}
	}
	for _, row := range s.live {
		if cellWidth(withoutStyles(row)) >= s.cols {
			t.Fatalf("queue row wraps: %q", row)
		}
	}
	if got := s.TakeSteering(); strings.Join(got, "|") != "make the fix minimal" {
		t.Fatalf("steering = %q", got)
	}
	var later []string
	for {
		text, ok := s.TakeFollowUp()
		if !ok {
			break
		}
		later = append(later, text)
	}
	if strings.Join(later, "|") != "also update the README and the changelog|/status" {
		t.Fatalf("follow-ups = %q", later)
	}
	if len(s.queue) != 0 {
		t.Fatalf("queue after taking all = %v", s.queue)
	}
}

// Alt+Up moves every queued message back into the editor, oldest first and
// after the draft, so the user can edit them before sending.
func TestAltUpRestoresTheQueueAfterTheDraft(t *testing.T) {
	s := Screen{mode: modeBusy}
	typeKeys(&s, "first")
	s.keyLocked(key{kind: keyEnter})
	typeKeys(&s, "second")
	s.keyLocked(key{kind: keyAltEnter})
	typeKeys(&s, "draft")
	s.keyLocked(key{kind: keyAltUp})
	if got := string(s.text); got != "draft\n\nfirst\n\nsecond" || len(s.queue) != 0 {
		t.Fatalf("restored editor = %q, queue %v", got, s.queue)
	}
	if s.position != len(s.text) || s.inputBytes != len(string(s.text)) {
		t.Fatalf("cursor %d and size %d do not match the restored text", s.position, s.inputBytes)
	}
}

// The draft that the user types while the agent works is still there when
// the next prompt opens, and Enter sends it.
func TestDraftSurvivesIntoTheNextPrompt(t *testing.T) {
	s, _ := renderScreen(t, modeBusy, 12, 60)
	s.ctx = context.Background()
	typeKeys(s, "half a thought")
	sent := make(chan string, 1)
	go func() {
		text, err := s.Prompt(context.Background(), "Ask")
		if err != nil {
			t.Error(err)
		}
		sent <- text
	}()
	for {
		s.mu.Lock()
		if s.result != nil {
			break
		}
		s.mu.Unlock()
		runtime.Gosched()
	}
	if got := string(s.text); got != "half a thought" {
		s.mu.Unlock()
		t.Fatalf("prompt opened with %q", got)
	}
	s.keyLocked(key{kind: keyEnter})
	s.mu.Unlock()
	if got := <-sent; got != "half a thought" {
		t.Fatalf("prompt sent %q", got)
	}
}

// An approval can appear while the user types a steering message. A key
// typed just after it appears edits the draft and does not answer; a key
// after the grace period answers and leaves the draft alone.
func TestApprovalIgnoresKeysTypedForTheDraft(t *testing.T) {
	result := make(chan answer, 1)
	s := Screen{mode: modeChoice, result: result, choiceSince: time.Now().Add(time.Hour)}
	s.keyLocked(key{kind: keyText, text: "y"})
	if len(result) != 0 || s.mode != modeChoice || string(s.text) != "y" {
		t.Fatalf("a key in the grace period answered or was lost: mode %d, draft %q", s.mode, string(s.text))
	}
	s.choiceSince = time.Now().Add(-choiceGrace)
	s.keyLocked(key{kind: keyText, text: "y"})
	if got := (<-result).choice; got != AllowOnce || string(s.text) != "y" {
		t.Fatalf("choice = %d, draft %q", got, string(s.text))
	}
}

// Bytes that wait unread when an approval appears were typed for the draft:
// the drain gives them to the editor, so they cannot answer the approval.
func TestDrainGivesWaitingBytesToTheDraft(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	if _, err := writer.WriteString("yes\ry"); err != nil {
		t.Fatal(err)
	}
	s := Screen{mode: modeBusy, input: input}
	if err := s.drainLocked(); err != nil {
		t.Fatal(err)
	}
	if string(s.text) != "y" || len(s.queue) != 1 || s.queue[0].text != "yes" || s.queue[0].kind != queueSteer {
		t.Fatalf("draft %q, queue %v", string(s.text), s.queue)
	}
}
