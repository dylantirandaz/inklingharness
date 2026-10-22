package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// maxBatchFiles is how many files or ranges one read_files call reads.
const maxBatchFiles = 8

// readFilesTool reads several files or line ranges in one call. Inkling Small
// makes one tool call in a message, and on SWE-bench 44% of the turns of Think
// were a read that followed a read, so a call that reads several files saves
// turns. Each file is read as read_file reads it. A file that cannot be read
// gives its error in its section and does not stop the others.
func readFilesTool(root string, limits Limits) Tool {
	return Tool{
		Name:        "read_files",
		Description: fmt.Sprintf("Read up to %d files or line ranges in one call. Each item has path, and optional offset (1-based line) and limit (default %d lines). Each file is shown under a === path === line; a file that cannot be read shows its error and the others are still read.", maxBatchFiles, limits.ReadLines),
		InputSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"files":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"]}}},"required":["files"]}`, maxBatchFiles)),
		ReadOnly:    true,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			var arguments struct {
				Files []struct {
					Path   string `json:"path"`
					Offset *int   `json:"offset"`
					Limit  *int   `json:"limit"`
				} `json:"files"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if len(arguments.Files) == 0 || len(arguments.Files) > maxBatchFiles {
				return invalidInput(fmt.Errorf("files must hold 1 to %d items", maxBatchFiles)), nil
			}
			type item struct {
				path          string
				offset, limit int
			}
			items := make([]item, 0, len(arguments.Files))
			for _, file := range arguments.Files {
				offset, limit := 1, limits.ReadLines
				if file.Offset != nil {
					offset = *file.Offset
				}
				if file.Limit != nil {
					limit = *file.Limit
				}
				if file.Path == "" {
					return invalidInput(errors.New("every item needs a path")), nil
				}
				if offset < 1 || limit < 1 {
					return invalidInput(fmt.Errorf("offset and limit of %s must be positive", file.Path)), nil
				}
				items = append(items, item{file.Path, offset, limit})
			}
			var output strings.Builder
			failed := 0
			for index, file := range items {
				result, err := readLines(ctx, resolvePath(root, file.path), file.offset, file.limit)
				if err != nil {
					return Result{}, err
				}
				section := fmt.Sprintf("=== %s ===\n%s\n", file.path, result.Content)
				// The first file always comes back, as read_file would return it.
				if index > 0 && output.Len()+len(section) > maxReadBytes {
					fmt.Fprintf(&output, "[not read: %d more files would pass the 256 KiB output limit; read them in another call]", len(items)-index)
					break
				}
				if result.IsError {
					failed++
				}
				output.WriteString(section)
			}
			return Result{Content: strings.TrimSuffix(output.String(), "\n"), IsError: failed == len(items)}, nil
		},
	}
}
