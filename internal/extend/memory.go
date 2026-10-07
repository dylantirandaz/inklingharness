package extend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const (
	memoryFileName = "memory.md"
	// memoryLoadLimit bounds the memory in the system prompt, about 1,000
	// tokens, because every request repeats it. The newest facts are at the
	// end, so a long file keeps its end.
	memoryLoadLimit = 4 << 10
	// memoryFactLimit bounds one fact, so the file stays a list of facts.
	memoryFactLimit = 300
)

// MemoryPath is the project memory file: .inkling/memory.md in workDir.
func MemoryPath(workDir string) string {
	return filepath.Join(workDir, projectDirName, memoryFileName)
}

// LoadMemory returns the project memory for the system prompt, or "" when
// there is no memory file. The text is read once for each session, so that
// the system prompt and its cached prefix stay the same during the session.
func LoadMemory(workDir string) (string, error) {
	data, err := os.ReadFile(MemoryPath(workDir))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("extend: read project memory: %w", err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", nil
	}
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("extend: %s is not UTF-8 text", MemoryPath(workDir))
	}
	omitted := ""
	if len(text) > memoryLoadLimit {
		text = text[len(text)-memoryLoadLimit:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		}
		omitted = "(Older facts omitted.)\n"
	}
	return "Project memory from earlier sessions; check a fact before you depend on it:\n" + omitted + text, nil
}

// MemoryTool is the remember tool. It adds one fact to the project memory,
// which loads at the start of the next session. It changes only that file.
func MemoryTool(workDir string) tools.Tool {
	return tools.Tool{
		Name:        "remember",
		Description: "Save one durable project fact for later sessions: a build or test command, the layout, a convention, or the cause of a hard bug. One line. No secrets. Call it together with other tools, not in a turn of its own.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"}},"required":["fact"],"additionalProperties":false}`),
		Run: func(ctx context.Context, input json.RawMessage) (tools.Result, error) {
			var arguments struct {
				Fact *string `json:"fact"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil || arguments.Fact == nil {
				return tools.Result{Content: "invalid input: fact is required and must be a string", IsError: true}, nil
			}
			fact := strings.TrimSpace(*arguments.Fact)
			switch {
			case fact == "":
				return tools.Result{Content: "invalid input: fact is empty", IsError: true}, nil
			case strings.ContainsAny(fact, "\r\n"):
				return tools.Result{Content: "invalid input: a fact must be one line", IsError: true}, nil
			case utf8.RuneCountInString(fact) > memoryFactLimit:
				return tools.Result{Content: fmt.Sprintf("invalid input: a fact must be at most %d characters", memoryFactLimit), IsError: true}, nil
			}
			if err := ctx.Err(); err != nil {
				return tools.Result{}, err
			}
			added, err := appendFact(workDir, fact)
			if err != nil {
				return tools.Result{Content: err.Error(), IsError: true}, nil
			}
			if !added {
				return tools.Result{Content: "Already saved."}, nil
			}
			return tools.Result{Content: "Saved for later sessions."}, nil
		},
	}
}

// appendFact adds fact as a new line. It returns false, and writes nothing,
// when the file already has the same fact apart from case and spacing, so a
// repeated save does not make every later request longer.
func appendFact(workDir, fact string) (bool, error) {
	path := MemoryPath(workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	existing, err := io.ReadAll(file)
	if err != nil {
		return false, errors.Join(fmt.Errorf("read %s: %w", path, err), file.Close())
	}
	key := factKey(fact)
	for line := range strings.Lines(string(existing)) {
		if factKey(strings.TrimPrefix(strings.TrimSpace(line), "- ")) == key {
			return false, file.Close()
		}
	}
	// A file that does not end with a new line gets one first, so each fact
	// stays on its own line.
	prefix := ""
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		prefix = "\n"
	}
	_, writeErr := file.WriteString(prefix + "- " + fact + "\n")
	return true, errors.Join(writeErr, file.Close())
}

// factKey is the form in which two facts compare as the same.
func factKey(fact string) string {
	return strings.ToLower(strings.Join(strings.Fields(fact), " "))
}
