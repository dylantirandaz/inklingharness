package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func lookup(t *testing.T, root, name string) Tool {
	t.Helper()
	return lookupWithOutputs(t, root, filepath.Join(t.TempDir(), "outputs"), name)
}

func lookupWithOutputs(t *testing.T, root, outputDirectory, name string) Tool {
	t.Helper()
	set, err := Standard(root, outputDirectory, newTestJobs(t, outputDirectory))
	if err != nil {
		t.Fatal(err)
	}
	tool, found := set.Lookup(name)
	if !found {
		t.Fatalf("tool %q missing", name)
	}
	return tool
}

// newTestJobs stops the jobs of a test when the test ends.
func newTestJobs(t *testing.T, outputDirectory string) *Jobs {
	t.Helper()
	jobs := NewJobs(outputDirectory)
	t.Cleanup(func() {
		if err := jobs.Close(); err != nil {
			t.Error(err)
		}
	})
	return jobs
}

func TestEditFileRequiresExactlyOneOccurrence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\na\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edit := lookup(t, root, "edit_file")

	ambiguous, err := edit.Run(context.Background(), json.RawMessage(`{"path":"f.txt","old_string":"a","new_string":"x"}`))
	if err != nil || !ambiguous.IsError || !strings.Contains(ambiguous.Content, "2 times") {
		t.Fatalf("ambiguous edit = %+v, %v", ambiguous, err)
	}
	missing, err := edit.Run(context.Background(), json.RawMessage(`{"path":"f.txt","old_string":"zzz","new_string":"x"}`))
	if err != nil || !missing.IsError || !strings.Contains(missing.Content, "not found") {
		t.Fatalf("missing edit = %+v, %v", missing, err)
	}
	unique, err := edit.Run(context.Background(), json.RawMessage(`{"path":"f.txt","old_string":"b","new_string":"B"}`))
	if err != nil || unique.IsError {
		t.Fatalf("unique edit = %+v, %v", unique, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "a\nB\na\n" {
		t.Fatalf("file = %q", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 kept", info.Mode().Perm())
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	bash := lookup(t, t.TempDir(), "bash")
	started := time.Now()
	result, err := bash.Run(context.Background(), json.RawMessage(`{"command":"sleep 30 & echo $!; wait","timeout_seconds":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("bash returned after %s", elapsed)
	}
	if !result.IsError || !strings.Contains(result.Content, "timed out") {
		t.Fatalf("result = %+v", result)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(result.Content, "\n", 2)[0]))
	if err != nil {
		t.Fatalf("no child pid in %q", result.Content)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child %d still alive after the timeout (kill 0: %v)", childPID, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBashReportsExitCodeAsExpectedFailure(t *testing.T) {
	bash := lookup(t, t.TempDir(), "bash")
	result, err := bash.Run(context.Background(), json.RawMessage(`{"command":"echo out; echo err 1>&2; exit 3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "out\n") || !strings.Contains(result.Content, "err\n") || !strings.HasSuffix(result.Content, "[exit code 3]") {
		t.Fatalf("result = %+v", result)
	}
}

func runBash(t *testing.T, bash Tool, command string) Result {
	t.Helper()
	input, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bash.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func savedOutputFiles(t *testing.T, outputDirectory string) []string {
	t.Helper()
	entries, err := os.ReadDir(outputDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, filepath.Join(outputDirectory, entry.Name()))
	}
	return paths
}

func TestBashReturnsOutputAtInlineLimitWhole(t *testing.T) {
	root := t.TempDir()
	outputs := filepath.Join(t.TempDir(), "outputs")
	bash := lookupWithOutputs(t, root, outputs, "bash")
	result := runBash(t, bash, "head -c 32768 /dev/zero | tr '\\0' a")
	if result.IsError || result.Content != strings.Repeat("a", 32768) {
		t.Fatalf("result has %d bytes, IsError %t", len(result.Content), result.IsError)
	}
	if _, err := os.Stat(outputs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output directory exists after a 32 KiB output (stat: %v)", err)
	}
}

func TestBashSavesLargeOutputForReadFile(t *testing.T) {
	root := t.TempDir()
	outputs := filepath.Join(t.TempDir(), "outputs")
	var lines strings.Builder
	for number := 1; number <= 100000; number++ {
		fmt.Fprintf(&lines, "%d\n", number)
	}
	full := lines.String()[:32769]
	result := runBash(t, lookupWithOutputs(t, root, outputs, "bash"), "seq 1 100000 | head -c 32769; exit 3")

	files := savedOutputFiles(t, outputs)
	if len(files) != 1 {
		t.Fatalf("saved files = %v", files)
	}
	saved, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != full {
		t.Fatalf("saved file has %d bytes and differs from the command output", len(saved))
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	// The head cut falls inside a line, so the marker starts on a new line.
	marker := fmt.Sprintf("\n[output truncated: 1 bytes omitted; full output (32769 bytes) saved to %s; use read_file with offset and limit to read it]\n", files[0])
	want := full[:16384] + marker + full[len(full)-16384:] + "\n[exit code 3]"
	if !result.IsError || result.Content != want {
		t.Fatalf("result content = %q, want head, %q, tail and exit code", result.Content, marker)
	}

	read := runTool(t, lookup(t, root, "read_file"), fmt.Sprintf(`{"path":%q,"offset":2,"limit":1}`, files[0]))
	if read.IsError || read.Content != "2: 2\n[truncated: line limit; continue with offset 3]" {
		t.Fatalf("read_file on saved output = %+v", read)
	}
}

func TestBashCapsSavedOutputFile(t *testing.T) {
	outputs := filepath.Join(t.TempDir(), "outputs")
	total := maxSavedOutputBytes + 1000
	result := runBash(t, lookupWithOutputs(t, t.TempDir(), outputs, "bash"), "head -c "+strconv.Itoa(total)+" /dev/zero")
	files := savedOutputFiles(t, outputs)
	if len(files) != 1 {
		t.Fatalf("saved files = %v", files)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != maxSavedOutputBytes {
		t.Fatalf("saved size = %d, want %d", info.Size(), maxSavedOutputBytes)
	}
	note := fmt.Sprintf("full output (%d bytes) saved to %s; the file holds only the first 67108864 bytes;", total, files[0])
	if result.IsError || !strings.Contains(result.Content, note) || len(result.Content) > outputInlineBytes+1024 {
		t.Fatalf("result has %d bytes and no capped note %q", len(result.Content), note)
	}
}

func TestBashTruncationKeepsUTF8Valid(t *testing.T) {
	root := t.TempDir()
	// The head cut and the tail cut both fall inside a three-byte rune.
	output := strings.Repeat("a", 16383) + "€" + strings.Repeat("m", 1000) + "€" + strings.Repeat("z", 16382)
	if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	outputs := filepath.Join(t.TempDir(), "outputs")
	result := runBash(t, lookupWithOutputs(t, root, outputs, "bash"), "cat out.txt")
	if !utf8.ValidString(result.Content) {
		t.Fatal("truncated result is not valid UTF-8")
	}
	omitted := len(output) - 16383 - 16382
	prefix := strings.Repeat("a", 16383) + "\n[output truncated: " + strconv.Itoa(omitted) + " bytes omitted;"
	if !strings.HasPrefix(result.Content, prefix) || !strings.HasSuffix(result.Content, "]\n"+strings.Repeat("z", 16382)) {
		t.Fatalf("result does not cut at rune boundaries: %q", result.Content)
	}
	files := savedOutputFiles(t, outputs)
	if len(files) != 1 {
		t.Fatalf("saved files = %v", files)
	}
	saved, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, []byte(output)) {
		t.Fatal("saved file differs from the command output")
	}
}

func TestBashCancellationRemovesSavedOutput(t *testing.T) {
	outputs := filepath.Join(t.TempDir(), "outputs")
	bash := lookupWithOutputs(t, t.TempDir(), outputs, "bash")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if entries, err := os.ReadDir(outputs); err == nil && len(entries) > 0 {
				cancel()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_, err := bash.Run(ctx, json.RawMessage(`{"command":"head -c 100000 /dev/zero; sleep 30"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if files := savedOutputFiles(t, outputs); len(files) != 0 {
		t.Fatalf("partial output files remain after cancellation: %v", files)
	}
}

func TestBashTimeoutKeepsSavedOutput(t *testing.T) {
	outputs := filepath.Join(t.TempDir(), "outputs")
	bash := lookupWithOutputs(t, t.TempDir(), outputs, "bash")
	result, err := bash.Run(context.Background(), json.RawMessage(`{"command":"head -c 40000 /dev/zero; sleep 30","timeout_seconds":1}`))
	if err != nil {
		t.Fatal(err)
	}
	files := savedOutputFiles(t, outputs)
	if len(files) != 1 {
		t.Fatalf("saved files = %v", files)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 40000 || !result.IsError || !strings.Contains(result.Content, "saved to "+files[0]) || !strings.HasSuffix(result.Content, "[timed out after 1s]") {
		t.Fatalf("saved size = %d, result = %q", info.Size(), result.Content)
	}
}

func TestBashReportsUnsavedOutputWithoutFailing(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bash := lookupWithOutputs(t, t.TempDir(), filepath.Join(blocker, "outputs"), "bash")
	result := runBash(t, bash, "head -c 40000 /dev/zero | tr '\\0' a")
	want := strings.Repeat("a", 16384) + "\n[output truncated: 7232 bytes omitted; full output was not saved: "
	if result.IsError || !strings.HasPrefix(result.Content, want) || !strings.HasSuffix(result.Content, "]\n"+strings.Repeat("a", 16384)) {
		t.Fatalf("result = %q", result.Content)
	}
}

// A session's saved outputs must stay bounded, but the file that the newest
// result names must survive even when it alone exceeds the limit.
func TestOutputDirectoryKeepsNewestFileAndRemovesOldestFirst(t *testing.T) {
	directory := t.TempDir()
	base := time.Now().Add(-time.Hour)
	files := []struct {
		name string
		size int
	}{{"a.txt", 40}, {"b.txt", 30}, {"c.txt", 50}, {"notes.log", 999}}
	for index, file := range files {
		path := filepath.Join(directory, file.name)
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), file.size), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := base.Add(time.Duration(index) * time.Minute)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(directory, name))
		return err == nil
	}
	keepNewest := func(path string) bool { return path == filepath.Join(directory, "c.txt") }
	if err := pruneOutputDirectory(directory, keepNewest, 100); err != nil {
		t.Fatal(err)
	}
	if exists("a.txt") || !exists("b.txt") || !exists("c.txt") || !exists("notes.log") {
		t.Fatal("pruning to 100 bytes must remove only the oldest output file")
	}
	if err := pruneOutputDirectory(directory, keepNewest, 10); err != nil {
		t.Fatal(err)
	}
	if exists("b.txt") || !exists("c.txt") || !exists("notes.log") {
		t.Fatal("pruning below the newest file must keep that file and other kinds of files")
	}
}
