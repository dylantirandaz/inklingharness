package project

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileList(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "main.go", "pkg/a b.go", ".hidden/secret", ".env", "node_modules/x/index.js", "line\nbreak.go")
	plain, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"line\nbreak.go", "main.go", "pkg/a b.go"}; !slices.Equal(plain.Files, want) {
		t.Fatalf("files outside git = %q, want %q", plain.Files, want)
	}
	prompt := plain.SystemPrompt()
	if !strings.Contains(prompt, `"line\nbreak.go" "main.go" "pkg/a b.go"`) || strings.Count(prompt, "\nEND GIT SNAPSHOT DATA\n") != 1 {
		t.Fatalf("file names escaped their quotes: %q", prompt)
	}

	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	writeFiles(t, root, "build/out.bin", ".gitignore")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("build/\nnode_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracked, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	// The same files as outside git: build/ is ignored and hidden paths are
	// left out.
	if want := []string{"line\nbreak.go", "main.go", "pkg/a b.go"}; !slices.Equal(tracked.Files, want) {
		t.Fatalf("files in git = %q, want %q", tracked.Files, want)
	}

	large := t.TempDir()
	for index := range fileListLimit + 1 {
		writeFiles(t, large, fmt.Sprintf("f%03d.go", index))
	}
	big, err := Inspect(context.Background(), large)
	if err != nil {
		t.Fatal(err)
	}
	if big.Files != nil || strings.Contains(big.SystemPrompt(), "Files at session start") {
		t.Fatalf("a project above the limit got a list of %d files", len(big.Files))
	}
}
