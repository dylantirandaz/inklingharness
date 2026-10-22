package terminal

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// queueKind tells when a queued message reaches the model.
type queueKind uint8

const (
	// queueSteer goes into the current turn at its next boundary.
	queueSteer queueKind = iota
	// queueLater starts a turn of its own after the current turn ends.
	queueLater
)

// queuedMessage is one message that the user sent while the agent worked.
type queuedMessage struct {
	kind queueKind
	text string
}

// choiceGrace is how long an approval ignores keys after it appears. The
// user can type a draft when the approval comes, so a key this soon goes to
// the draft and cannot answer the approval.
const choiceGrace = 400 * time.Millisecond

// queueRowLimit bounds the queue rows, so the transcript stays in view.
const queueRowLimit = 5

// TakeSteering removes the steering messages from the queue and returns them,
// oldest first. The messages for later stay in the queue.
func (s *Screen) TakeSteering() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var taken []string
	s.queue = slices.DeleteFunc(s.queue, func(message queuedMessage) bool {
		steer := message.kind == queueSteer
		if steer {
			taken = append(taken, message.text)
		}
		return steer
	})
	if len(taken) > 0 {
		s.layoutDirty = true
		s.drawLocked()
	}
	return taken
}

// TakeFollowUp removes the oldest message for later from the queue and
// returns it.
func (s *Screen) TakeFollowUp() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := slices.IndexFunc(s.queue, func(message queuedMessage) bool { return message.kind == queueLater })
	if index < 0 {
		return "", false
	}
	text := s.queue[index].text
	s.queue = slices.Delete(s.queue, index, index+1)
	s.layoutDirty = true
	s.drawLocked()
	return text, true
}

// RestoreQueue moves every queued message back into the editor, oldest first,
// after the draft and with a blank line between messages. A message that
// does not fit in the input limit stays in the queue, with all messages
// after it, so the order does not change.
func (s *Screen) RestoreQueue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restoreQueueLocked() {
		s.drawLocked()
	}
}

// restoreQueueLocked is RestoreQueue without the redraw. It reports whether
// the editor changed.
func (s *Screen) restoreQueueLocked() bool {
	var text strings.Builder
	text.WriteString(string(s.text))
	moved := 0
	for _, message := range s.queue {
		size := len(message.text)
		if text.Len() > 0 {
			size += len("\n\n")
		}
		if text.Len()+size > maxInputBytes {
			break
		}
		if text.Len() > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(message.text)
		moved++
	}
	if moved == 0 {
		return false
	}
	s.queue = slices.Delete(s.queue, 0, moved)
	s.text = []rune(text.String())
	s.position, s.inputBytes = len(s.text), text.Len()
	s.historyPosition, s.draft = len(s.history), ""
	s.verticalMove = false
	s.menuIndex, s.menuDismissed = 0, false
	s.layoutDirty = true
	return true
}

// sendLocked ends an edit at Enter, or at Alt+Enter when later is true. A
// prompt answers with the text. While the agent works, the text goes into
// the queue; an empty draft goes nowhere.
func (s *Screen) sendLocked(later bool) {
	text := string(s.text)
	switch s.mode {
	case modePrompt:
		s.rememberLocked(text)
		s.finishLocked(answer{text: text})
	case modeBusy, modeChoice:
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return
		}
		s.rememberLocked(text)
		message := queuedMessage{kind: queueSteer, text: text}
		// A command cannot go into a model request, so it waits for the
		// turn to end, as a message for later does.
		if later || strings.HasPrefix(trimmed, "/") {
			message.kind = queueLater
		}
		s.queue = append(s.queue, message)
	case modeView:
		panic("terminal: document view reached the editor")
	}
	s.text, s.position, s.inputBytes = nil, 0, 0
	s.historyPosition, s.draft = len(s.history), ""
	s.verticalMove = false
	s.layoutDirty = true
}

// rememberLocked adds sent text to the history. The history skips a repeat of
// its last entry and keeps the newest 100 entries.
func (s *Screen) rememberLocked(text string) {
	if text == "" || len(s.history) > 0 && s.history[len(s.history)-1] == text {
		return
	}
	s.history = append(s.history, text)
	if len(s.history) > 100 {
		copy(s.history, s.history[1:])
		s.history = s.history[:100]
	}
}

// queueRowsLocked draws up to limit rows of the queue, oldest first, one row
// for each message. When the queue has more messages than rows, the last row
// counts the messages that it does not show.
func (s *Screen) queueRowsLocked(width, limit int) []string {
	limit = min(limit, queueRowLimit)
	// The label and its margins take nine cells; a narrower row has no room
	// for the text.
	if len(s.queue) == 0 || limit <= 0 || width < 12 {
		return nil
	}
	shown := s.queue
	if len(shown) > limit {
		shown = shown[:limit-1]
	}
	rows := make([]string, 0, limit)
	for _, message := range shown {
		var label string
		switch message.kind {
		case queueSteer:
			label = "steer"
		case queueLater:
			label = "later"
		}
		text := presentation.Ellipsize(strings.Join(strings.Fields(message.text), " "), width-9)
		rows = append(rows, "  "+s.theme.Graphite(label)+"  "+s.theme.Ink(text))
	}
	if hidden := len(s.queue) - len(shown); hidden > 0 {
		rows = append(rows, "  "+s.theme.Graphite("+"+strconv.Itoa(hidden)+" more"))
	}
	return rows
}

// busyPlaceholder is the composer text while the agent works and the draft
// is empty. A screen of one row has no room for the key hints, so there the
// placeholder names the stop key.
func busyPlaceholder(rows, room int) string {
	if rows == 1 {
		return widest(room, "Steer · ^C stop", "^C stop", "^C")
	}
	return widest(room, "Steer, or Alt+Enter to queue for later", "Steer the work", "Steer")
}

// busyHint names the keys while the agent works. The edit key shows only
// when the queue has a message to edit; the stop key stays at every width.
func busyHint(room int, queued bool) string {
	if queued {
		return widest(room, "Enter steer · Alt+Enter later · Alt+↑ edit · ^C stop", "Enter steer · Alt+↑ edit · ^C stop", "Enter steer · ^C stop", "^C stop")
	}
	return widest(room, "Enter steer · Alt+Enter later · ^C stop", "Enter steer · ^C stop", "^C stop")
}

// widest returns the first form that fits room cells, or the last form when
// none fits.
func widest(room int, forms ...string) string {
	for _, form := range forms[:len(forms)-1] {
		if cellWidth(form) <= room {
			return form
		}
	}
	return forms[len(forms)-1]
}
