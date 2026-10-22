package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func leanTool(t *testing.T, root, outputDirectory, name string) Tool {
	t.Helper()
	set, err := Lean(root, outputDirectory, newTestJobs(t, outputDirectory))
	if err != nil {
		t.Fatal(err)
	}
	tool, found := set.Lookup(name)
	if !found {
		t.Fatalf("the lean set has no %s tool", name)
	}
	return tool
}

func writeLines(t *testing.T, path string, count int) {
	t.Helper()
	var text strings.Builder
	for line := 1; line <= count; line++ {
		fmt.Fprintf(&text, "line %d\n", line)
	}
	if err := os.WriteFile(path, []byte(text.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Each item is read with its own range, in the order of the call, and a
// missing file does not stop the files after it.
func TestReadFilesReadsEachItemWithItsOwnRangeAndKeepsGoingAfterAMissingFile(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "a.txt"), 10)
	writeLines(t, filepath.Join(root, "b.txt"), 3)
	readFiles := leanTool(t, root, filepath.Join(t.TempDir(), "outputs"), "read_files")
	result, err := readFiles.Run(context.Background(), json.RawMessage(`{"files":[{"path":"a.txt","offset":4,"limit":2},{"path":"missing.txt"},{"path":"b.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("one missing file of three made the whole result an error: %q", result.Content)
	}
	first := strings.Index(result.Content, "=== a.txt ===\n[from line 4]\nline 4\nline 5\n[truncated: line limit; continue with offset 6]")
	missing := strings.Index(result.Content, "=== missing.txt ===\n")
	last := strings.Index(result.Content, "=== b.txt ===\nline 1\nline 2\nline 3\n[end of file: 3 lines]")
	if first < 0 || missing < 0 || last < 0 || !(first < missing && missing < last) {
		t.Fatalf("sections are missing or out of order:\n%s", result.Content)
	}
}

func TestReadFilesIsAnErrorOnlyWhenNoFileCanBeRead(t *testing.T) {
	readFiles := leanTool(t, t.TempDir(), filepath.Join(t.TempDir(), "outputs"), "read_files")
	result, err := readFiles.Run(context.Background(), json.RawMessage(`{"files":[{"path":"x.txt"},{"path":"y.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("no file was read, but the result is not an error: %q", result.Content)
	}
}

// The batch keeps the 256 KiB limit of one read_file result, except that the
// first file always comes back, as read_file would return it alone.
func TestReadFilesStopsBeforeTheOutputLimitAndNamesTheFilesThatItDidNotRead(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat(strings.Repeat("x", 99)+"\n", 1000)
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(big), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readFiles := leanTool(t, root, filepath.Join(t.TempDir(), "outputs"), "read_files")
	result, err := readFiles.Run(context.Background(), json.RawMessage(`{"files":[{"path":"one.txt","limit":1000},{"path":"two.txt","limit":1000},{"path":"three.txt","limit":1000}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) > maxReadBytes+256 {
		t.Fatalf("result has %d bytes, more than the limit", len(result.Content))
	}
	if !strings.Contains(result.Content, "=== two.txt ===") || strings.Contains(result.Content, "=== three.txt ===") || !strings.HasSuffix(result.Content, "[not read: 1 more files would pass the 256 KiB output limit; read them in another call]") {
		t.Fatalf("result does not stop before three.txt with a note; it ends with %q", result.Content[max(0, len(result.Content)-200):])
	}
}

func TestReadFilesRejectsInputThatTheModelCanCorrect(t *testing.T) {
	readFiles := leanTool(t, t.TempDir(), filepath.Join(t.TempDir(), "outputs"), "read_files")
	nine := `{"files":[` + strings.TrimSuffix(strings.Repeat(`{"path":"a"},`, 9), ",") + `]}`
	for name, input := range map[string]string{
		"no files":        `{"files":[]}`,
		"too many files":  nine,
		"no path":         `{"files":[{"offset":2}]}`,
		"offset of zero":  `{"files":[{"path":"a","offset":0}]}`,
		"negative limit":  `{"files":[{"path":"a","limit":-1}]}`,
		"files not array": `{"files":"a.txt"}`,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := readFiles.Run(context.Background(), json.RawMessage(input))
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.HasPrefix(result.Content, "invalid input: ") {
				t.Fatalf("got %+v", result)
			}
		})
	}
}

// The lean set keeps 8 KiB of a large command output in the result and saves
// the rest, where the standard set keeps 32 KiB.
func TestLeanBashKeepsLessOfALargeOutputAndSavesAllOfIt(t *testing.T) {
	root := t.TempDir()
	command := "head -c 20000 /dev/zero | tr '\\0' 'a'"
	standard := runBash(t, lookupWithOutputs(t, root, filepath.Join(t.TempDir(), "outputs"), "bash"), command)
	if strings.Contains(standard.Content, "[output truncated") {
		t.Fatalf("the standard set cut an output of 20,000 bytes")
	}
	outputs := filepath.Join(t.TempDir(), "outputs")
	lean := runBash(t, leanTool(t, root, outputs, "bash"), command)
	if !strings.Contains(lean.Content, "[output truncated: 11808 bytes omitted; full output (20000 bytes) saved to ") {
		t.Fatalf("the lean set did not cut the output at 8 KiB: %.300q", lean.Content)
	}
	files := savedOutputFiles(t, outputs)
	if len(files) != 1 {
		t.Fatalf("saved files = %v", files)
	}
	saved, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 20000 {
		t.Fatalf("saved output has %d bytes, want 20000", len(saved))
	}
}
