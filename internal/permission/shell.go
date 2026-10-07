package permission

import (
	"encoding/json"
	"errors"
	"strings"
)

// commandPattern matches a whole bash command line. '*' matches any run of
// characters; every other character matches only itself.
type commandPattern string

// blanks are the characters that bash ignores at both ends of a command.
const blanks = " \t\n"

// denySeparators split a command into the parts that a deny rule also
// checks. They are the control characters of an allow check plus '(' and
// ')', so that "$(rm -rf /)" and "(rm -rf /)" give the part "rm -rf /".
const denySeparators = ";&|`<>()\n"

func parseCommandPattern(pattern string) (commandPattern, error) {
	trimmed := strings.Trim(pattern, blanks)
	if trimmed == "" {
		return "", errors.New("pattern is empty")
	}
	return commandPattern(trimmed), nil
}

func decodeCommand(input json.RawMessage) (string, bool) {
	var arguments struct {
		Command *string `json:"command"`
	}
	if err := json.Unmarshal(input, &arguments); err != nil || arguments.Command == nil || *arguments.Command == "" {
		return "", false
	}
	return strings.Trim(*arguments.Command, blanks), true
}

// allows does not let '*' match shell control syntax: ';' '&' '|' '`' '$('
// '>' '<' and newline. A here-doc starts with "<<", so it is control syntax
// too. Each of these in the command must match the same syntax, written at
// the same place in the pattern. Thus "go test *" does not allow
// "go test ./... && rm -rf /": a rule for one program must not let a second
// program run. The pattern "* | head" allows "ls | head", but not
// "ls | sh | head".
//
// A '*' still matches any argument, so a rule such as "git *" trusts every
// option of git.
func (pattern commandPattern) allows(command string) bool {
	rest := string(pattern)
	for {
		patternIndex, patternLength := nextControl(rest)
		commandIndex, commandLength := nextControl(command)
		if patternIndex < 0 || commandIndex < 0 {
			return patternIndex < 0 && commandIndex < 0 && matchWildcard(rest, command)
		}
		if rest[patternIndex:patternIndex+patternLength] != command[commandIndex:commandIndex+commandLength] ||
			!matchWildcard(rest[:patternIndex], command[:commandIndex]) {
			return false
		}
		rest = rest[patternIndex+patternLength:]
		command = command[commandIndex+commandLength:]
	}
}

// nextControl returns the position and length of the first control syntax
// in text, or -1 when there is none.
func nextControl(text string) (int, int) {
	for i := range len(text) {
		switch text[i] {
		case ';', '&', '|', '`', '>', '<', '\n':
			return i, 1
		case '$':
			if i+1 < len(text) && text[i+1] == '(' {
				return i, 2
			}
		}
	}
	return -1, 0
}

// denies checks the whole command and each part between control
// characters, so "rm -rf *" also denies "echo hi; rm -rf /". A deny rule is
// a guard against mistakes, not a sandbox: quotes, variables or other
// programs can still hide a command.
func (pattern commandPattern) denies(command string) bool {
	if matchWildcard(string(pattern), command) {
		return true
	}
	rest := command
	for {
		end := strings.IndexAny(rest, denySeparators)
		if end < 0 {
			// A command without separators was checked above as a whole.
			return rest != command && matchWildcard(string(pattern), strings.Trim(rest, blanks))
		}
		if matchWildcard(string(pattern), strings.Trim(rest[:end], blanks)) {
			return true
		}
		rest = rest[end+1:]
	}
}
