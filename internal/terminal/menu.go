package terminal

import (
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// Command is one slash command of the completion menu. Usage names its
// arguments, such as "tools [N]"; a command without arguments has none.
type Command struct {
	Name, Usage, Description string
}

// takesArgument reports whether completing the command should leave the
// cursor after a space, ready for its argument.
func (c Command) takesArgument() bool { return strings.Contains(c.Usage, " ") }

// menuRowLimit bounds the menu so the transcript stays in view.
const menuRowLimit = 7

// SetCommands installs the slash commands that the composer completes. The
// menu opens while the input is a single word that starts with a slash.
func (s *Screen) SetCommands(commands []Command) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands[:0], commands...)
	for index := range s.commands {
		command := &s.commands[index]
		command.Name = strings.Join(strings.Fields(presentation.Safe(command.Name)), "")
		command.Usage = strings.Join(strings.Fields(presentation.Safe(command.Usage)), " ")
		command.Description = strings.Join(strings.Fields(presentation.Safe(command.Description)), " ")
	}
	s.layoutDirty = true
	s.drawLocked()
}

// menuLocked lists the commands that the input names so far, or nil when the
// menu is closed: another mode, no slash word, or the user dismissed it.
func (s *Screen) menuLocked() []Command {
	if s.mode != modePrompt || s.menuDismissed || len(s.commands) == 0 || s.rows < 8 || s.cols < 20 {
		return nil
	}
	text := string(s.text)
	if !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \n\t") {
		return nil
	}
	prefix := text[1:]
	matches := s.menuMatches[:0]
	for _, command := range s.commands {
		if strings.HasPrefix(command.Name, prefix) {
			matches = append(matches, command)
		}
	}
	s.menuMatches = matches
	return matches
}

// completeLocked replaces the input with the command; a command that takes
// an argument leaves the cursor after a space.
func (s *Screen) completeLocked(command Command) {
	text := "/" + command.Name
	if command.takesArgument() {
		text += " "
	}
	s.text = append(s.text[:0], []rune(text)...)
	s.position = len(s.text)
	s.inputBytes = len(text)
	s.menuIndex, s.menuDismissed = 0, false
}

// menuKeyLocked handles a key while the menu is open. It reports whether the
// key was consumed. Enter on a command without arguments sends it.
func (s *Screen) menuKeyLocked(k key, matches []Command) bool {
	if k.paste {
		return false
	}
	s.menuIndex = min(s.menuIndex, len(matches)-1)
	switch k.kind {
	case keyUp:
		s.menuIndex = max(0, s.menuIndex-1)
	case keyDown:
		s.menuIndex = min(len(matches)-1, s.menuIndex+1)
	case keyEscape:
		s.menuDismissed = true
	case keyText:
		if k.text != "\t" {
			return false
		}
		s.completeLocked(matches[s.menuIndex])
	case keyEnter:
		selected := matches[s.menuIndex]
		if string(s.text) == "/"+selected.Name {
			return false
		}
		s.completeLocked(selected)
		if selected.takesArgument() {
			return true
		}
		return false
	default:
		return false
	}
	return true
}

// menuRowsLocked draws up to limit menu rows: the name in the accent, the
// usage, and the description, with the selected row marked.
func (s *Screen) menuRowsLocked(matches []Command, width, limit int) []string {
	if limit <= 0 || len(matches) == 0 {
		return nil
	}
	s.menuIndex = min(s.menuIndex, len(matches)-1)
	first := max(0, min(s.menuIndex-limit+1, len(matches)-limit))
	usageWidth := 0
	for _, command := range matches[first:min(first+limit, len(matches))] {
		usageWidth = max(usageWidth, cellWidth(command.Usage))
	}
	usageWidth = min(usageWidth, max(width-3, 0))
	rows := make([]string, 0, limit)
	for index := first; index < len(matches) && len(rows) < limit; index++ {
		command := matches[index]
		marker := "  "
		usage := fitDocumentLabel(command.Usage, usageWidth)
		name := s.theme.Accent("/" + usage)
		if index == s.menuIndex {
			marker = s.theme.Accent("▸ ")
			name = s.theme.Bold(name)
		}
		row := marker + name
		remaining := width - 3 - cellWidth(usage)
		pad := usageWidth - cellWidth(usage) + 2
		if remaining > pad {
			description := presentation.Ellipsize(command.Description, remaining-pad)
			row += strings.Repeat(" ", pad) + s.theme.Graphite(description)
		}
		rows = append(rows, row)
	}
	return rows
}
