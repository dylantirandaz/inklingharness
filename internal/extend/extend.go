// Package extend loads user extensions from plain files and runs hook
// commands. Extensions are settings, prompt commands, skills, and custom
// tools. There is no plugin runtime: every extension is a file or an
// executable that runs with bash -c.
//
// Extensions come from two directories. The user directory applies to all
// projects. The project directory is .inkling in the working directory. When
// both directories define the same name, the project definition wins.
package extend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const projectDirName = ".inkling"

// UserDir resolves $XDG_CONFIG_HOME/inkling, or ~/.config/inkling when
// XDG_CONFIG_HOME is not set. It uses the same rule as the credential store,
// so all user files are in one directory.
func UserDir() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("extend: find home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "inkling"), nil
}

// layerDirs returns the user directory first, so that a later project entry
// overrides an earlier user entry.
func layerDirs(userDir, workDir string) [2]string {
	return [2]string{userDir, filepath.Join(workDir, projectDirName)}
}

// readDirIfExists returns no entries for a missing directory, because an
// extension directory is optional.
func readDirIfExists(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("extend: read %s: %w", dir, err)
	}
	return entries, nil
}

// decodeStrict rejects unknown fields and trailing data, so a misspelled
// key fails loudly instead of being silently ignored.
func decodeStrict(path string, content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("extend: parse %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("extend: parse %s: data after the JSON value", path)
	}
	return nil
}

// isValidName accepts [a-z0-9][a-z0-9_-]*. It does not use regexp, so the
// package costs nothing at startup.
func isValidName(name string) bool {
	if name == "" || !isLowerAlphanumeric(name[0]) {
		return false
	}
	for index := 1; index < len(name); index++ {
		character := name[index]
		if !isLowerAlphanumeric(character) && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func isLowerAlphanumeric(character byte) bool {
	return ('a' <= character && character <= 'z') || ('0' <= character && character <= '9')
}
