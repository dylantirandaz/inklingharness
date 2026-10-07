package extend

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// GoalTimeout bounds one run of a goal check, such as a test suite.
	GoalTimeout = 10 * time.Minute
	// goalTailLimit is the end of the check output that the model sees.
	goalTailLimit = 4 << 10
	// goalCaptureLimit bounds the memory for the output of one check.
	goalCaptureLimit = 1 << 20
)

// GoalResult is the outcome of one run of a goal check.
type GoalResult struct {
	Passed bool
	// Status is "exit code N", "killed by a signal", or "timed out after D".
	Status string
	// Tail is the end of stdout and stderr together, at most 4 KiB.
	Tail string
}

// CheckGoal runs the user's goal command with bash -c in workDir, in its own
// process group. Exit code 0 means that the goal is met. The error is not
// nil only when the shell cannot run or ctx ends.
func CheckGoal(ctx context.Context, workDir, command string) (GoalResult, error) {
	return checkGoal(ctx, workDir, command, GoalTimeout)
}

func checkGoal(ctx context.Context, workDir, command string, timeout time.Duration) (GoalResult, error) {
	output := &tailBuffer{limit: goalCaptureLimit}
	status, err := runShell(ctx, workDir, command, nil, output, output, timeout)
	if err != nil {
		return GoalResult{}, fmt.Errorf("goal check %q: %w", command, err)
	}
	return GoalResult{
		Passed: !status.timedOut && status.code == 0,
		Status: status.describe(timeout),
		Tail:   output.tail(goalTailLimit),
	}, nil
}

// tailBuffer keeps the last limit bytes of a stream, because the end of a
// failed check holds the summary and the exit reason.
type tailBuffer struct {
	limit   int
	content []byte
	dropped bool
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.content = append(b.content, data...)
	if extra := len(b.content) - b.limit; extra > 0 {
		b.content = append(b.content[:0], b.content[extra:]...)
		b.dropped = true
	}
	return len(data), nil
}

// tail returns at most size bytes from the end, starting at a whole UTF-8
// character and a whole line when one starts within the cut.
func (b *tailBuffer) tail(size int) string {
	text := b.content
	cut := b.dropped
	if len(text) > size {
		text = text[len(text)-size:]
		cut = true
	}
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	if cut {
		if newline := strings.IndexByte(string(text), '\n'); newline >= 0 && newline < len(text)-1 {
			text = text[newline+1:]
		}
		return "[earlier output omitted]\n" + strings.TrimRight(string(text), "\n")
	}
	return strings.TrimRight(string(text), "\n")
}
