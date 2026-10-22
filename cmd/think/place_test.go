package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// The branch comes from the HEAD file of the repository around the folder,
// found from any folder below it; a detached HEAD gives its short hash, and a
// folder outside a repository gives nothing.
func TestGitBranchReadsHeadFromAnyFolderBelow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/feature/footer\n")
	nested := filepath.Join(root, "internal", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(nested); got != "feature/footer" {
		t.Fatalf("branch = %q", got)
	}
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "0123456789abcdef0123456789abcdef01234567\n")
	if got := gitBranch(root); got != "0123456" {
		t.Fatalf("detached HEAD = %q", got)
	}
	if got := gitBranch(t.TempDir()); got != "" {
		t.Fatalf("a folder outside a repository = %q", got)
	}
}

// A worktree has a .git file that names the real folder, with a path that is
// absolute or relative to the worktree.
func TestGitBranchFollowsAWorktreeFile(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "repo", ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(real, "HEAD"), "ref: refs/heads/side\n")
	worktree := filepath.Join(base, "wt")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+real+"\n")
	if got := gitBranch(worktree); got != "side" {
		t.Fatalf("absolute worktree branch = %q", got)
	}
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: ../repo/.git/worktrees/wt\n")
	if got := gitBranch(worktree); got != "side" {
		t.Fatalf("relative worktree branch = %q", got)
	}
}

func TestHomeRelativeShortensOnlyPathsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for path, want := range map[string]string{
		home:                                "~",
		filepath.Join(home, "code", "a"):    "~/code/a",
		home + "-other":                     home + "-other",
		"/somewhere/else":                   "/somewhere/else",
		strings.TrimSuffix(home, "/") + "x": strings.TrimSuffix(home, "/") + "x",
	} {
		if got := homeRelative(path); got != want {
			t.Errorf("homeRelative(%q) = %q, want %q", path, got, want)
		}
	}
}

// A sent prompt sits on a band whose leading edge leans forward: a blank
// row, a padding row, the rows of the prompt with the time at the end of the
// first, and a padding row. The text keeps one column, and no row leaves the
// live area. Without a tint it stays a plain marked row.
func TestPrintUserEchoesABand(t *testing.T) {
	var card bytes.Buffer
	printUser(&card, "first line\n"+strings.Repeat("word ", 30), 60, tintedTheme)
	rows := strings.Split(strings.TrimRight(card.String(), "\n"), "\n")
	if len(rows) < 6 || rows[0] != "" || presentation.Safe(rows[1]) != "  ▞" || presentation.Safe(rows[len(rows)-1]) != "▌" {
		t.Fatalf("band = %q", rows)
	}
	for index, row := range rows[2 : len(rows)-1] {
		plain := []rune(presentation.Safe(row))
		if edge := strings.TrimSpace(string(plain[:min(3, len(plain))])); presentation.CellWidth(string(plain)) > 59 || len(plain) < 4 || plain[3] == ' ' || edge != "▞" && edge != "▌" {
			t.Fatalf("prompt row %d leaves the live area or the text column: %q", index, string(plain))
		}
	}
	if first := presentation.Safe(rows[2]); !strings.HasPrefix(first, " ▞ first line") {
		t.Fatalf("first row = %q", first)
	}

	var plain bytes.Buffer
	printUser(&plain, "hello", 60, plainTheme)
	if got := presentation.Safe(plain.String()); !strings.Contains(got, "› hello") {
		t.Fatalf("plain prompt = %q", got)
	}
}
