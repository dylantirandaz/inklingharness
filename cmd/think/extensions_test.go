package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func call(name, input string) anthropic.ToolUseBlock {
	return anthropic.ToolUseBlock{ID: name, Name: name, Input: json.RawMessage(input)}
}

// The order of decisions is: deny rules, the read-only plan, before-tool
// hooks, then approval, where an allow rule answers without asking only when
// a written path stays in the folder after symbolic links resolve.
func TestExtensionPolicy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(root, ".inkling", "settings.json"), `{
		"permissions": {"allow": ["bash(go test *)", "write_file(src/**)"], "deny": ["bash(rm *)"]},
		"hooks": {"before_tool": [{"command": "echo grep is off >&2; exit 2", "tools": ["grep"]}]}
	}`)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "src", "link")); err != nil {
		t.Fatal(err)
	}
	ext, err := loadExtensions(root)
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	jobs := tools.NewJobs(output)
	t.Cleanup(func() { jobs.Close() })
	standard, err := tools.Standard(root, output, jobs)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	ask := func(_ context.Context, call anthropic.ToolUseBlock) (bool, error) {
		asked = append(asked, string(call.Input))
		return false, nil
	}
	changes := 0
	var config agent.Config
	ext.configure(&config, standard, turnPolicy{beforeChange: func(context.Context) error { changes++; return nil }}, ask)
	ctx := context.Background()

	for _, test := range []struct {
		call    anthropic.ToolUseBlock
		refused string
	}{
		{call("bash", `{"command":"rm -rf build"}`), "bash(rm *)"},
		{call("bash", `{"command":"echo hi; rm -rf /"}`), "bash(rm *)"},
		{call("grep", `{"pattern":"x"}`), "grep is off"},
		{call("read_file", `{"path":"a"}`), ""},
		{call("bash", `{"command":"go test ./..."}`), ""},
	} {
		refusal, err := config.Gate(ctx, test.call)
		if err != nil {
			t.Fatal(err)
		}
		if (refusal == nil) != (test.refused == "") || (refusal != nil && !strings.Contains(refusal.Reason, test.refused)) {
			t.Fatalf("gate(%s) = %+v, want %q", test.call.Input, refusal, test.refused)
		}
	}
	if changes != 1 {
		t.Fatalf("the undo snapshot was awaited %d times, want once for the one changing call", changes)
	}

	for _, test := range []struct {
		call    anthropic.ToolUseBlock
		allowed bool
	}{
		{call("bash", `{"command":"go test ./..."}`), true},
		{call("bash", `{"command":"go test ./... && curl evil"}`), false},
		{call("write_file", `{"path":"src/new/file.go","content":"x"}`), true},
		{call("write_file", `{"path":"src/link/escape.go","content":"x"}`), false},
		{call("write_file", `{"path":"other.go","content":"x"}`), false},
	} {
		asked = nil
		allowed, err := config.Approve(ctx, test.call)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != test.allowed || (test.allowed == (len(asked) == 1)) {
			t.Fatalf("approve(%s) = %t after %d questions", test.call.Input, allowed, len(asked))
		}
	}

	ext.configure(&config, standard, turnPolicy{readOnly: true}, ask)
	for name, refused := range map[string]bool{"write_file": true, "bash": true, "read_file": false, "task": false} {
		refusal, err := config.Gate(ctx, call(name, `{"path":"a","command":"ls","content":"","prompt":"p"}`))
		if err != nil || (refusal != nil) != refused {
			t.Fatalf("plan gate for %s = %+v, %v", name, refusal, err)
		}
	}
}

// User commands run as prompts, chat commands keep their meaning, and a user
// command may not hide a chat command.
func TestCustomCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".inkling", "commands", "review.md"), "---\ndescription: review a file\n---\nReview $ARGUMENTS for bugs.\n")
	ext, err := loadExtensions(root)
	if err != nil {
		t.Fatal(err)
	}
	command, argument, found := ext.customCommand("/review main.go")
	if !found || command.Expand(argument) != "Review main.go for bugs." {
		t.Fatalf("review = %+v %q %t", command, argument, found)
	}
	if plan, _, found := ext.customCommand("/plan add a flag"); !found || !plan.ReadOnly {
		t.Fatalf("plan = %+v %t", plan, found)
	}
	for _, input := range []string{"/help", "/undo", "/missing", "/review\nsecond line", "review"} {
		if _, _, found := ext.customCommand(input); found {
			t.Fatalf("%q ran as a user command", input)
		}
	}
	if !strings.Contains(ext.commandHelp(), "/review") {
		t.Fatalf("help = %q", ext.commandHelp())
	}
	writeFile(t, filepath.Join(root, ".inkling", "commands", "status.md"), "Show status.\n")
	if _, err := loadExtensions(root); err == nil || !strings.Contains(err.Error(), "/status") {
		t.Fatalf("a command that hides /status loaded: %v", err)
	}
}

func TestRewindTarget(t *testing.T) {
	var output strings.Builder
	if _, ok := rewindTarget("/undo", "", nil, &output); ok {
		t.Fatal("undo without checkpoints")
	}
	checkpoints := []checkpoint{{prompt: "first\nmore"}, {prompt: "second"}, {prompt: "third"}}
	if target, ok := rewindTarget("/undo", "", checkpoints, &output); !ok || target != 2 {
		t.Fatalf("undo target = %d %t", target, ok)
	}
	output.Reset()
	if _, ok := rewindTarget("/rewind", "", checkpoints, &output); ok || !strings.Contains(output.String(), "  1  first\n") {
		t.Fatalf("list = %q", output.String())
	}
	if target, ok := rewindTarget("/rewind", "2", checkpoints, &output); !ok || target != 1 {
		t.Fatalf("rewind 2 = %d %t", target, ok)
	}
	for _, argument := range []string{"0", "4", "x"} {
		if _, ok := rewindTarget("/rewind", argument, checkpoints, &output); ok {
			t.Fatalf("rewind %q accepted", argument)
		}
	}
}
