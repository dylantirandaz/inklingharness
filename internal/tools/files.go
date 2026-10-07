package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// maxReadBytes caps one read_file result so a large file cannot fill the
// context window.
const maxReadBytes = 256 * 1024

func readFileTool(root string) Tool {
	return Tool{
		Name:        "read_file",
		Description: "Read a text file as numbered lines. Give path in every call. offset is 1-based; limit defaults to 2000 lines.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"]}`),
		ReadOnly:    true,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			arguments := struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}{Offset: 1, Limit: 2000}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Path == "" {
				return invalidInput(errors.New("path is required")), nil
			}
			if arguments.Offset < 1 || arguments.Limit < 1 {
				return invalidInput(errors.New("offset and limit must be positive")), nil
			}
			return readLines(ctx, resolvePath(root, arguments.Path), arguments.Offset, arguments.Limit)
		},
	}
}

func writeFileTool(root string) Tool {
	return Tool{
		Name:        "write_file",
		Description: "Create or replace a file. Missing parent directories are created.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			var arguments struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Path == "" {
				return invalidInput(errors.New("path is required")), nil
			}
			if arguments.Content == nil {
				return invalidInput(errors.New("content is required and must be a string")), nil
			}
			path := resolvePath(root, arguments.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fileFailure(err)
			}
			if err := os.WriteFile(path, []byte(*arguments.Content), 0o644); err != nil {
				return fileFailure(err)
			}
			return Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(*arguments.Content), arguments.Path)}, nil
		},
	}
}

func editFileTool(root string) Tool {
	return Tool{
		Name:        "edit_file",
		Description: "Replace the one exact occurrence of old_string with new_string. Give path, old_string, and new_string in every call. Fails if old_string is absent or not unique; include enough context to make it unique.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			var arguments struct {
				Path      string  `json:"path"`
				OldString string  `json:"old_string"`
				NewString *string `json:"new_string"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Path == "" {
				return invalidInput(errors.New("path is required")), nil
			}
			if arguments.OldString == "" {
				return invalidInput(errors.New("old_string must not be empty")), nil
			}
			if arguments.NewString == nil {
				return invalidInput(errors.New("new_string is required and must be a string")), nil
			}
			path := resolvePath(root, arguments.Path)
			info, err := os.Stat(path)
			if err != nil {
				return fileFailure(err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return fileFailure(err)
			}
			old := []byte(arguments.OldString)
			switch occurrences := bytes.Count(content, old); occurrences {
			case 0:
				return Result{Content: "old_string not found in " + arguments.Path, IsError: true}, nil
			case 1:
				updated := bytes.Replace(content, old, []byte(*arguments.NewString), 1)
				if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
					return fileFailure(err)
				}
				return Result{Content: "edited " + arguments.Path}, nil
			default:
				return Result{Content: fmt.Sprintf("old_string occurs %d times in %s; include more context to make it unique", occurrences, arguments.Path), IsError: true}, nil
			}
		},
	}
}

// fileFailure reports a file system error to the model. A missing file or a
// permission problem is expected and correctable, so it becomes a Result.
func fileFailure(err error) (Result, error) {
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		return Result{Content: err.Error(), IsError: true}, nil
	}
	return Result{}, err
}
