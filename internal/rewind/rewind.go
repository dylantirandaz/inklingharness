// Package rewind takes snapshots of a git working tree and restores them.
//
// A snapshot is a git tree object. Take writes it with a private, temporary
// index file, so the index, HEAD, refs, stash, and config of the user stay
// unchanged. The objects go into the object store of the repository. No ref
// points to them, so "git gc" can prune them after its grace period
// (gc.pruneExpire, two weeks by default). A snapshot is for undo in one
// session, not for long-term storage.
//
// Ignored files are not in a snapshot, and Restore does not touch them.
// Submodules and embedded repositories (gitlinks) are other repositories.
// Restore does not change them.
package rewind

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ErrNotRepository tells that the directory is not inside a git work tree.
var ErrNotRepository = errors.New("rewind: not inside a git work tree")

// Snapshot identifies the state of a working tree.
type Snapshot struct {
	// Tree is a git tree object id. The empty string means no snapshot.
	Tree string
}

// Take records the tracked and untracked, not ignored, files of the work tree
// that contains workDir.
func Take(ctx context.Context, workDir string) (Snapshot, error) {
	top, err := topLevel(ctx, workDir)
	if err != nil {
		return Snapshot{}, err
	}
	return takeAt(ctx, top)
}

// Restore makes the files of the work tree that contains workDir equal to
// target. It returns the changed paths, relative to the top level of the
// repository and sorted. If ctx stops the work, some files can be restored
// and others not.
func Restore(ctx context.Context, workDir string, target Snapshot) ([]string, error) {
	if target.Tree == "" {
		return nil, errors.New("rewind: restore needs a snapshot, but the snapshot is empty")
	}
	top, err := topLevel(ctx, workDir)
	if err != nil {
		return nil, err
	}
	current, err := takeAt(ctx, top)
	if err != nil {
		return nil, err
	}
	if current.Tree == target.Tree {
		return nil, nil
	}
	changes, err := diffTrees(ctx, top, current.Tree, target.Tree)
	if err != nil {
		return nil, err
	}
	// Removals go first. Then a path can change from a file to a directory,
	// or from a directory to a file.
	for _, change := range changes {
		if change.targetMode == modeNone {
			if err := removeEntry(top, change.path); err != nil {
				return nil, err
			}
		}
	}
	for _, change := range changes {
		if change.targetMode != modeNone {
			if err := writeEntry(ctx, top, change); err != nil {
				return nil, err
			}
		}
	}
	changed := make([]string, len(changes))
	for index, change := range changes {
		changed[index] = change.path
	}
	sort.Strings(changed)
	return changed, nil
}

func topLevel(ctx context.Context, workDir string) (string, error) {
	// The C locale gives English messages, so the check of stderr is reliable.
	output, err := runGit(ctx, workDir, []string{"LC_ALL=C"}, "rev-parse", "--show-toplevel")
	var commandErr *commandError
	if errors.As(err, &commandErr) &&
		(strings.Contains(commandErr.stderr, "not a git repository") ||
			strings.Contains(commandErr.stderr, "must be run in a work tree")) {
		return "", ErrNotRepository
	}
	if err != nil {
		return "", err
	}
	top := strings.TrimSuffix(string(output), "\n")
	if top == "" {
		return "", ErrNotRepository
	}
	return top, nil
}

func takeAt(ctx context.Context, top string) (Snapshot, error) {
	output, err := runGit(ctx, top, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return Snapshot{}, err
	}
	userIndex := strings.TrimSuffix(string(output), "\n")
	if !filepath.IsAbs(userIndex) {
		userIndex = filepath.Join(top, userIndex)
	}
	// A private directory also holds the "index.lock" file that git makes.
	scratch, err := os.MkdirTemp("", "think-rewind-")
	if err != nil {
		return Snapshot{}, fmt.Errorf("rewind: make temporary directory: %w", err)
	}
	defer os.RemoveAll(scratch)
	scratchIndex := filepath.Join(scratch, "index")
	if err := copyIndex(userIndex, scratchIndex); err != nil {
		return Snapshot{}, err
	}
	environment := []string{"GIT_INDEX_FILE=" + scratchIndex}
	if _, err := runGit(ctx, top, environment, "add", "-A"); err != nil {
		return Snapshot{}, err
	}
	output, err = runGit(ctx, top, environment, "write-tree")
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Tree: strings.TrimSuffix(string(output), "\n")}, nil
}

