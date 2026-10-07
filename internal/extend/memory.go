package extend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const (
	memoryFileName = "memory.md"
	// memoryLoadLimit bounds the memory in the system prompt. The newest
	// facts are at the end, so a long file keeps its end.
	memoryLoadLimit = 16 << 10
	// memoryFactLimit bounds one fact, so the file stays a list of facts.
	memoryFactLimit = 500
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
		omitted = "(Older facts are omitted; the file is longer than 16 KiB.)\n"
	}
	return "Project memory from " + filepath.Join(projectDirName, memoryFileName) +
		". These facts were saved in earlier sessions; check a fact before you depend on it.\n" + omitted + text, nil
}

// MemoryTool is the remember tool. It adds one fact to the project memory,
// which loads at the start of the next session. It changes only that file.
func MemoryTool(workDir string) tools.Tool {
	return tools.Tool{
		Name: "remember",
		Description: "Save one durable fact about this project for later sessions, for example a build command, a convention, or a cause of a bug that was hard to find. " +
			"Do not save secrets or facts about this task only. The fact must be one line of at most 500 characters. Saved facts load at the start of the next session, not this one.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string","description":"One line, at most 500 characters."}},"required":["fact"],"additionalProperties":false}`),
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
			if err := appendFact(workDir, fact); err != nil {
				return tools.Result{Content: err.Error(), IsError: true}, nil
			}
			return tools.Result{Content: "Saved to " + filepath.Join(projectDirName, memoryFileName) + ". It loads in the next session."}, nil
		},
	}
}

func appendFact(workDir, fact string) error {
	path := MemoryPath(workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	// A file that does not end with a new line gets one first, so each fact
	// stays on its own line.
	prefix := ""
	info, err := file.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("inspect %s: %w", path, err), file.Close())
	}
	if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := file.ReadAt(last, info.Size()-1); err != nil {
			return errors.Join(fmt.Errorf("read %s: %w", path, err), file.Close())
		}
		if last[0] != '\n' {
			prefix = "\n"
		}
	}
	_, writeErr := file.WriteString(prefix + "- " + fact + "\n")
	return errors.Join(writeErr, file.Close())
}
