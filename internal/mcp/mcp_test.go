package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func fakeConfig(t *testing.T, name string, env map[string]string) ServerConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{fakeRoleVariable: "server"}
	maps.Copy(variables, env)
	return ServerConfig{Name: name, Command: executable, Env: variables, Description: "Fake server " + name + "."}
}

func newManager(t *testing.T, workDir string, servers ...ServerConfig) *Manager {
	t.Helper()
	manager, err := NewManager(servers, workDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return manager
}

func toolNamed(t *testing.T, manager *Manager, name string) tools.Tool {
	t.Helper()
	for _, tool := range manager.Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q missing", name)
	return tools.Tool{}
}

func runTool(t *testing.T, manager *Manager, name, input string) tools.Result {
	t.Helper()
	result, err := toolNamed(t, manager, name).Run(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s %s: %v", name, input, err)
	}
	return result
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(content))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// processGone waits until the process with the pid is reaped.
func processGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func markerPid(t *testing.T, marker string) int {
	t.Helper()
	lines := fileLines(t, marker)
	if len(lines) != 1 {
		t.Fatalf("marker lines = %q, want one start", lines)
	}
	pid, err := strconv.Atoi(strings.Fields(lines[0])[0])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestNewManagerRejectsInvalidServers(t *testing.T) {
	valid := ServerConfig{Name: "a-1_b", Command: "server"}
	cases := []struct {
		name    string
		servers []ServerConfig
		want    string
	}{
		{"empty name", []ServerConfig{{Command: "x"}}, `server 1: name "" does not match [a-z0-9_-]+`},
		{"upper case", []ServerConfig{{Name: "Git", Command: "x"}}, `name "Git" does not match`},
		{"dot", []ServerConfig{valid, {Name: "a.b", Command: "x"}}, `server 2: name "a.b" does not match`},
		{"space", []ServerConfig{{Name: "a b", Command: "x"}}, `name "a b" does not match`},
		{"duplicate", []ServerConfig{valid, valid}, `server 2: duplicate name "a-1_b"`},
		{"no command", []ServerConfig{{Name: "a"}}, `server "a" has no command`},
		{"two-line description", []ServerConfig{{Name: "a", Command: "x", Description: "one\ntwo"}}, "one line"},
		{"variable name with =", []ServerConfig{{Name: "a", Command: "x", Env: map[string]string{"A=B": "c"}}}, `name "A=B" is not valid`},
		{"empty variable name", []ServerConfig{{Name: "a", Command: "x", Env: map[string]string{"": "c"}}}, `name "" is not valid`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manager, err := NewManager(test.servers, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewManager = %v, %v; want error with %q", manager, err, test.want)
			}
		})
	}
	if _, err := NewManager([]ServerConfig{valid, {Name: "z", Command: "y", Env: map[string]string{"KEY": "a=b"}}}, t.TempDir()); err != nil {
		t.Fatalf("valid servers: %v", err)
	}
}

func TestPromptSectionAndTools(t *testing.T) {
	empty := newManager(t, t.TempDir())
	if section := empty.PromptSection(); section != "" {
		t.Fatalf("empty prompt section = %q", section)
	}
	manager := newManager(t, t.TempDir(), ServerConfig{Name: "zeta", Command: "z", Description: "Search the docs."}, ServerConfig{Name: "alpha", Command: "a"})
	want := "MCP servers give more tools. Call mcp_list with the server name to get its tools before you use mcp_call.\n- alpha\n- zeta: Search the docs."
	if section := manager.PromptSection(); section != want {
		t.Fatalf("prompt section = %q, want %q", section, want)
	}
	all := manager.Tools()
	if len(all) != 2 || all[0].Name != "mcp_list" || !all[0].ReadOnly || all[1].Name != "mcp_call" || all[1].ReadOnly {
		t.Fatalf("tools = %+v", all)
	}
	for _, tool := range all {
		if !json.Valid(tool.InputSchema) {
			t.Fatalf("%s input schema is not JSON", tool.Name)
		}
	}
}

func TestServerStartsOnFirstUseOnlyOnce(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "starts")
	manager := newManager(t, workDir, fakeConfig(t, "fake", map[string]string{markerVariable: marker}))
	manager.Tools()
	manager.PromptSection()
	time.Sleep(100 * time.Millisecond)
	if lines := fileLines(t, marker); lines != nil {
		t.Fatalf("the server started before first use: %q", lines)
	}

	list := toolNamed(t, manager, "mcp_list")
	results := make([]tools.Result, 8)
	failures := make([]error, len(results))
	var group sync.WaitGroup
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			results[index], failures[index] = list.Run(context.Background(), json.RawMessage(`{"server":"fake"}`))
		}()
	}
	group.Wait()
	for index, result := range results {
		if failures[index] != nil || result.IsError || result.Content != results[0].Content {
			t.Fatalf("call %d = %+v, %v", index, result, failures[index])
		}
	}
	lines := fileLines(t, marker)
	if len(lines) != 1 {
		t.Fatalf("starts = %q, want one", lines)
	}
	fields := strings.SplitN(lines[0], " ", 3)
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if fields[0] != fields[1] || fields[2] != resolved {
		t.Fatalf("start record %q: want own process group and directory %s", lines[0], resolved)
	}
}

