package extend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// layout is a user directory and a work directory, each in its own temporary
// directory, so tests never touch the real home directory.
type layout struct {
	userDir, workDir string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	return layout{userDir: t.TempDir(), workDir: t.TempDir()}
}

func (l layout) projectDir() string {
	return filepath.Join(l.workDir, projectDirName)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantErrorContaining(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want an error with %q", parts)
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("err = %v, want it to contain %q", err, part)
		}
	}
}

// waitForProcessExit fails the test when the process is still alive after
// a short grace period.
func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child %d still alive (kill 0: %v)", pid, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForPIDFile polls until a hook or tool has written a process ID to path.
func waitForPIDFile(path string) (int, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, err := os.ReadFile(path)
		if err == nil {
			if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content))); parseErr == nil {
				return pid, nil
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no process ID in %s (read: %v)", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUserDirFollowsConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	dir, err := UserDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configHome, "inkling"); dir != want {
		t.Fatalf("UserDir = %q, want %q", dir, want)
	}

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	dir, err = UserDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "inkling"); dir != want {
		t.Fatalf("UserDir without XDG_CONFIG_HOME = %q, want %q", dir, want)
	}
}

func TestMissingDirectoriesGiveEmptyResults(t *testing.T) {
	missing := layout{
		userDir: filepath.Join(t.TempDir(), "absent"),
		workDir: filepath.Join(t.TempDir(), "absent"),
	}
	settings, err := loadSettings(missing.userDir, missing.workDir)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if len(settings.Allow)+len(settings.Deny)+len(settings.MCPServers)+
		len(settings.Hooks.BeforeTool)+len(settings.Hooks.AfterTool)+len(settings.Hooks.Prompt) != 0 {
		t.Fatalf("settings = %+v, want empty", settings)
	}
	commands, err := loadCommands(missing.userDir, missing.workDir)
	if err != nil || len(commands) != 0 {
		t.Fatalf("commands = %v, %v; want empty", commands, err)
	}
	skills, err := loadSkills(missing.userDir, missing.workDir)
	if err != nil || len(skills) != 0 {
		t.Fatalf("skills = %v, %v; want empty", skills, err)
	}
	loaded, err := loadTools(missing.userDir, missing.workDir)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("tools = %v, %v; want empty", loaded, err)
	}
}
