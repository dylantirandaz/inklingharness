package rewind

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "rewind tests need git:", err)
		os.Exit(1)
	}
	// The configuration of the machine must not change the results.
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func runTestGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	// Optional locks would let "git status" and similar commands rewrite the index.
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func newRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runTestGit(t, root, "init", "-q")
	return root
}

func writeTestFile(t *testing.T, root, name string, content []byte, permission fs.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, permission); err != nil {
		t.Fatal(err)
	}
	// WriteFile does not change the mode of a file that exists.
	if err := os.Chmod(path, permission); err != nil {
		t.Fatal(err)
	}
}

func replaceWithSymlink(t *testing.T, root, name, target string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, root, name string, content []byte, permission fs.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != permission {
		t.Fatalf("%s mode = %v, want regular %v", name, info.Mode(), permission)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("%s = %q, want %q", name, got, content)
	}
}

func assertSymlink(t *testing.T, root, name, target string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if got != target {
		t.Fatalf("%s -> %q, want %q", name, got, target)
	}
}

func assertDirectory(t *testing.T, root, name string) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil || !info.IsDir() {
		t.Fatalf("%s is not a directory: %v, %v", name, info, err)
	}
}

func assertMissing(t *testing.T, root, name string) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists, or Lstat failed with another error: %v", name, err)
	}
}

// userState holds the git state of the user that rewind must never change.
type userState struct {
	index      []byte
	indexFound bool
	headFile   string
	refs       string
	stash      string
}

func readUserState(t *testing.T, root string) userState {
	t.Helper()
	index, indexErr := os.ReadFile(filepath.Join(root, ".git", "index"))
	if indexErr != nil && !errors.Is(indexErr, fs.ErrNotExist) {
		t.Fatal(indexErr)
	}
	headFile, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return userState{
		index:      index,
		indexFound: indexErr == nil,
		headFile:   string(headFile),
		refs:       runTestGit(t, root, "for-each-ref"),
		stash:      runTestGit(t, root, "stash", "list"),
	}
}

func assertUserState(t *testing.T, root string, want userState) {
	t.Helper()
	got := readUserState(t, root)
	if !bytes.Equal(got.index, want.index) || got.indexFound != want.indexFound {
		t.Fatalf("index changed: found %v (want %v), %d bytes (want %d)", got.indexFound, want.indexFound, len(got.index), len(want.index))
	}
	if got.headFile != want.headFile || got.refs != want.refs || got.stash != want.stash {
		t.Fatalf("HEAD, refs, or stash changed: got %+v, want %+v", got, want)
	}
}

func TestRestoreBringsBackExactTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := newRepository(t)
	binary := []byte{0, 1, 2, 0xff, 0xfe, '\n', 0, '\r', '\n'}
	writeTestFile(t, root, ".gitignore", []byte("*.log\nignored/\n"), 0o644)
	writeTestFile(t, root, "a.txt", []byte("alpha\n"), 0o644)
	writeTestFile(t, root, "run.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	writeTestFile(t, root, "plain.txt", []byte("plain\n"), 0o644)
	writeTestFile(t, root, "bin.dat", binary, 0o644)
	writeTestFile(t, root, "dir/sub/nested.txt", []byte("nested\n"), 0o644)
	writeTestFile(t, root, "becomes-link.txt", []byte("regular\n"), 0o644)
	writeTestFile(t, root, "swap/inner.txt", []byte("inner\n"), 0o644)
	writeTestFile(t, root, "flip", []byte("flip file\n"), 0o644)
	replaceWithSymlink(t, root, "link", "a.txt")
	runTestGit(t, root, "add", "-A")
	runTestGit(t, root, "commit", "-q", "-m", "base")
	writeTestFile(t, root, "untracked.txt", []byte("untracked\n"), 0o644)
	writeTestFile(t, root, "keep.log", []byte("log before\n"), 0o644)
	writeTestFile(t, root, "ignored/data.txt", []byte("ignored\n"), 0o644)
	before := readUserState(t, root)

	snapshot, err := Take(ctx, filepath.Join(root, "dir", "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tree == "" {
		t.Fatal("Take returned an empty snapshot")
	}
	assertUserState(t, root, before)

	writeTestFile(t, root, "a.txt", []byte("changed\n"), 0o644)
	writeTestFile(t, root, "run.sh", []byte("#!/bin/sh\necho hi\n"), 0o644)
	writeTestFile(t, root, "plain.txt", []byte("plain\n"), 0o755)
	writeTestFile(t, root, "bin.dat", []byte{9, 9, 0}, 0o644)
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	replaceWithSymlink(t, root, "link", "run.sh")
	replaceWithSymlink(t, root, "becomes-link.txt", "a.txt")
	if err := os.RemoveAll(filepath.Join(root, "swap")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "swap", []byte("now a file\n"), 0o644)
	if err := os.Remove(filepath.Join(root, "flip")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "flip/inner.txt", []byte("now a directory\n"), 0o644)
	if err := os.Remove(filepath.Join(root, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "added.txt", []byte("added\n"), 0o644)
	writeTestFile(t, root, "new/deep/file.txt", []byte("deep\n"), 0o644)
	writeTestFile(t, root, "keep.log", []byte("log after\n"), 0o644)
	writeTestFile(t, root, "ignored/new.txt", []byte("new ignored\n"), 0o644)

	changed, err := Restore(ctx, root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"a.txt", "added.txt", "becomes-link.txt", "bin.dat", "dir/sub/nested.txt",
		"flip", "flip/inner.txt", "link", "new/deep/file.txt", "plain.txt",
		"run.sh", "swap", "swap/inner.txt", "untracked.txt",
	}
	if !slices.Equal(changed, want) {
		t.Fatalf("changed = %q\nwant      %q", changed, want)
	}
	assertFile(t, root, "a.txt", []byte("alpha\n"), 0o644)
	assertFile(t, root, "run.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	assertFile(t, root, "plain.txt", []byte("plain\n"), 0o644)
	assertFile(t, root, "bin.dat", binary, 0o644)
	assertFile(t, root, "dir/sub/nested.txt", []byte("nested\n"), 0o644)
	assertFile(t, root, "becomes-link.txt", []byte("regular\n"), 0o644)
	assertSymlink(t, root, "link", "a.txt")
	assertFile(t, root, "swap/inner.txt", []byte("inner\n"), 0o644)
	assertFile(t, root, "flip", []byte("flip file\n"), 0o644)
	assertFile(t, root, "untracked.txt", []byte("untracked\n"), 0o644)
	assertMissing(t, root, "added.txt")
	assertMissing(t, root, "new")
	assertFile(t, root, "keep.log", []byte("log after\n"), 0o644)
	assertFile(t, root, "ignored/data.txt", []byte("ignored\n"), 0o644)
	assertFile(t, root, "ignored/new.txt", []byte("new ignored\n"), 0o644)
	assertUserState(t, root, before)

	again, err := Restore(ctx, root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second restore changed %q", again)
	}
	assertUserState(t, root, before)
}

func TestRestoreKeepsStagedChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := newRepository(t)
	writeTestFile(t, root, "a.txt", []byte("one\n"), 0o644)
	runTestGit(t, root, "add", "a.txt")
	runTestGit(t, root, "commit", "-q", "-m", "base")
	writeTestFile(t, root, "a.txt", []byte("two\n"), 0o644)
	writeTestFile(t, root, "staged-new.txt", []byte("staged\n"), 0o644)
	runTestGit(t, root, "add", "a.txt", "staged-new.txt")
	writeTestFile(t, root, "a.txt", []byte("three\n"), 0o644)
	before := readUserState(t, root)

	snapshot, err := Take(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "a.txt", []byte("four\n"), 0o644)
	if _, err := Restore(ctx, root, snapshot); err != nil {
		t.Fatal(err)
	}

	assertFile(t, root, "a.txt", []byte("three\n"), 0o644)
	assertUserState(t, root, before)
	if staged := runTestGit(t, root, "diff", "--cached", "--name-only"); staged != "a.txt\nstaged-new.txt\n" {
		t.Fatalf("staged paths = %q", staged)
	}
	if content := runTestGit(t, root, "show", ":a.txt"); content != "two\n" {
		t.Fatalf("staged a.txt = %q", content)
	}
}

func TestTakeAndRestoreWithoutIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := newRepository(t)
	writeTestFile(t, root, "f.txt", []byte("first\n"), 0o644)

	snapshot, err := Take(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	assertMissing(t, root, ".git/index")
	writeTestFile(t, root, "f.txt", []byte("second\n"), 0o644)
	changed, err := Restore(ctx, root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"f.txt"}) {
		t.Fatalf("changed = %q", changed)
	}
	assertFile(t, root, "f.txt", []byte("first\n"), 0o644)
	assertMissing(t, root, ".git/index")
}

func TestRestoreToCurrentStateChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := newRepository(t)
	writeTestFile(t, root, "f.txt", []byte("same\n"), 0o644)
	runTestGit(t, root, "add", "f.txt")
	runTestGit(t, root, "commit", "-q", "-m", "base")
	writeTestFile(t, root, "u.txt", []byte("untracked\n"), 0o644)
	snapshot, err := Take(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, err := os.Stat(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}

	changed, err := Restore(ctx, root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %q", changed)
	}
	infoAfter, err := os.Stat(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) || !os.SameFile(infoBefore, infoAfter) {
		t.Fatal("restore rewrote a file that did not change")
	}
}

func TestRestoreReplacesSymlinkedDirectoryWithoutWritingThroughIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := newRepository(t)
	outside := t.TempDir()
	writeTestFile(t, outside, "f.txt", []byte("outside\n"), 0o644)
	writeTestFile(t, root, "d/f.txt", []byte("inside\n"), 0o644)
	snapshot, err := Take(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	replaceWithSymlink(t, root, "d", outside)

	changed, err := Restore(ctx, root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"d", "d/f.txt"}) {
		t.Fatalf("changed = %q", changed)
	}
	assertDirectory(t, root, "d")
	assertFile(t, root, "d/f.txt", []byte("inside\n"), 0o644)
	assertFile(t, outside, "f.txt", []byte("outside\n"), 0o644)
}