// copyIndex copies the index of the user, if it exists. The stat data in it
// lets "git add" skip files that did not change. The copy keeps the
// modification time of the source, because git compares file times with the
// index time to find racily clean entries.
func copyIndex(source, destination string) error {
	input, err := os.Open(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rewind: open index: %w", err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("rewind: read index: %w", err)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("rewind: copy index: %w", err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("rewind: copy index: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("rewind: copy index: %w", err)
	}
	if err := os.Chtimes(destination, info.ModTime(), info.ModTime()); err != nil {
		return fmt.Errorf("rewind: copy index: %w", err)
	}
	return nil
}

type entryMode int

const (
	modeNone entryMode = iota
	modeFile
	modeExecutable
	modeSymlink
	modeGitlink
)

func parseMode(text string) (entryMode, error) {
	switch text {
	case "000000":
		return modeNone, nil
	case "100644":
		return modeFile, nil
	case "100755":
		return modeExecutable, nil
	case "120000":
		return modeSymlink, nil
	case "160000":
		return modeGitlink, nil
	default:
		return modeNone, fmt.Errorf("rewind: unknown git mode %q", text)
	}
}

type change struct {
	path         string
	targetMode   entryMode
	targetObject string
}

// diffTrees lists the paths that differ between the two trees. It leaves out
// gitlinks, because Restore does not change other repositories.
func diffTrees(ctx context.Context, top, current, target string) ([]change, error) {
	output, err := runGit(ctx, top, nil, "diff-tree", "-r", "-z", "--no-renames", current, target)
	if err != nil {
		return nil, err
	}
	// Each record is ":<mode> <mode> <object> <object> <status>\0<path>\0".
	fields := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("rewind: unexpected git diff-tree output %q", output)
	}
	changes := make([]change, 0, len(fields)/2)
	for index := 0; index < len(fields); index += 2 {
		entry, gitlink, err := parseRecord(fields[index], fields[index+1])
		if err != nil {
			return nil, err
		}
		if !gitlink {
			changes = append(changes, entry)
		}
	}
	return changes, nil
}

// parseRecord also tells if one side of the record is a gitlink.
func parseRecord(header, entryPath string) (change, bool, error) {
	parts := strings.Fields(strings.TrimPrefix(header, ":"))
	if !strings.HasPrefix(header, ":") || len(parts) != 5 {
		return change{}, false, fmt.Errorf("rewind: unexpected git diff-tree record %q", header)
	}
	switch parts[4] {
	case "A", "D", "M", "T":
	default:
		return change{}, false, fmt.Errorf("rewind: unexpected git diff-tree status %q for %q", parts[4], entryPath)
	}
	// Git does not make such paths, but a bad path must never leave the tree.
	if !filepath.IsLocal(filepath.FromSlash(entryPath)) {
		return change{}, false, fmt.Errorf("rewind: path %q is not inside the work tree", entryPath)
	}
	currentMode, err := parseMode(parts[0])
	if err != nil {
		return change{}, false, err
	}
	targetMode, err := parseMode(parts[1])
	if err != nil {
		return change{}, false, err
	}
	entry := change{path: entryPath, targetMode: targetMode, targetObject: parts[3]}
	return entry, currentMode == modeGitlink || targetMode == modeGitlink, nil
}

