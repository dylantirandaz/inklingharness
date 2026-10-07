package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/session"
)

// pickLimit is the number of recent sessions that the picker shows.
const pickLimit = 20

var errNoSessions = errors.New("no saved sessions in this directory")

// pickSession lists the recent sessions of workDir and reads the number of
// one from input. It runs before the terminal interface starts.
func pickSession(ctx context.Context, store *session.Store, workDir string, input io.Reader, output io.Writer) (string, error) {
	summaries, err := store.List(workDir)
	if err != nil {
		return "", err
	}
	if len(summaries) == 0 {
		return "", errNoSessions
	}
	shown := summaries[:min(len(summaries), pickLimit)]
	for index, summary := range shown {
		fmt.Fprintf(output, "%3d  %s\n", index+1, describeSession(summary))
	}
	for {
		fmt.Fprintf(output, "Session number (1-%d), or Ctrl-D to cancel: ", len(shown))
		line, err := readLine(input)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		number, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && number >= 1 && number <= len(shown) {
			return shown[number-1].ID, nil
		}
	}
}

// readLine reads one line a byte at a time. A buffered or background reader
// would take input that belongs to the terminal interface after the pick.
func readLine(input io.Reader) (string, error) {
	var line []byte
	var buffer [1]byte
	for {
		count, err := input.Read(buffer[:])
		if count == 1 {
			if buffer[0] == '\n' {
				return string(line), nil
			}
			line = append(line, buffer[0])
		}
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("pick a session: %w", err)
		}
		if err != nil {
			return "", err
		}
	}
}

// describeSession is one line for a session list: the time, the title or
// the start of the ID, the size, and the model.
func describeSession(summary session.Summary) string {
	name := summary.Title
	if name == "" {
		name = "(untitled " + summary.ID[:8] + ")"
	}
	return fmt.Sprintf("%s  %-40s %4d messages  %s", summary.UpdatedAt.Local().Format("2006-01-02 15:04"), presentation.Safe(name), summary.MessageCount, presentation.Safe(summary.Model))
}

func describeTitle(title string) string {
	if title == "" {
		return "This session has no title. Use /title TEXT to set one."
	}
	return "Title: " + presentation.Safe(title)
}