func TestRestoreRefusesToWriteThroughIgnoredSymlink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := newRepository(t)
	outside := t.TempDir()
	writeTestFile(t, outside, "f.txt", []byte("outside\n"), 0o644)
	writeTestFile(t, root, "d/f.txt", []byte("inside\n"), 0o644)
	snapshot, err := Take(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	// The ignored symbolic link is not in the current snapshot, so Restore
	// sees "d/f.txt" as a file to add below it.
	writeTestFile(t, root, ".git/info/exclude", []byte("d\n"), 0o644)
	replaceWithSymlink(t, root, "d", outside)

	_, err = Restore(ctx, root, snapshot)
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("Restore error = %v, want a refusal", err)
	}
	assertSymlink(t, root, "d", outside)
	assertFile(t, outside, "f.txt", []byte("outside\n"), 0o644)
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outside directory has %d entries, want 1", len(entries))
	}
}

func TestRestoreRejectsEmptySnapshot(t *testing.T) {
	t.Parallel()
	root := newRepository(t)
	_, err := Restore(context.Background(), root, Snapshot{})
	if err == nil || errors.Is(err, ErrNotRepository) {
		t.Fatalf("Restore error = %v, want an empty snapshot error", err)
	}
}

func TestOutsideRepository(t *testing.T) {
	directory := t.TempDir()
	// Git must not find a repository above the temporary directory.
	t.Setenv("GIT_CEILING_DIRECTORIES", directory)
	ctx := context.Background()

	if _, err := Take(ctx, directory); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Take error = %v, want ErrNotRepository", err)
	}
	emptyTree := Snapshot{Tree: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"}
	if _, err := Restore(ctx, directory, emptyTree); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Restore error = %v, want ErrNotRepository", err)
	}
	gitDirectory := filepath.Join(newRepository(t), ".git")
	if _, err := Take(ctx, gitDirectory); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Take in .git error = %v, want ErrNotRepository", err)
	}
}
