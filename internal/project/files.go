package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// fileListLimit is the largest project whose file list goes into the
// context. The list spares the model a first turn that only lists files; in
// a larger project the list costs more tokens in every request than that turn.
const fileListLimit = 200

var errTooManyFiles = errors.New("too many files")

// listFiles returns the sorted file paths below workDir, relative to it, or
// nil when there are more than fileListLimit. In a git repository it lists
// tracked and untracked files that .gitignore does not exclude; elsewhere it
// walks the folder. Both leave out hidden paths and node_modules.
func listFiles(ctx context.Context, workDir string, inGit bool) ([]string, error) {
	if inGit {
		output, stderr, err := gitCommand(ctx, workDir, "ls-files", "--cached", "--others", "--exclude-standard")
		if err != nil {
			return nil, gitError("list files", stderr, err)
		}
		if strings.HasSuffix(output, truncationMarker) {
			return nil, nil
		}
		var files []string
		for _, line := range strings.FieldsFunc(output, func(character rune) bool { return character == '\n' }) {
			// core.quotePath quotes a name with special characters.
			if strings.HasPrefix(line, `"`) {
				unquoted, err := strconv.Unquote(line)
				if err != nil {
					return nil, fmt.Errorf("git file name %s: %w", line, err)
				}
				line = unquoted
			}
			if !hidden(line) {
				files = append(files, line)
			}
		}
		if len(files) > fileListLimit {
			return nil, nil
		}
		slices.Sort(files)
		return slices.Compact(files), nil
	}
	var files []string
	err := filepath.WalkDir(workDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil && path != workDir {
			// A folder that cannot be read is left out of the list.
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == workDir {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if len(files) == fileListLimit {
			return errTooManyFiles
		}
		relative, err := filepath.Rel(workDir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if errors.Is(err, errTooManyFiles) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return files, nil
}

// hidden reports whether a slash-separated path has a part that starts with
// a dot or is node_modules; such files are left out of the list.
func hidden(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		if strings.HasPrefix(part, ".") || part == "node_modules" {
			return true
		}
	}
	return false
}
