package extend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const (
	toolsDirName       = "tools"
	defaultToolTimeout = 60 * time.Second
	maxToolTimeout     = 600 * time.Second
	// toolOutputLimit is the same cap that the read-type built-in tools use.
	toolOutputLimit = 256 << 10
)

// reservedToolNames are the names of built-in and MCP tools. A custom tool
// cannot replace them, because the model would get a different tool than the
// one that the system prompt describes.
var reservedToolNames = []string{
	"read_file", "write_file", "edit_file", "bash", "bash_job", "glob", "grep",
	"todo_write", "task", "mcp_list", "mcp_call", "web_fetch", "remember", "ask_user",
}

type toolManifest struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	InputSchema    json.RawMessage `json:"input_schema"`
	Command        string          `json:"command"`
	ReadOnly       bool            `json:"read_only"`
	TimeoutSeconds int             `json:"timeout_seconds"`
}

// LoadTools reads tools/*.json manifests from the user directory and from
// <workDir>/.inkling. Each tool runs its command in workDir. The result is
// sorted by name.
func LoadTools(workDir string) ([]tools.Tool, error) {
	userDir, err := UserDir()
	if err != nil {
		return nil, err
	}
	return loadTools(userDir, workDir)
}

func loadTools(userDir, workDir string) ([]tools.Tool, error) {
	byName := map[string]tools.Tool{}
	for _, dir := range layerDirs(userDir, workDir) {
		toolsDir := filepath.Join(dir, toolsDirName)
		entries, err := readDirIfExists(toolsDir)
		if err != nil {
			return nil, err
		}
		sources := map[string]string{}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(toolsDir, entry.Name())
			tool, err := readToolManifest(path, workDir)
			if err != nil {
				return nil, err
			}
			if earlier, taken := sources[tool.Name]; taken {
				return nil, fmt.Errorf("extend: %s: tool name %q is also in %s", path, tool.Name, earlier)
			}
			sources[tool.Name] = path
			byName[tool.Name] = tool
		}
	}
	loaded := make([]tools.Tool, 0, len(byName))
	for _, tool := range byName {
		loaded = append(loaded, tool)
	}
	slices.SortFunc(loaded, func(left, right tools.Tool) int {
		return strings.Compare(left.Name, right.Name)
	})
	return loaded, nil
}

func readToolManifest(path, workDir string) (tools.Tool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("extend: read %s: %w", path, err)
	}
	var manifest toolManifest
	if err := decodeStrict(path, content, &manifest); err != nil {
		return tools.Tool{}, err
	}
	switch {
	case !isValidToolName(manifest.Name):
		return tools.Tool{}, fmt.Errorf("extend: %s: tool name %q must match [a-z][a-z0-9_]*", path, manifest.Name)
	case slices.Contains(reservedToolNames, manifest.Name):
		return tools.Tool{}, fmt.Errorf("extend: %s: tool name %q is reserved for a built-in tool", path, manifest.Name)
	case strings.TrimSpace(manifest.Description) == "":
		return tools.Tool{}, fmt.Errorf("extend: %s: description is empty", path)
	case strings.TrimSpace(manifest.Command) == "":
		return tools.Tool{}, fmt.Errorf("extend: %s: command is empty", path)
	case manifest.TimeoutSeconds < 0 || time.Duration(manifest.TimeoutSeconds)*time.Second > maxToolTimeout:
		return tools.Tool{}, fmt.Errorf("extend: %s: timeout_seconds must be from 1 to %d", path, int(maxToolTimeout.Seconds()))
	}
	if err := checkObjectSchema(manifest.InputSchema); err != nil {
		return tools.Tool{}, fmt.Errorf("extend: %s: input_schema: %w", path, err)
	}
	timeout := defaultToolTimeout
	if manifest.TimeoutSeconds > 0 {
		timeout = time.Duration(manifest.TimeoutSeconds) * time.Second
	}
	command := manifest.Command
	return tools.Tool{
		Name:        manifest.Name,
		Description: manifest.Description,
		InputSchema: manifest.InputSchema,
		ReadOnly:    manifest.ReadOnly,
		Run: func(ctx context.Context, input json.RawMessage) (tools.Result, error) {
			return runCustomTool(ctx, workDir, command, input, timeout)
		},
	}, nil
}

// isValidToolName accepts [a-z][a-z0-9_]*, the shape of the built-in names.
func isValidToolName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for index := 1; index < len(name); index++ {
		if character := name[index]; !isLowerAlphanumeric(character) && character != '_' {
			return false
		}
	}
	return true
}

// checkObjectSchema requires a JSON object with "type": "object", because
// the model API accepts only object schemas for tool input.
func checkObjectSchema(schema json.RawMessage) error {
	if len(schema) == 0 {
		return errors.New("is missing")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil || fields == nil {
		return errors.New("is not a JSON object")
	}
	var schemaType string
	if err := json.Unmarshal(fields["type"], &schemaType); err != nil || schemaType != "object" {
		return errors.New(`must have "type": "object"`)
	}
	return nil
}

func runCustomTool(ctx context.Context, workDir, command string, input json.RawMessage, timeout time.Duration) (tools.Result, error) {
	output := &cappedBuffer{limit: toolOutputLimit}
	status, err := runShell(ctx, workDir, command, input, output, output, timeout)
	if err != nil {
		return tools.Result{}, err
	}
	content := output.String()
	if output.truncated() {
		content += fmt.Sprintf("\n[output truncated: showing the first %d of %d bytes]", len(output.content), output.total)
	}
	if content == "" {
		content = "(no output)"
	}
	if status.timedOut || status.code != 0 {
		return tools.Result{Content: content + "\n[" + status.describe(timeout) + "]", IsError: true}, nil
	}
	return tools.Result{Content: content}, nil
}
