package extend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const (
	hookTimeout = 30 * time.Second
	// hookStderrLimit caps a deny reason and the stderr in a hook error.
	hookStderrLimit = 4 << 10
	// hookOutputLimit caps the context that one prompt hook can add.
	hookOutputLimit = 256 << 10
	// denyExitCode is the exit code with which a before_tool hook denies a
	// tool call. All other non-zero codes mean that the hook is broken.
	denyExitCode = 2
)

// ToolEvent is one tool call that the hooks see.
type ToolEvent struct {
	Tool  string
	Input json.RawMessage
	// Result is nil before the tool runs.
	Result *tools.Result
}

// Decision is the verdict of the before_tool hooks.
type Decision struct {
	Deny   bool
	Reason string
}

type toolPayload struct {
	Event  string          `json:"event"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	Result *resultPayload  `json:"result,omitempty"`
}

type resultPayload struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

type promptPayload struct {
	Event  string `json:"event"`
	Prompt string `json:"prompt"`
}

func (h Hook) matches(tool string) bool {
	return len(h.Tools) == 0 || slices.Contains(h.Tools, tool)
}

// RunBeforeTool runs the matching before_tool hooks in order. Exit code 0
// continues; exit code 2 denies the call with stderr as the reason. Any other
// result is an error, because a broken hook must not silently allow or deny.
func (h Hooks) RunBeforeTool(ctx context.Context, workDir string, event ToolEvent) (Decision, error) {
	return h.beforeTool(ctx, workDir, event, hookTimeout)
}

func (h Hooks) beforeTool(ctx context.Context, workDir string, event ToolEvent, timeout time.Duration) (Decision, error) {
	var payload []byte
	for _, hook := range h.BeforeTool {
		if !hook.matches(event.Tool) {
			continue
		}
		if payload == nil {
			encoded, err := encodeToolPayload("before_tool", event, nil)
			if err != nil {
				return Decision{}, err
			}
			payload = encoded
		}
		stderr := &cappedBuffer{limit: hookStderrLimit}
		status, err := runShell(ctx, workDir, hook.Command, payload, nil, stderr, timeout)
		if err != nil {
			return Decision{}, fmt.Errorf("extend: before_tool hook %q: %w", hook.Command, err)
		}
		switch {
		case status.timedOut || (status.code != 0 && status.code != denyExitCode):
			return Decision{}, hookFailure("before_tool", hook.Command, status, timeout, stderr)
		case status.code == denyExitCode:
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = fmt.Sprintf("denied by before_tool hook %q", hook.Command)
			}
			return Decision{Deny: true, Reason: reason}, nil
		case status.code == 0:
			continue
		}
	}
	return Decision{}, nil
}

// RunAfterTool runs the matching after_tool hooks in order and returns their
// output for the model: the trimmed stdout of each hook, joined by newlines,
// so a hook that runs a formatter or a linter can report problems. Empty
// outputs are skipped. A non-zero exit or a timeout is an error.
// event.Result must not be nil.
func (h Hooks) RunAfterTool(ctx context.Context, workDir string, event ToolEvent) (string, error) {
	return h.afterTool(ctx, workDir, event, hookTimeout)
}

func (h Hooks) afterTool(ctx context.Context, workDir string, event ToolEvent, timeout time.Duration) (string, error) {
	if event.Result == nil {
		panic("extend: RunAfterTool needs the tool result")
	}
	var payload []byte
	var outputs []string
	for _, hook := range h.AfterTool {
		if !hook.matches(event.Tool) {
			continue
		}
		if payload == nil {
			encoded, err := encodeToolPayload("after_tool", event, &resultPayload{Content: event.Result.Content, IsError: event.Result.IsError})
			if err != nil {
				return "", err
			}
			payload = encoded
		}
		stdout := &cappedBuffer{limit: hookOutputLimit}
		stderr := &cappedBuffer{limit: hookStderrLimit}
		status, err := runShell(ctx, workDir, hook.Command, payload, stdout, stderr, timeout)
		if err != nil {
			return "", fmt.Errorf("extend: after_tool hook %q: %w", hook.Command, err)
		}
		if status.timedOut || status.code != 0 {
			return "", hookFailure("after_tool", hook.Command, status, timeout, stderr)
		}
		if output := strings.TrimSpace(stdout.String()); output != "" {
			outputs = append(outputs, output)
		}
	}
	return strings.Join(outputs, "\n"), nil
}

// RunPrompt runs every prompt hook in order and returns extra context for the
// prompt: the trimmed stdout of each hook, joined by newlines. Empty outputs
// are skipped. A non-zero exit or a timeout is an error.
func (h Hooks) RunPrompt(ctx context.Context, workDir string, prompt string) (string, error) {
	return h.prompt(ctx, workDir, prompt, hookTimeout)
}

func (h Hooks) prompt(ctx context.Context, workDir string, prompt string, timeout time.Duration) (string, error) {
	if len(h.Prompt) == 0 {
		return "", nil
	}
	payload, err := json.Marshal(promptPayload{Event: "prompt", Prompt: prompt})
	if err != nil {
		return "", fmt.Errorf("extend: encode prompt event: %w", err)
	}
	var outputs []string
	for _, hook := range h.Prompt {
		stdout := &cappedBuffer{limit: hookOutputLimit}
		stderr := &cappedBuffer{limit: hookStderrLimit}
		status, err := runShell(ctx, workDir, hook.Command, payload, stdout, stderr, timeout)
		if err != nil {
			return "", fmt.Errorf("extend: prompt hook %q: %w", hook.Command, err)
		}
		if status.timedOut || status.code != 0 {
			return "", hookFailure("prompt", hook.Command, status, timeout, stderr)
		}
		if output := strings.TrimSpace(stdout.String()); output != "" {
			outputs = append(outputs, output)
		}
	}
	return strings.Join(outputs, "\n"), nil
}

func encodeToolPayload(event string, toolEvent ToolEvent, result *resultPayload) ([]byte, error) {
	input := toolEvent.Input
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	payload, err := json.Marshal(toolPayload{Event: event, Tool: toolEvent.Tool, Input: input, Result: result})
	if err != nil {
		return nil, fmt.Errorf("extend: encode %s event for %s: %w", event, toolEvent.Tool, err)
	}
	return payload, nil
}

func hookFailure(event, command string, status exitStatus, timeout time.Duration, stderr *cappedBuffer) error {
	message := fmt.Sprintf("extend: %s hook %q failed: %s", event, command, status.describe(timeout))
	if detail := strings.TrimSpace(stderr.String()); detail != "" {
		message += ": " + detail
	}
	return errors.New(message)
}
