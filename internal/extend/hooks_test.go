package extend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func readText(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestBeforeToolHooksAllowInOrderAndSendTheEvent(t *testing.T) {
	workDir := t.TempDir()
	script := filepath.Join(workDir, "record.sh")
	writeFile(t, script, "#!/bin/bash\ncat > event.json\necho first >> order.txt\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := Hooks{BeforeTool: []Hook{
		{Command: "./record.sh"},
		{Command: "echo bash-only >> order.txt", Tools: []string{"bash"}},
		{Command: "echo edit >> order.txt", Tools: []string{"write_file", "edit_file"}},
	}}
	decision, err := hooks.RunBeforeTool(context.Background(), workDir, ToolEvent{Tool: "edit_file", Input: json.RawMessage(`{"path":"a.go"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if decision != (Decision{}) {
		t.Fatalf("decision = %+v, want allow", decision)
	}
	if order := readText(t, filepath.Join(workDir, "order.txt")); order != "first\nedit\n" {
		t.Fatalf("order = %q, want first then the edit hook, without the bash hook", order)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(workDir, "event.json"))), &event); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"event": "before_tool", "tool": "edit_file", "input": map[string]any{"path": "a.go"}}
	if !reflect.DeepEqual(event, want) {
		t.Fatalf("event = %v, want %v", event, want)
	}
}

func TestBeforeToolHookOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		hooks      []Hook
		tool       string
		want       Decision
		wantError  string
		wantNoFile bool
	}{
		{
			name:  "deny with stderr reason stops later hooks",
			hooks: []Hook{{Command: "echo '  no rm here  ' >&2; exit 2"}, {Command: "touch later"}},
			tool:  "bash",
			want:  Decision{Deny: true, Reason: "no rm here"},
		},
		{
			name:  "deny without stderr names the hook",
			hooks: []Hook{{Command: "exit 2"}, {Command: "touch later"}},
			tool:  "bash",
			want:  Decision{Deny: true, Reason: `denied by before_tool hook "exit 2"`},
		},
		{
			name:      "other exit code is an error",
			hooks:     []Hook{{Command: "echo broken >&2; exit 1"}, {Command: "touch later"}},
			tool:      "bash",
			wantError: `before_tool hook "echo broken >&2; exit 1" failed: exit code 1: broken`,
		},
		{
			name:      "killed by a signal is an error",
			hooks:     []Hook{{Command: "kill -9 $$"}, {Command: "touch later"}},
			tool:      "bash",
			wantError: "killed by a signal",
		},
		{
			name:  "deny hook for another tool does not run",
			hooks: []Hook{{Command: "exit 2", Tools: []string{"bash"}}},
			tool:  "read_file",
			want:  Decision{},
		},
		{
			name:  "deny hook matches its tool",
			hooks: []Hook{{Command: "echo blocked >&2; exit 2", Tools: []string{"grep", "bash"}}},
			tool:  "bash",
			want:  Decision{Deny: true, Reason: "blocked"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			decision, err := Hooks{BeforeTool: test.hooks}.RunBeforeTool(context.Background(), workDir, ToolEvent{Tool: test.tool, Input: json.RawMessage(`{}`)})
			if test.wantError != "" {
				wantErrorContaining(t, err, test.wantError)
			} else if err != nil {
				t.Fatal(err)
			}
			if decision != test.want {
				t.Fatalf("decision = %+v, want %+v", decision, test.want)
			}
			if _, err := os.Stat(filepath.Join(workDir, "later")); err == nil {
				t.Fatal("a hook after the deciding hook ran")
			}
		})
	}
}

func TestBeforeToolDenyReasonIsCapped(t *testing.T) {
	decision, err := Hooks{BeforeTool: []Hook{{Command: "head -c 10000 /dev/zero | tr '\\0' x >&2; exit 2"}}}.
		RunBeforeTool(context.Background(), t.TempDir(), ToolEvent{Tool: "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Deny || len(decision.Reason) != hookStderrLimit {
		t.Fatalf("deny = %v, reason length = %d, want %d", decision.Deny, len(decision.Reason), hookStderrLimit)
	}
}

func TestHookTimeoutIsAnErrorAndKillsProcessGroup(t *testing.T) {
	const slowHook = "sleep 30 & echo $! > child.pid; wait"
	tests := []struct {
		name string
		run  func(workDir string) error
	}{
		{"before_tool", func(workDir string) error {
			_, err := Hooks{BeforeTool: []Hook{{Command: slowHook}}}.beforeTool(context.Background(), workDir, ToolEvent{Tool: "bash"}, time.Second)
			return err
		}},
		{"after_tool", func(workDir string) error {
			_, err := Hooks{AfterTool: []Hook{{Command: slowHook}}}.afterTool(context.Background(), workDir, ToolEvent{Tool: "bash", Result: &tools.Result{}}, time.Second)
			return err
		}},
		{"prompt", func(workDir string) error {
			_, err := Hooks{Prompt: []Hook{{Command: slowHook}}}.prompt(context.Background(), workDir, "hello", time.Second)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			started := time.Now()
			err := test.run(workDir)
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("hook returned after %s", elapsed)
			}
			wantErrorContaining(t, err, test.name+" hook", "timed out after 1s")
			childPID, err := waitForPIDFile(filepath.Join(workDir, "child.pid"))
			if err != nil {
				t.Fatal(err)
			}
			waitForProcessExit(t, childPID)
		})
	}
}

func TestAfterToolHooks(t *testing.T) {
	workDir := t.TempDir()
	hooks := Hooks{AfterTool: []Hook{
		{Command: "cat > event.json; echo '  vet: sum.go:3: unused x  '"},
		{Command: "true"},
		{Command: "touch grep-ran; echo grep", Tools: []string{"grep"}},
		{Command: "echo 'fmt: ok'; echo ignored >&2"},
	}}
	event := ToolEvent{Tool: "bash", Input: json.RawMessage(`{"command":"ls"}`), Result: &tools.Result{Content: "a\nb", IsError: true}}
	output, err := hooks.RunAfterTool(context.Background(), workDir, event)
	if err != nil {
		t.Fatal(err)
	}
	if output != "vet: sum.go:3: unused x\nfmt: ok" {
		t.Fatalf("output = %q", output)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(workDir, "event.json"))), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"event":  "after_tool",
		"tool":   "bash",
		"input":  map[string]any{"command": "ls"},
		"result": map[string]any{"content": "a\nb", "is_error": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event = %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(workDir, "grep-ran")); err == nil {
		t.Fatal("the grep hook ran for bash")
	}

	failing := Hooks{AfterTool: []Hook{{Command: "echo bad >&2; exit 2"}}}
	output, err = failing.RunAfterTool(context.Background(), workDir, event)
	wantErrorContaining(t, err, "after_tool hook", "exit code 2", "bad")
	if output != "" {
		t.Fatalf("output = %q after a failure, want empty", output)
	}
}

func TestPromptHooksJoinOutput(t *testing.T) {
	workDir := t.TempDir()
	hooks := Hooks{Prompt: []Hook{
		{Command: "cat > event.json; printf '  branch: main \\n\\n'"},
		{Command: "true"},
		{Command: "echo '   '"},
		{Command: "echo 'tests: green'; echo ignored >&2"},
	}}
	extra, err := hooks.RunPrompt(context.Background(), workDir, "fix the bug")
	if err != nil {
		t.Fatal(err)
	}
	if extra != "branch: main\ntests: green" {
		t.Fatalf("extra = %q", extra)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(workDir, "event.json"))), &event); err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"event": "prompt", "prompt": "fix the bug"}); !reflect.DeepEqual(event, want) {
		t.Fatalf("event = %v, want %v", event, want)
	}

	failing := Hooks{Prompt: []Hook{{Command: "echo partial"}, {Command: "exit 4"}}}
	extra, err = failing.RunPrompt(context.Background(), workDir, "x")
	wantErrorContaining(t, err, `prompt hook "exit 4" failed: exit code 4`)
	if extra != "" {
		t.Fatalf("extra = %q after a failure, want empty", extra)
	}
}

func TestNoHooksRunNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	var hooks Hooks
	decision, err := hooks.RunBeforeTool(context.Background(), missing, ToolEvent{Tool: "bash"})
	if err != nil || decision != (Decision{}) {
		t.Fatalf("RunBeforeTool = %+v, %v", decision, err)
	}
	if output, err := hooks.RunAfterTool(context.Background(), missing, ToolEvent{Tool: "bash", Result: &tools.Result{}}); err != nil || output != "" {
		t.Fatal(err)
	}
	extra, err := hooks.RunPrompt(context.Background(), missing, "x")
	if err != nil || extra != "" {
		t.Fatalf("RunPrompt = %q, %v", extra, err)
	}
}
