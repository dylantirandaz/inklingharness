package permission

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pathPattern is a slash-separated path glob. In a segment, '*' matches any
// run of characters, also a leading dot, and '?' matches one character. A
// "**" segment matches any number of segments, also none. Every other
// character matches only itself.
//
// A relative pattern matches paths in root. An absolute pattern matches the
// absolute path; in an allow rule the path must also be in root.
type pathPattern struct {
	absolute bool
	segments []string
}

// target is a file path from a tool input after lexical cleaning.
type target struct {
	absolute []string
	// relative is valid only when inside is true. It is empty for root.
	relative []string
	inside   bool
}

func parsePathPattern(pattern string) (pathPattern, error) {
	absolute := filepath.IsAbs(pattern)
	slashed := filepath.ToSlash(pattern)
	if absolute {
		slashed = strings.TrimPrefix(slashed, "/")
	}
	segments := strings.Split(slashed, "/")
	for _, segment := range segments {
		switch {
		case segment == "":
			return pathPattern{}, errors.New("path pattern has an empty segment")
		case segment == "." || segment == "..":
			return pathPattern{}, errors.New("path pattern cannot contain . or .. segments")
		case segment != "**" && strings.Contains(segment, "**"):
			return pathPattern{}, errors.New("** must be a complete path segment")
		}
	}
	return pathPattern{absolute: absolute, segments: segments}, nil
}

func decodeFile(root string, input json.RawMessage) (target, bool) {
	var arguments struct {
		Path *string `json:"path"`
	}
	if err := json.Unmarshal(input, &arguments); err != nil || arguments.Path == nil || *arguments.Path == "" {
		return target{}, false
	}
	return locate(root, *arguments.Path), true
}

// locate cleans the path lexically, the same way the file tools join it to
// root, so ".." cannot hide that the path leaves root.
func locate(root, name string) target {
	var full string
	if filepath.IsAbs(name) {
		full = filepath.Clean(name)
	} else {
		full = filepath.Join(root, name)
	}
	located := target{absolute: splitSlashed(strings.TrimPrefix(filepath.ToSlash(full), "/"))}
	relative, err := filepath.Rel(root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return located
	}
	located.inside = true
	if relative != "." {
		located.relative = splitSlashed(filepath.ToSlash(relative))
	}
	return located
}

func splitSlashed(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func (pattern pathPattern) allows(file target) bool {
	if !file.inside {
		return false
	}
	if pattern.absolute {
		return matchSegments(pattern.segments, file.absolute, false)
	}
	return matchSegments(pattern.segments, file.relative, false)
}

// denies ignores letter case. The file systems of macOS and Windows usually
// ignore it too, so ".ENV" can be the same file as ".env".
func (pattern pathPattern) denies(file target) bool {
	if pattern.absolute {
		return matchSegments(pattern.segments, file.absolute, true)
	}
	return file.inside && matchSegments(pattern.segments, file.relative, true)
}

func matchSegments(pattern, names []string, ignoreCase bool) bool {
	patternIndex, nameIndex := 0, 0
	star, resume := -1, 0
	for nameIndex < len(names) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == "**":
			star, resume = patternIndex, nameIndex
			patternIndex++
		case patternIndex < len(pattern) && matchSegment(pattern[patternIndex], names[nameIndex], ignoreCase):
			patternIndex++
			nameIndex++
		case star >= 0:
			// Let the last "**" take one more segment, then try again.
			resume++
			patternIndex, nameIndex = star+1, resume
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == "**" {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

func matchSegment(pattern, name string, ignoreCase bool) bool {
	patternIndex, nameIndex := 0, 0
	star, resume := -1, 0
	for nameIndex < len(name) {
		if patternIndex < len(pattern) && pattern[patternIndex] == '*' {
			star, resume = patternIndex, nameIndex
			patternIndex++
			continue
		}
		nameRune, nameSize := utf8.DecodeRuneInString(name[nameIndex:])
		if patternIndex < len(pattern) {
			patternRune, patternSize := utf8.DecodeRuneInString(pattern[patternIndex:])
			if patternRune == '?' || sameRune(patternRune, nameRune, ignoreCase) {
				patternIndex += patternSize
				nameIndex += nameSize
				continue
			}
		}
		if star < 0 {
			return false
		}
		// Let the last star take one more character, then try again.
		_, skipped := utf8.DecodeRuneInString(name[resume:])
		resume += skipped
		patternIndex, nameIndex = star+1, resume
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

func sameRune(first, second rune, ignoreCase bool) bool {
	if first == second {
		return true
	}
	if !ignoreCase {
		return false
	}
	// SimpleFold steps through all case forms of a rune and then returns to it.
	for folded := unicode.SimpleFold(first); folded != first; folded = unicode.SimpleFold(folded) {
		if folded == second {
			return true
		}
	}
	return false
}
