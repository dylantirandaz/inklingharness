package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

const (
	defaultCommandTimeout = 120 * time.Second
	maxCommandTimeout     = 600 * time.Second
	// pipeDrainDelay bounds the wait for stdout and stderr after the process
	// group is killed, in case a grandchild still holds the pipes.
	pipeDrainDelay = 2 * time.Second
)

// bashTool saves large outputs in outputDirectory, so a noisy command cannot
// fill the context window and the model can still read the full output.
func bashTool(root, outputDirectory string) Tool {
	return Tool{
		Name:        "bash",
		Description: "Run a bash command in the working directory. Returns stdout and stderr together, then the exit code when it is not zero. Output larger than 32 KiB returns only its start and end, plus the path of a file with the full output; use read_file to read that file. The default timeout is 120 seconds; the maximum is 600.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Command line for bash -c."},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600,"description":"Kill the command after this many seconds. Default 120."}},"required":["command"]}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			var arguments struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Command == "" {
				return invalidInput(errors.New("command is required")), nil
			}
			timeout := defaultCommandTimeout
			if arguments.TimeoutSeconds > 0 {
				timeout = time.Duration(arguments.TimeoutSeconds) * time.Second
			}
			if timeout > maxCommandTimeout {
				return invalidInput(fmt.Errorf("timeout_seconds %d exceeds the maximum of %d", arguments.TimeoutSeconds, int(maxCommandTimeout.Seconds()))), nil
			}
			return runCommand(ctx, root, outputDirectory, arguments.Command, timeout)
		},
	}
}

func runCommand(ctx context.Context, root, outputDirectory, commandLine string, timeout time.Duration) (Result, error) {
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runContext, "/bin/bash", "-c", commandLine)
	command.Dir = root
	// The command gets its own process group, so a timeout or an interrupt
	// kills background children too, not only the shell.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = pipeDrainDelay
	output := newOutputCollector(outputDirectory)
	command.Stdout = output
	command.Stderr = output

	runErr := command.Run()
	if errors.Is(runContext.Err(), context.DeadlineExceeded) {
		return Result{Content: output.finish() + fmt.Sprintf("\n[timed out after %s]", timeout), IsError: true}, nil
	}
	if ctx.Err() != nil {
		return Result{}, discardOutput(output, ctx.Err())
	}
	exitCode := 0
	if runErr != nil {
		var exitError *exec.ExitError
		if !errors.As(runErr, &exitError) {
			return Result{}, discardOutput(output, fmt.Errorf("bash: %w", runErr))
		}
		exitCode = exitError.ExitCode()
	}
	content := output.finish()
	if content == "" {
		content = "(no output)"
	}
	if exitCode != 0 {
		return Result{Content: content + fmt.Sprintf("\n[exit code %d]", exitCode), IsError: true}, nil
	}
	return Result{Content: content}, nil
}

// discardOutput deletes the saved output when the model gets no result, and
// returns cause unchanged when the deletion succeeds.
func discardOutput(output *outputCollector, cause error) error {
	if err := output.discard(); err != nil {
		return errors.Join(cause, fmt.Errorf("bash: remove output file: %w", err))
	}
	return cause
}