func TestListFollowsPagination(t *testing.T) {
	manager := newManager(t, t.TempDir(), fakeConfig(t, "fake", nil))
	result := runTool(t, manager, "mcp_list", `{"server":"fake"}`)
	if result.IsError || !strings.HasPrefix(result.Content, `MCP server "fake" has 8 tools.`) {
		t.Fatalf("list = %+v", result)
	}
	if !strings.Contains(result.Content, "\n\ntool: echo\ndescription: Fake tool echo.\ninput schema: {\"type\":\"object\",\"properties\":{\"text\":{\"type\":\"string\"}}}\n\n") {
		t.Fatalf("list has no full echo entry: %s", result.Content)
	}
	previous := -1
	for _, page := range fakeToolPages {
		for _, name := range page {
			position := strings.Index(result.Content, "\ntool: "+name+"\n")
			if position <= previous {
				t.Fatalf("tool %s is missing or out of order: %s", name, result.Content)
			}
			previous = position
		}
	}

	loop := newManager(t, t.TempDir(), fakeConfig(t, "loop", map[string]string{cursorLoopVariable: "1"}))
	result = runTool(t, loop, "mcp_list", `{"server":"loop"}`)
	if !result.IsError || !strings.Contains(result.Content, `tools/list gave the cursor "1" again`) {
		t.Fatalf("cursor loop = %+v", result)
	}
}

func TestCallResults(t *testing.T) {
	manager := newManager(t, t.TempDir(), fakeConfig(t, "fake", nil))
	cases := []struct {
		tool    string
		input   string
		want    string
		isError bool
	}{
		{"mcp_call", `{"server":"fake","tool":"echo","arguments":{"text":"hello"}}`, "hello", false},
		{"mcp_call", `{"server":"fake","tool":"echo","arguments":null}`, "(no content)", false},
		{"mcp_call", `{"server":"fake","tool":"mixed"}`, "first\n[image/png content omitted]\n[audio/wav content omitted]\n[resource link file:///tmp/a.txt]\n[embedded resource file:///tmp/b.txt omitted]\n[widget content omitted]\nlast", false},
		{"mcp_call", `{"server":"fake","tool":"fail","arguments":{}}`, "it failed", true},
		{"mcp_call", `{"server":"fake","tool":"rpc_error"}`, `MCP server "fake": JSON-RPC error -32602: bad arguments`, true},
		{"mcp_call", `{"server":"fake","tool":"missing"}`, `MCP server "fake": JSON-RPC error -32602: unknown tool missing`, true},
		{"mcp_call", `{"server":"fake","tool":"ping"}`, "pong", false},
		{"mcp_call", `{"server":"nope","tool":"echo"}`, `unknown MCP server "nope"; known servers: fake`, true},
		{"mcp_list", `{"server":"nope"}`, `unknown MCP server "nope"; known servers: fake`, true},
		{"mcp_call", `{"server":"fake","tool":"echo","arguments":[1]}`, "invalid input: arguments must be a JSON object", true},
		{"mcp_call", `{"server":"fake"}`, "invalid input: tool is required", true},
		{"mcp_list", `{}`, "invalid input: server is required", true},
	}
	for _, test := range cases {
		result := runTool(t, manager, test.tool, test.input)
		if result.Content != test.want || result.IsError != test.isError {
			t.Errorf("%s %s = %+v, want %q with error %v", test.tool, test.input, result, test.want, test.isError)
		}
	}
}

