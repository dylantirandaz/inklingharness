package extend

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckGoal(t *testing.T) {
	workDir := t.TempDir()
	for _, test := range []struct {
		command, status, tail string
		passed                bool
	}{
		{"echo all good", "exit code 0", "all good", true},
		{"echo failing test >&2; exit 3", "exit code 3", "failing test", false},
		{"pwd", "exit code 0", "", true},
	} {
		result, err := CheckGoal(context.Background(), workDir, test.command)
		if err != nil {
			t.Fatal(err)
		}
		if result.Passed != test.passed || result.Status != test.status || (test.tail != "" && result.Tail != test.tail) {
			t.Fatalf("%q = %+v", test.command, result)
		}
	}
	// The check runs in the working directory.
	result, err := CheckGoal(context.Background(), workDir, "pwd -P")
	if resolved, _ := filepath.EvalSymlinks(workDir); err != nil || result.Tail != resolved {
		t.Fatalf("pwd = %+v, %v", result, err)
	}
}

// A long output keeps its end, where a test summary is, and stays bounded.
func TestCheckGoalKeepsTheTail(t *testing.T) {
	result, err := CheckGoal(context.Background(), t.TempDir(), "for i in $(seq 1 5000); do echo line $i; done; echo 'FAIL summary'; exit 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || !strings.HasSuffix(result.Tail, "line 5000\nFAIL summary") ||
		!strings.HasPrefix(result.Tail, "[earlier output omitted]\nline ") || len(result.Tail) > goalTailLimit+64 {
		t.Fatalf("tail = %d bytes: %q ... %q", len(result.Tail), result.Tail[:60], result.Tail[len(result.Tail)-30:])
	}
}

// A check that hangs is reported as a failure, not as a pass, and its
// process group is stopped.
func TestCheckGoalTimeout(t *testing.T) {
	workDir := t.TempDir()
	started := time.Now()
	result, err := checkGoal(context.Background(), workDir, "sleep 30 & echo $! > child.pid; wait", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || !strings.HasPrefix(result.Status, "timed out") || time.Since(started) > 10*time.Second {
		t.Fatalf("timeout = %+v after %v", result, time.Since(started))
	}
	childPID, err := waitForPIDFile(filepath.Join(workDir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, childPID)
}
