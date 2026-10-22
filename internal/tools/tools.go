// Package tools holds the tools that the model can call.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Result is what the model receives from one tool run. IsError marks an
// expected failure, for example a missing file, so the model can react.
type Result struct {
	Content string
	IsError bool
}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// ReadOnly tools change no files and run no commands, so a batch of them
	// can run in parallel.
	ReadOnly bool
	// Run returns an error only for runtime faults that the model cannot
	// correct. Expected failures go in Result.
	Run func(ctx context.Context, input json.RawMessage) (Result, error)
}

// Set is an ordered collection of tools with unique names. The order is
// stable across turns, which keeps the prompt prefix identical for caching.
type Set struct {
	ordered []Tool
	byName  map[string]Tool
}

// NewSet builds a set. Two tools with the same name are an error.
func NewSet(tools ...Tool) (*Set, error) {
	set := &Set{byName: make(map[string]Tool, len(tools))}
	for _, tool := range tools {
		if _, taken := set.byName[tool.Name]; taken {
			return nil, fmt.Errorf("tools: duplicate tool name %q", tool.Name)
		}
		set.byName[tool.Name] = tool
		set.ordered = append(set.ordered, tool)
	}
	return set, nil
}

// All returns the tools in registration order.
func (s *Set) All() []Tool {
	return s.ordered
}

// Lookup finds a tool by name.
func (s *Set) Lookup(name string) (Tool, bool) {
	tool, found := s.byName[name]
	return tool, found
}

// Limits are the sizes of the tool results that enter the model context.
type Limits struct {
	// ReadLines is the number of lines that read_file returns when the call
	// gives no limit.
	ReadLines int
	// OutputHeadBytes and OutputTailBytes are the start and the end of a
	// large bash output that stay in the result. The rest goes to a file.
	OutputHeadBytes int
	OutputTailBytes int
}

var standardLimits = Limits{ReadLines: 2000, OutputHeadBytes: 16 * 1024, OutputTailBytes: 16 * 1024}

// leanLimits keep less of a large result. Every later turn reads a result
// again from the prompt cache: on SWE-bench with Inkling Small, old tool
// output was 70% of the cached input of Think, and results over 8,000
// characters were 22% of it. The rest of a large result stays in a file or
// behind an offset.
var leanLimits = Limits{ReadLines: 400, OutputHeadBytes: 4 * 1024, OutputTailBytes: 4 * 1024}

// Standard returns the coding tool set rooted at the given directory.
// Relative paths resolve against root. There is no sandbox: the tools run with
// the permissions of the user, on the user's machine, by design. The bash tool
// saves large outputs in outputDirectory and starts background commands as
// jobs in jobs, which must keep their output in the same directory. The
// directory must be absolute, because results give the saved path to the
// model, and a path in a result must not depend on the working directory.
func Standard(root, outputDirectory string, jobs *Jobs) (*Set, error) {
	if err := checkOutputDirectory(outputDirectory, jobs); err != nil {
		return nil, err
	}
	return NewSet(readFileTool(root, standardLimits), writeFileTool(root), editFileTool(root), bashTool(root, jobs, standardLimits), bashJobTool(jobs),
		globTool(root), grepTool(root))
}

// Lean is Standard with read_files, which reads several files in one call,
// and with leanLimits. Inkling Small makes one tool call in a message, so a
// call that does more saves a turn.
func Lean(root, outputDirectory string, jobs *Jobs) (*Set, error) {
	if err := checkOutputDirectory(outputDirectory, jobs); err != nil {
		return nil, err
	}
	return NewSet(readFileTool(root, leanLimits), readFilesTool(root, leanLimits), writeFileTool(root), editFileTool(root), bashTool(root, jobs, leanLimits), bashJobTool(jobs),
		globTool(root), grepTool(root))
}

func checkOutputDirectory(outputDirectory string, jobs *Jobs) error {
	if !filepath.IsAbs(outputDirectory) {
		return fmt.Errorf("tools: output directory %q is not absolute", outputDirectory)
	}
	if jobs.outputDirectory != outputDirectory {
		return fmt.Errorf("tools: jobs keep output in %q, not in output directory %q", jobs.outputDirectory, outputDirectory)
	}
	return nil
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

// invalidInput reports a malformed tool input to the model as an expected
// failure, so it can send a corrected call.
func invalidInput(err error) Result {
	return Result{Content: "invalid input: " + err.Error(), IsError: true}
}
