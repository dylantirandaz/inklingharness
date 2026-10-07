package extend

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const frontMatterDelimiter = "---"

// readFrontMatter reads "key: value" lines between two --- lines. It reads
// no further than the line after the closing ---, so the caller can stop
// there or read the body from the same reader.
func readFrontMatter(reader *bufio.Reader, allowedKeys ...string) (map[string]string, error) {
	opening, err := readLine(reader)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if opening != frontMatterDelimiter {
		return nil, errors.New("front matter does not start with ---")
	}
	fields := map[string]string{}
	for {
		line, err := readLine(reader)
		if errors.Is(err, io.EOF) {
			return nil, errors.New("front matter has no closing ---")
		}
		if err != nil {
			return nil, err
		}
		if line == frontMatterDelimiter {
			return fields, nil
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("front matter line %q is not \"key: value\"", line)
		}
		key = strings.TrimSpace(key)
		if !slices.Contains(allowedKeys, key) {
			return nil, fmt.Errorf("unknown front matter key %q (allowed: %s)", key, strings.Join(allowedKeys, ", "))
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("front matter key %q occurs two times", key)
		}
		value = unquote(strings.TrimSpace(value))
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("front matter key %q is not valid UTF-8", key)
		}
		fields[key] = value
	}
}

// readLine returns io.EOF only when no text is left, so a last line without
// a newline still counts.
func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if errors.Is(err, io.EOF) && line != "" {
		err = nil
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), err
}

// unquote removes one pair of matching quotes, because many skill files
// quote their descriptions.
func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if first == last && (first == '"' || first == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