func TestConcurrentCallsGetTheirOwnResults(t *testing.T) {
	manager := newManager(t, t.TempDir(), fakeConfig(t, "fake", nil))
	call := toolNamed(t, manager, "mcp_call")
	// The first calls answer last, so the answers come out of order.
	results := make([]tools.Result, 6)
	failures := make([]error, len(results))
	var group sync.WaitGroup
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			input := fmt.Sprintf(`{"server":"fake","tool":"echo","arguments":{"text":"call %d","delay_milliseconds":%d}}`, index, (len(results)-index)*50)
			results[index], failures[index] = call.Run(context.Background(), json.RawMessage(input))
		}()
	}
	group.Wait()
	for index, result := range results {
		if failures[index] != nil || result.IsError || result.Content != fmt.Sprintf("call %d", index) {
			t.Fatalf("call %d = %+v, %v", index, result, failures[index])
		}
	}
}

func TestCancelledCallReturnsPromptlyAndTellsTheServer(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events")
	manager := newManager(t, t.TempDir(), fakeConfig(t, "fake", map[string]string{eventLogVariable: events}))
	// Start the server first, so the measured time is the call only.
	if result := runTool(t, manager, "mcp_list", `{"server":"fake"}`); result.IsError {
		t.Fatalf("list = %+v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	_, err := toolNamed(t, manager, "mcp_call").Run(ctx, json.RawMessage(`{"server":"fake","tool":"sleep"}`))
	if elapsed := time.Since(started); !errors.Is(err, context.Canceled) || elapsed > time.Second {
		t.Fatalf("cancelled call = %v after %s", err, elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		lines := fileLines(t, events)
		if len(lines) == 2 {
			sleep, cancelled := strings.TrimPrefix(lines[0], "sleep "), strings.TrimPrefix(lines[1], "cancelled ")
			if sleep == lines[0] || sleep != cancelled {
				t.Fatalf("events = %q, want a cancellation of the sleep call", lines)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %q, want a cancellation", lines)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result := runTool(t, manager, "mcp_call", `{"server":"fake","tool":"echo","arguments":{"text":"after"}}`); result.IsError || result.Content != "after" {
		t.Fatalf("call after cancellation = %+v", result)
	}
}

func TestCrashGivesExitStatusAndStderrTail(t *testing.T) {
	manager := newManager(t, t.TempDir(), fakeConfig(t, "fake", nil))
	want := "MCP server \"fake\": the server exited (exit status 3); stderr tail:\nfatal: boom"
	if result := runTool(t, manager, "mcp_call", `{"server":"fake","tool":"crash"}`); !result.IsError || result.Content != want {
		t.Fatalf("crash = %+v, want %q", result, want)
	}
	for _, later := range []struct{ tool, input string }{
		{"mcp_call", `{"server":"fake","tool":"echo","arguments":{"text":"x"}}`},
		{"mcp_list", `{"server":"fake"}`},
	} {
		if result := runTool(t, manager, later.tool, later.input); !result.IsError || result.Content != want {
			t.Fatalf("%s after crash = %+v, want %q", later.tool, result, want)
		}
	}
}

func TestStartupFailuresAreResultsAndFinal(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "starts")
	missing := filepath.Join(t.TempDir(), "no-such-server")
	manager := newManager(t, t.TempDir(),
		fakeConfig(t, "early", map[string]string{failStartVariable: "missing token", markerVariable: marker}),
		ServerConfig{Name: "absent", Command: missing})
	want := "MCP server \"early\": start: initialize: the server exited (exit status 2); stderr tail:\nmissing token"
	for attempt := range 2 {
		if result := runTool(t, manager, "mcp_list", `{"server":"early"}`); !result.IsError || result.Content != want {
			t.Fatalf("attempt %d = %+v, want %q", attempt, result, want)
		}
	}
	if lines := fileLines(t, marker); len(lines) != 1 {
		t.Fatalf("starts = %q, want one", lines)
	}
	result := runTool(t, manager, "mcp_call", `{"server":"absent","tool":"x"}`)
	if !result.IsError || !strings.HasPrefix(result.Content, `MCP server "absent": start: `) || !strings.Contains(result.Content, "no such file or directory") {
		t.Fatalf("missing command = %+v", result)
	}
}

func TestMessageLineLimit(t *testing.T) {
	manager := newManager(t, t.TempDir(), fakeConfig(t, "fake", nil))
	result := runTool(t, manager, "mcp_call", fmt.Sprintf(`{"server":"fake","tool":"sized","arguments":{"bytes":%d}}`, maxLineBytes))
	if result.IsError || len(result.Content) > maxResultBytes || !strings.HasPrefix(result.Content, "xxxx") || !strings.HasSuffix(result.Content, "bytes omitted]") || !strings.Contains(result.Content, "\n[truncated: 256 KiB output limit; ") {
		t.Fatalf("line at the limit: length %d, error %v, end %q", len(result.Content), result.IsError, result.Content[max(0, len(result.Content)-100):])
	}
	result = runTool(t, manager, "mcp_call", fmt.Sprintf(`{"server":"fake","tool":"sized","arguments":{"bytes":%d}}`, maxLineBytes+1))
	if want := `MCP server "fake": the server sent a message line larger than 16 MiB`; !result.IsError || result.Content != want {
		t.Fatalf("line above the limit = %q, want %q", result.Content, want)
	}
}

func TestCloseStopsServers(t *testing.T) {
	directory := t.TempDir()
	politeMarker := filepath.Join(directory, "polite")
	stubbornMarker := filepath.Join(directory, "stubborn")
	childFile := filepath.Join(directory, "child")
	unusedMarker := filepath.Join(directory, "unused")

	polite := newManager(t, t.TempDir(), fakeConfig(t, "polite", map[string]string{markerVariable: politeMarker}))
	if result := runTool(t, polite, "mcp_list", `{"server":"polite"}`); result.IsError {
		t.Fatalf("list = %+v", result)
	}
	started := time.Now()
	if err := polite.Close(); err != nil {
		t.Fatal(err)
	}
	// The server exits when stdin closes, so Close does not wait for the kill.
	if elapsed := time.Since(started); elapsed >= stopDelay {
		t.Fatalf("close of a server that exits on end of input took %s", elapsed)
	}
	processGone(t, markerPid(t, politeMarker))

	manager := newManager(t, t.TempDir(),
		fakeConfig(t, "stubborn", map[string]string{markerVariable: stubbornMarker, stubbornVariable: childFile}),
		fakeConfig(t, "unused", map[string]string{markerVariable: unusedMarker}))
	if result := runTool(t, manager, "mcp_list", `{"server":"stubborn"}`); result.IsError {
		t.Fatalf("list = %+v", result)
	}
	childText, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(string(childText))
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < stopDelay || elapsed > stopDelay+3*time.Second {
		t.Fatalf("close of a server that ignores end of input took %s", elapsed)
	}
	processGone(t, markerPid(t, stubbornMarker))
	// The kill reaches the whole process group.
	processGone(t, child)

	for _, input := range []struct{ tool, input, server string }{
		{"mcp_list", `{"server":"unused"}`, "unused"},
		{"mcp_call", `{"server":"stubborn","tool":"echo"}`, "stubborn"},
	} {
		want := fmt.Sprintf("MCP server %q: the server is stopped", input.server)
		if result := runTool(t, manager, input.tool, input.input); !result.IsError || result.Content != want {
			t.Fatalf("%s after close = %+v, want %q", input.tool, result, want)
		}
	}
	if lines := fileLines(t, unusedMarker); lines != nil {
		t.Fatalf("an unused server started: %q", lines)
	}
}
