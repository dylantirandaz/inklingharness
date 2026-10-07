package extend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// pipeDrainDelay bounds the wait for output pipes after the shell exits or
// is killed, in case a background child still holds them.
const pipeDrainDelay = 2 * time.Second

// exitStatus is the outcome of a shell that ran to the end or timed out.
type exitStatus struct {
	timedOut bool
	// code is -1 when a signal ended the shell.
	code int
}

func (s exitStatus) describe(timeout time.Duration) string {
	switch {
	case s.timedOut:
		return fmt.Sprintf("timed out after %s", timeout)
	case s.code == -1:
		return "killed by a signal"
	default:
		return fmt.Sprintf("exit code %d", s.code)
	}
}

// runShell runs commandLine with bash -c in workDir. The shell gets its own
// process group, so a timeout or a cancel kills background children too. A
// nil stdout or stderr goes to the null device. The error is not nil only
// when the shell cannot run or ctx ends.
func runShell(ctx context.Context, workDir, commandLine string, stdin []byte, stdout, stderr io.Writer, timeout time.Duration) (exitStatus, error) {
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runContext, "/bin/bash", "-c", commandLine)
	command.Dir = workDir
	command.Stdin = bytes.NewReader(stdin)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = pipeDrainDelay

	runErr := command.Run()
	if ctx.Err() != nil {
		return exitStatus{}, ctx.Err()
	}
	if errors.Is(runContext.Err(), context.DeadlineExceeded) {
		return exitStatus{timedOut: true}, nil
	}
	var exitError *exec.ExitError
	switch {
	case runErr == nil:
		return exitStatus{code: 0}, nil
	case errors.As(runErr, &exitError):
		return exitStatus{code: exitError.ExitCode()}, nil
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The shell exited, but a background child kept the pipes open.
		return exitStatus{code: command.ProcessState.ExitCode()}, nil
	default:
		return exitStatus{}, fmt.Errorf("run %q: %w", commandLine, runErr)
	}
}

// cappedBuffer keeps the first limit bytes and counts the rest, so a noisy
// command cannot use unbounded memory.
type cappedBuffer struct {
	limit   int
	content []byte
	total   int64
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	b.total += int64(len(data))
	if room := b.limit - len(b.content); room > 0 {
		b.content = append(b.content, data[:min(room, len(data))]...)
	}
	return len(data), nil
}

func (b *cappedBuffer) truncated() bool {
	return b.total > int64(len(b.content))
}

func (b *cappedBuffer) String() string {
	return string(b.content)
}
