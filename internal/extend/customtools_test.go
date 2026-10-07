package extend

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const objectSchema = `{"type":"object","properties":{"name":{"type":"string"}}}`

func manifest(name, command string, extra string) string {
	return `{"name":"` + name + `","description":"Tool ` + name + `","input_schema":` + objectSchema + `,"command":` + strconv.Quote(command) + extra + `}`
}

func loadOneTool(t *testing.T, command, extra string) (tools.Tool, string) {
	t.Helper()
	dirs := newLayout(t)
	writeFile(t, filepath.Join(dirs.projectDir(), toolsDirName, "probe.json"), manifest("probe", command, extra))
	loaded, err := loadTools(dirs.userDir, dirs.workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("tools = %d, want 1", len(loaded))
	}
	return loaded[0], dirs.workDir
}

func TestToolsProjectOverridesUserAndSortByName(t *testing.T) {
	dirs := newLayout(t)
	userTools := filepath.Join(dirs.userDir, toolsDirName)
	projectTools := filepath.Join(dirs.projectDir(), toolsDirName)
	writeFile(t, filepath.Join(userTools, "deploy.json"), manifest("deploy", "echo user", ""))
	writeFile(t, filepath.Join(userTools, "zeta.json"), manifest("zeta", "echo zeta", `,"read_only":true`))
	writeFile(t, filepath.Join(userTools, "notes.txt"), "not a manifest")
	writeFile(t, filepath.Join(projectTools, "deploy-tool.json"), manifest("deploy", "echo project", ""))
	writeFile(t, filepath.Join(projectTools, "alpha.json"), manifest("alpha", "echo alpha", ""))

	loaded, err := loadTools(dirs.userDir, dirs.workDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range loaded {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "alpha,deploy,zeta" {
		t.Fatalf("names = %v", names)
	}
	if loaded[1].ReadOnly || !loaded[2].ReadOnly {
		t.Fatalf("read-only flags = %v, %v", loaded[1].ReadOnly, loaded[2].ReadOnly)
	}
	if string(loaded[0].InputSchema) != objectSchema || loaded[0].Description != "Tool alpha" {
		t.Fatalf("alpha = %+v", loaded[0])
	}
	result, err := loaded[1].Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || strings.TrimSpace(result.Content) != "project" {
		t.Fatalf("deploy result = %+v, want the project command", result)
	}
}

func TestToolManifestErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"unknown field", manifest("probe", "true", `,"timeout":5`), `unknown field "timeout"`},
		{"invalid JSON", `{"name":`, "parse"},
		{"upper case name", manifest("Probe", "true", ""), "must match [a-z][a-z0-9_]*"},
		{"name with dash", manifest("my-probe", "true", ""), "must match"},
		{"name starts with digit", manifest("1probe", "true", ""), "must match"},
		{"empty name", manifest("", "true", ""), "must match"},
		{"empty command", manifest("probe", " ", ""), "command is empty"},
		{"no description", `{"name":"probe","input_schema":` + objectSchema + `,"command":"true"}`, "description is empty"},
		{"no schema", `{"name":"probe","description":"x","command":"true"}`, "input_schema: is missing"},
		{"array schema", `{"name":"probe","description":"x","input_schema":[],"command":"true"}`, "is not a JSON object"},
		{"schema of wrong type", `{"name":"probe","description":"x","input_schema":{"type":"string"},"command":"true"}`, `must have "type": "object"`},
		{"timeout too large", manifest("probe", "true", `,"timeout_seconds":601`), "timeout_seconds must be from 1 to 600"},
		{"negative timeout", manifest("probe", "true", `,"timeout_seconds":-1`), "timeout_seconds"},
	}
	for _, name := range reservedToolNames {
		tests = append(tests, struct {
			name    string
			content string
			want    string
		}{"reserved " + name, manifest(name, "true", ""), "reserved for a built-in tool"})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dirs := newLayout(t)
			path := filepath.Join(dirs.userDir, toolsDirName, "probe.json")
			writeFile(t, path, test.content)
			_, err := loadTools(dirs.userDir, dirs.workDir)
			wantErrorContaining(t, err, path, test.want)
		})
	}

	t.Run("same name twice in one directory", func(t *testing.T) {
		dirs := newLayout(t)
		writeFile(t, filepath.Join(dirs.userDir, toolsDirName, "a.json"), manifest("probe", "true", ""))
		writeFile(t, filepath.Join(dirs.userDir, toolsDirName, "b.json"), manifest("probe", "true", ""))
		_, err := loadTools(dirs.userDir, dirs.workDir)
		wantErrorContaining(t, err, `tool name "probe" is also in`)
	})
}

func TestToolGetsInputOnStdinAndRunsInWorkDir(t *testing.T) {
	tool, workDir := loadOneTool(t, `cat; echo; pwd -P`, "")
	input := json.RawMessage(`{"name":"value"}`)
	result, err := tool.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(input) + "\n" + resolvedWorkDir + "\n"; result.IsError || result.Content != want {
		t.Fatalf("result = %+v, want content %q", result, want)
	}
}

func TestToolReportsExitCodeAsExpectedFailure(t *testing.T) {
	tool, _ := loadOneTool(t, `echo out; echo err 1>&2; exit 3`, "")
	result, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Content != "out\nerr\n\n[exit code 3]" {
		t.Fatalf("result = %+v", result)
	}
}

func TestToolEmptyOutput(t *testing.T) {
	tool, _ := loadOneTool(t, `true`, "")
	result, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "(no output)" {
		t.Fatalf("result = %+v", result)
	}
}

func TestToolOutputIsCapped(t *testing.T) {
	tool, _ := loadOneTool(t, `head -c 300000 /dev/zero | tr '\0' a`, "")
	result, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	content, notice, found := strings.Cut(result.Content, "\n[output truncated")
	if result.IsError || !found {
		t.Fatalf("result has no truncation notice: IsError=%v, tail %q", result.IsError, result.Content[len(result.Content)-100:])
	}
	if len(content) != toolOutputLimit || strings.Trim(content, "a") != "" {
		t.Fatalf("kept %d bytes, want %d of the output", len(content), toolOutputLimit)
	}
	if !strings.Contains(notice, "of 300000 bytes") {
		t.Fatalf("notice = %q", notice)
	}
}

func TestToolTimeoutKillsProcessGroup(t *testing.T) {
	tool, _ := loadOneTool(t, `sleep 30 & echo $!; wait`, `,"timeout_seconds":1`)
	started := time.Now()
	result, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("tool returned after %s", elapsed)
	}
	if !result.IsError || !strings.Contains(result.Content, "[timed out after 1s]") {
		t.Fatalf("result = %+v", result)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(result.Content, "\n", 2)[0]))
	if err != nil {
		t.Fatalf("no child pid in %q", result.Content)
	}
	waitForProcessExit(t, childPID)
}

func TestToolCancelKillsProcessGroup(t *testing.T) {
	dirs := newLayout(t)
	pidFile := filepath.Join(dirs.workDir, "child.pid")
	writeFile(t, filepath.Join(dirs.userDir, toolsDirName, "probe.json"), manifest("probe", `sleep 30 & echo $! > child.pid; wait`, ""))
	loaded, err := loadTools(dirs.userDir, dirs.workDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// The tool runs until the cancel, so cancel also when the wait fails.
		_, _ = waitForPIDFile(pidFile)
		cancel()
	}()
	_, err = loaded[0].Run(ctx, json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	childPID, err := waitForPIDFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, childPID)
}
