package main

import (
	"os"
	"path/filepath"
	"strings"
)

// homeRelative writes a path under the home folder as "~/…", as a shell
// prompt does, so the footer says "~/code/harbor" and not the whole path.
func homeRelative(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, found := strings.CutPrefix(path, home+string(filepath.Separator)); found {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// gitBranch is the branch that the repository around dir has checked out. A
// detached HEAD gives its short hash, and a folder outside a repository gives
// the empty string. It reads the HEAD file and starts no process, so the
// footer can ask after every turn.
func gitBranch(dir string) string {
	head := headFile(dir)
	if head == "" {
		return ""
	}
	data, err := os.ReadFile(head)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	if branch, found := strings.CutPrefix(content, "ref: refs/heads/"); found {
		return branch
	}
	if _, found := strings.CutPrefix(content, "ref: "); found {
		return ""
	}
	return content[:min(len(content), 7)]
}

// headFile finds the HEAD file of the repository around dir. A worktree or a
// submodule has a .git file that names the real folder.
func headFile(dir string) string {
	for {
		marker := filepath.Join(dir, ".git")
		info, err := os.Stat(marker)
		switch {
		case err == nil && info.IsDir():
			return filepath.Join(marker, "HEAD")
		case err == nil:
			data, readErr := os.ReadFile(marker)
			if readErr != nil {
				return ""
			}
			target, found := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
			if !found {
				return ""
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			return filepath.Join(target, "HEAD")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
