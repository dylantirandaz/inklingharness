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

// Standard returns the coding tool set rooted at the given directory.
// Relative paths resolve against root. There is no sandbox: the tools run with
// the permissions of the user, on the user's machine, by design. The bash tool
// saves large outputs in outputDirectory. The directory must be absolute,
// because results give the saved path to the model, and a path in a result
// must not depend on the working directory.
func Standard(root, outputDirectory string) (*Set, error) {
	if !filepath.IsAbs(outputDirectory) {
		return nil, fmt.Errorf("tools: output directory %q is not absolute", outputDirectory)
	}
	return NewSet(readFileTool(root), writeFileTool(root), editFileTool(root), bashTool(root, outputDirectory),
		listDirTool(root), globTool(root), grepTool(root), todoWriteTool())
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
