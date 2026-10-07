package lsp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testWaits are long, so a slow machine does not make a test fail. A test
// that expects no diagnostics checks that the answer came before the budget
// ended, so a timeout cannot pass for an empty answer.
var testWaits = waitBudgets{first: 60 * time.Second, later: 20 * time.Second}

// goplsPath finds the real gopls. The tests need it, so they fail when it is
// missing.
func goplsPath(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("gopls"); err == nil {
		return path
	}
	output, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		t.Fatalf("go env GOPATH: %v", err)
	}
	for _, directory := range filepath.SplitList(strings.TrimSpace(string(output))) {
		candidate := filepath.Join(directory, "bin", "gopls")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	t.Fatal("gopls is not in PATH or in $(go env GOPATH)/bin; install it with: go install golang.org/x/tools/gopls@latest")
	return ""
}

// goModule makes a temporary Go module and returns its root.
func goModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/m\n\ngo 1.24\n")
	return root
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestManager(t *testing.T, root string, servers ...ServerConfig) *Manager {
	t.Helper()
	manager, err := NewManager(servers, root)
	if err != nil {
		t.Fatal(err)
	}
	manager.waits = testWaits
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	return manager
}

func goplsManager(t *testing.T, root string) *Manager {
	t.Helper()
	return newTestManager(t, root, ServerConfig{Name: "gopls", Command: goplsPath(t), Extensions: []string{".go"}})
}

// diagnose calls Diagnose and fails the test on an error.
func diagnose(t *testing.T, manager *Manager, path string) (string, time.Duration) {
	t.Helper()
	started := time.Now()
	result, err := manager.Diagnose(context.Background(), path)
	if err != nil {
		t.Fatalf("Diagnose(%s): %v", path, err)
	}
	return result, time.Since(started)
}

// serverPID gives the process id of a started server. It is also the
// process group id.
func serverPID(t *testing.T, manager *Manager, name string) int {
	t.Helper()
	for _, server := range manager.servers {
		if server.config.Name != name {
			continue
		}
		server.mutex.Lock()
		session := server.session
		server.mutex.Unlock()
		if session == nil || session.connection == nil {
			t.Fatalf("server %s has no process", name)
		}
		return session.connection.command.Process.Pid
	}
	t.Fatalf("no server %s", name)
	return 0
}

// waitGroupGone waits until no process is in the group. Killed children can
// stay as zombies for a short time until init reaps them.
func waitGroupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still exists (kill: %v)", pgid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const badGo = `package m

func F() int {
	return "x"
}
`

const goodGo = `package m

