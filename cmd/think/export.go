package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/transcript"
)

// exportSession writes the session as a self-contained HTML page and returns
// its absolute path. A relative target resolves against the session's
// directory; no target names the file after the session.
func exportSession(current session.Session, target string, now time.Time) (string, error) {
	if target == "" {
		short := current.ID
		if len(short) > 8 {
			short = short[:8]
		}
		target = "inkling-session-" + short + ".html"
	}
	if !strings.HasSuffix(strings.ToLower(target), ".html") {
		target += ".html"
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(current.WorkDir, target)
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".inkling-export-*")
	if err != nil {
		return "", fmt.Errorf("export: %w", err)
	}
	defer os.Remove(file.Name())
	if err := transcript.Write(file, current, now); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		return "", fmt.Errorf("export: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("export: %w", err)
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return "", fmt.Errorf("export: %w", err)
	}
	return target, nil
}