func removeEntry(top, entryPath string) error {
	err := os.Remove(filepath.Join(top, filepath.FromSlash(entryPath)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("rewind: remove %s: %w", entryPath, err)
	}
	for directory := path.Dir(entryPath); directory != "."; directory = path.Dir(directory) {
		err := os.Remove(filepath.Join(top, filepath.FromSlash(directory)))
		switch {
		case err == nil, errors.Is(err, fs.ErrNotExist):
		case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
			return nil
		default:
			return fmt.Errorf("rewind: remove directory %s: %w", directory, err)
		}
	}
	return nil
}

func writeEntry(ctx context.Context, top string, entry change) error {
	if err := makeParentDirectories(top, entry.path); err != nil {
		return err
	}
	destination := filepath.Join(top, filepath.FromSlash(entry.path))
	switch entry.targetMode {
	case modeFile:
		return writeFile(ctx, top, entry, destination, 0o644)
	case modeExecutable:
		return writeFile(ctx, top, entry, destination, 0o755)
	case modeSymlink:
		return writeSymlink(ctx, top, entry, destination)
	case modeNone, modeGitlink:
		panic(fmt.Sprintf("rewind: writeEntry called for %q with mode %d", entry.path, entry.targetMode))
	default:
		panic(fmt.Sprintf("rewind: unknown entry mode %d", entry.targetMode))
	}
}

// makeParentDirectories makes the missing parent directories of entryPath.
// It refuses a parent that is a symbolic link, so a write cannot go outside
// the work tree.
func makeParentDirectories(top, entryPath string) error {
	directory := path.Dir(entryPath)
	if directory == "." {
		return nil
	}
	current := top
	for _, name := range strings.Split(directory, "/") {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return fmt.Errorf("rewind: make directory for %s: %w", entryPath, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("rewind: check directory for %s: %w", entryPath, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("rewind: cannot write %s: %s is not a directory", entryPath, current)
		}
	}
	return nil
}

// writeFile writes to a temporary file and renames it over the destination.
// The rename replaces a symbolic link and does not write through it.
func writeFile(ctx context.Context, top string, entry change, destination string, permission fs.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".think-rewind-*")
	if err != nil {
		return fmt.Errorf("rewind: write %s: %w", entry.path, err)
	}
	defer os.Remove(temporary.Name())
	// The filters apply the same conversions (for example, line endings) as
	// a checkout, so the content is equal to the content that "git add" read.
	command := gitCommand(ctx, top, nil, "cat-file", "--filters", "--path="+entry.path, entry.targetObject)
	command.Stdout = temporary
	runErr := wrapRun(ctx, command)
	closeErr := temporary.Close()
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return fmt.Errorf("rewind: write %s: %w", entry.path, closeErr)
	}
	if err := os.Chmod(temporary.Name(), permission); err != nil {
		return fmt.Errorf("rewind: write %s: %w", entry.path, err)
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		return fmt.Errorf("rewind: write %s: %w", entry.path, err)
	}
	return nil
}

func writeSymlink(ctx context.Context, top string, entry change, destination string) error {
	linkTarget, err := runGit(ctx, top, nil, "cat-file", "blob", entry.targetObject)
	if err != nil {
		return err
	}
	// CreateTemp reserves a free name. The symbolic link then takes the name.
	reserved, err := os.CreateTemp(filepath.Dir(destination), ".think-rewind-*")
	if err != nil {
		return fmt.Errorf("rewind: write %s: %w", entry.path, err)
	}
	temporaryName := reserved.Name()
	reserved.Close()
	if err := os.Remove(temporaryName); err != nil {
		return fmt.Errorf("rewind: write %s: %w", entry.path, err)
	}
	if err := os.Symlink(string(linkTarget), temporaryName); err != nil {
		return fmt.Errorf("rewind: write %s: %w", entry.path, err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		os.Remove(temporaryName)
		return fmt.Errorf("rewind: write %s: %w", entry.path, err)
	}
	return nil
}

type commandError struct {
	arguments []string
	stderr    string
	err       error
}

func (e *commandError) Error() string {
	message := fmt.Sprintf("rewind: git %s: %v", strings.Join(e.arguments, " "), e.err)
	if e.stderr != "" {
		message += ": " + e.stderr
	}
	return message
}

func (e *commandError) Unwrap() error { return e.err }

func gitCommand(ctx context.Context, directory string, extraEnvironment []string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	// Later entries take precedence, so these values replace the values of the user.
	environment := append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	command.Env = append(environment, extraEnvironment...)
	return command
}

func runGit(ctx context.Context, directory string, extraEnvironment []string, arguments ...string) ([]byte, error) {
	command := gitCommand(ctx, directory, extraEnvironment, arguments...)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := wrapRun(ctx, command); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func wrapRun(ctx context.Context, command *exec.Cmd) error {
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return nil
	}
	// A killed process only reports a signal. The context tells why.
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	return &commandError{
		arguments: command.Args[1:],
		stderr:    strings.TrimSpace(stderr.String()),
		err:       err,
	}
}