func F() int {
	return 1
}
`

func TestGoplsVersionTracking(t *testing.T) {
	root := goModule(t)
	manager := goplsManager(t, root)
	path := filepath.Join(root, "main.go")

	writeFile(t, path, badGo)
	result, _ := diagnose(t, manager, path)
	if want := `main.go:4:9: error: cannot use "x"`; !strings.HasPrefix(result, want) || strings.Count(result, "\n") != 0 {
		t.Fatalf("result for the bad file = %q, want one line that starts with %q", result, want)
	}

	writeFile(t, path, goodGo)
	result, elapsed := diagnose(t, manager, path)
	if result != "" {
		t.Fatalf("result for the fixed file = %q, want empty", result)
	}
	if elapsed >= testWaits.later {
		t.Fatalf("the empty result came after the budget (%s), so it was a timeout", elapsed)
	}

	writeFile(t, path, strings.Replace(goodGo, "return 1", "var unused int\n\treturn 1", 1))
	result, _ = diagnose(t, manager, path)
	if want := "main.go:4:6: error: declared and not used: unused"; result != want {
		t.Fatalf("result after a new error = %q, want %q", result, want)
	}
	if unavailable := manager.Unavailable(); len(unavailable) != 0 {
		t.Fatalf("Unavailable = %q, want none", unavailable)
	}
}

func TestGoplsConcurrentFiles(t *testing.T) {
	root := goModule(t)
	manager := goplsManager(t, root)
	files := map[string]string{
		"a.go": "package m\n\nfunc A() int {\n\treturn \"a\"\n}\n",
		"b.go": "package m\n\nfunc B() string {\n\treturn 2\n}\n",
	}
	for name, text := range files {
		writeFile(t, filepath.Join(root, name), text)
	}
	results := make(map[string]string)
	var mutex sync.Mutex
	var group sync.WaitGroup
	for name := range files {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := manager.Diagnose(context.Background(), filepath.Join(root, name))
			if err != nil {
				t.Errorf("Diagnose(%s): %v", name, err)
			}
			mutex.Lock()
			results[name] = result
			mutex.Unlock()
		}()
	}
	group.Wait()
	if want := `a.go:4:9: error: cannot use "a"`; !strings.HasPrefix(results["a.go"], want) || strings.Contains(results["a.go"], "b.go") {
		t.Errorf("a.go result = %q, want only a line that starts with %q", results["a.go"], want)
	}
	if want := `b.go:4:9: error: cannot use 2`; !strings.HasPrefix(results["b.go"], want) || strings.Contains(results["b.go"], "a.go") {
		t.Errorf("b.go result = %q, want only a line that starts with %q", results["b.go"], want)
	}
}

func TestGoplsClose(t *testing.T) {
	root := goModule(t)
	manager := goplsManager(t, root)
	path := filepath.Join(root, "main.go")
	writeFile(t, path, goodGo)
	diagnose(t, manager, path)
	pgid := serverPID(t, manager, "gopls")

	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	waitGroupGone(t, pgid)
	if unavailable := manager.Unavailable(); len(unavailable) != 0 {
		t.Fatalf("Unavailable after Close = %q, want none", unavailable)
	}
	if _, err := manager.Diagnose(context.Background(), path); !errors.Is(err, errClosed) {
		t.Fatalf("Diagnose after Close: error = %v, want %v", err, errClosed)
	}
}

func TestGoplsCrash(t *testing.T) {
	root := goModule(t)
	manager := goplsManager(t, root)
	path := filepath.Join(root, "main.go")
	writeFile(t, path, badGo)
	if result, _ := diagnose(t, manager, path); result == "" {
		t.Fatal("no diagnostics before the crash")
	}
	pid := serverPID(t, manager, "gopls")
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if result, _ := diagnose(t, manager, path); result != "" {
			t.Fatalf("result after the crash = %q, want empty", result)
		}
	}
	unavailable := manager.Unavailable()
	if len(unavailable) != 1 || !strings.HasPrefix(unavailable[0], "gopls: the server exited (signal: killed)") {
		t.Fatalf("Unavailable = %q, want the crash of gopls", unavailable)
	}
	if got := serverPID(t, manager, "gopls"); got != pid {
		t.Fatalf("the server restarted as process %d", got)
	}
}

func TestNoServerForExtension(t *testing.T) {
	root := goModule(t)
	manager := goplsManager(t, root)
	for _, name := range []string{"notes.txt", "Makefile", "main.go.orig"} {
		path := filepath.Join(root, name)
		writeFile(t, path, "text")
		if result, _ := diagnose(t, manager, path); result != "" {
			t.Fatalf("result for %s = %q, want empty", name, result)
		}
	}
	if session := manager.servers[0].session; session != nil {
		t.Fatal("a file without a server started gopls")
	}
	if _, err := manager.Diagnose(context.Background(), "main.go"); err == nil {
		t.Fatal("Diagnose accepted a relative path")
	}
}

func TestUnknownCommand(t *testing.T) {
	root := t.TempDir()
	missingPath := filepath.Join(t.TempDir(), "gone-server")
	manager := newTestManager(t, root,
		ServerConfig{Name: "missing", Command: "inkling-no-such-language-server", Extensions: []string{".zz"}},
		ServerConfig{Name: "gone", Command: missingPath, Extensions: []string{".yy"}},
	)
	if unavailable := manager.Unavailable(); len(unavailable) != 0 {
		t.Fatalf("Unavailable before use = %q, want none", unavailable)
	}
	for range 2 {
		for _, name := range []string{"a.zz", "b.YY"} {
			path := filepath.Join(root, name)
			writeFile(t, path, "text")
			if result, _ := diagnose(t, manager, path); result != "" {
				t.Fatalf("result for %s = %q, want empty", name, result)
			}
		}
	}
	want := []string{
		`missing: command "inkling-no-such-language-server" not found`,
		`gone: command "` + missingPath + `" not found`,
	}
	if got := manager.Unavailable(); !slices.Equal(got, want) {
		t.Fatalf("Unavailable = %q, want %q", got, want)
	}
}

func TestFakeServerVersions(t *testing.T) {
	root := t.TempDir()
	manager := newTestManager(t, root, fakeServer(t, "echo"))
	path := filepath.Join(root, "dir", "x.fake")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for version, text := range []string{"one", "two", "three"} {
		writeFile(t, path, text)
		result, _ := diagnose(t, manager, path)
		want := "dir/x.fake:1:1: error: version " + strconv.Itoa(version+1) + ": " + text + "; replies ok"
		if result != want {
			t.Fatalf("result = %q, want %q", result, want)
		}
	}
}

func TestFakeServerWithoutVersion(t *testing.T) {
	root := t.TempDir()
	manager := newTestManager(t, root, fakeServer(t, "unversioned"))
	path := filepath.Join(root, "x.fake")
	for _, text := range []string{"one", "two"} {
		writeFile(t, path, text)
		if result, _ := diagnose(t, manager, path); result != "x.fake:1:1: error: unversioned: "+text {
			t.Fatalf("result = %q, want the diagnostics of %q", result, text)
		}
	}
}

func TestSlowServer(t *testing.T) {
	root := t.TempDir()
	manager := newTestManager(t, root, fakeServer(t, "silent"))
	manager.waits = waitBudgets{first: time.Second, later: 100 * time.Millisecond}
	path := filepath.Join(root, "x.fake")
	writeFile(t, path, "text")

	result, elapsed := diagnose(t, manager, path)
	if result != "" || elapsed < time.Second || elapsed > 5*time.Second {
		t.Fatalf("first call: result %q after %s, want empty after the first budget", result, elapsed)
	}
	result, elapsed = diagnose(t, manager, path)
	if result != "" || elapsed < 100*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Fatalf("second call: result %q after %s, want empty after the later budget", result, elapsed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	manager.waits.later = time.Minute
	if _, err := manager.Diagnose(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Diagnose with an ended context: error = %v, want %v", err, context.DeadlineExceeded)
	}

	// The server ignores shutdown, so Close must kill it.
	pgid := serverPID(t, manager, "fake")
	started := time.Now()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*stopDelay {
		t.Fatalf("Close took %s", elapsed)
	}
	waitGroupGone(t, pgid)
}

func TestBrokenServer(t *testing.T) {
	root := t.TempDir()
	manager := newTestManager(t, root, fakeServer(t, "garbage"))
	path := filepath.Join(root, "x.fake")
	writeFile(t, path, "text")
	if result, _ := diagnose(t, manager, path); result != "" {
		t.Fatalf("result = %q, want empty", result)
	}
	unavailable := manager.Unavailable()
	if len(unavailable) != 1 || !strings.HasPrefix(unavailable[0], "fake: the server broke the protocol: malformed message frame") {
		t.Fatalf("Unavailable = %q, want the broken frame", unavailable)
	}
	waitGroupGone(t, serverPID(t, manager, "fake"))
}

func TestNewManagerValidation(t *testing.T) {
	valid := ServerConfig{Name: "go", Command: "gopls", Extensions: []string{".go"}}
	with := func(change func(*ServerConfig)) ServerConfig {
		config := valid
		config.Extensions = slices.Clone(valid.Extensions)
		change(&config)
		return config
	}
	tests := []struct {
		name    string
		root    string
		servers []ServerConfig
		failure string // "" when the configuration is valid
	}{
		{name: "defaults", root: "/work", servers: DefaultServers()},
		{name: "no servers", root: "/work"},
		{name: "absolute command", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Command = "/usr/local/bin/gopls" })}},
		{name: "relative root", root: "work", servers: []ServerConfig{valid}, failure: "not an absolute path"},
		{name: "empty name", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Name = "" })}, failure: "does not match"},
		{name: "name with space", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Name = "my server" })}, failure: "does not match"},
		{name: "duplicate name", root: "/work", servers: []ServerConfig{valid, with(func(c *ServerConfig) { c.Extensions = []string{".mod"} })}, failure: "duplicate name"},
		{name: "no command", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Command = "" })}, failure: "no command"},
		{name: "relative command path", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Command = "bin/gopls" })}, failure: "absolute path"},
		{name: "no extensions", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Extensions = nil })}, failure: "no extensions"},
		{name: "extension without dot", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Extensions = []string{"go"} })}, failure: "must be a dot"},
		{name: "dot only", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Extensions = []string{"."} })}, failure: "must be a dot"},
		{name: "uppercase extension", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Extensions = []string{".Go"} })}, failure: "must be a dot"},
		{name: "two dots", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Extensions = []string{".d.ts"} })}, failure: "must be a dot"},
		{name: "space in extension", root: "/work", servers: []ServerConfig{with(func(c *ServerConfig) { c.Extensions = []string{".g o"} })}, failure: "must be a dot"},
		{name: "extension of two servers", root: "/work", servers: []ServerConfig{valid, with(func(c *ServerConfig) { c.Name = "other" })}, failure: `already given to server "go"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, err := NewManager(test.servers, test.root)
			switch {
			case test.failure == "" && err != nil:
				t.Fatalf("error = %v, want none", err)
			case test.failure == "":
				if len(manager.servers) != len(test.servers) {
					t.Fatalf("%d servers, want %d", len(manager.servers), len(test.servers))
				}
			case err == nil || !strings.Contains(err.Error(), test.failure):
				t.Fatalf("error = %v, want one with %q", err, test.failure)
			}
		})
	}
}

func TestNewManagerCopiesConfiguration(t *testing.T) {
	servers := []ServerConfig{{Name: "go", Command: "gopls", Args: []string{"serve"}, Extensions: []string{".go"}}}
	manager, err := NewManager(servers, "/work")
	if err != nil {
		t.Fatal(err)
	}
	servers[0].Args[0] = "changed"
	servers[0].Extensions[0] = ".changed"
	config := manager.servers[0].config
	if config.Args[0] != "serve" || config.Extensions[0] != ".go" {
		t.Fatalf("the manager shares slices with the caller: %+v", config)
	}
}
