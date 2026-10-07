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

// bashTool saves large outputs in the output directory of jobs, so a noisy
// command cannot fill the context window and the model can still read the
// full output. A background command becomes a job in jobs.
func bashTool(root string, jobs *Jobs) Tool {
	return Tool{
		Name:        "bash",
		Description: "Run a bash command in the working directory. Returns stdout and stderr, and the exit code when not 0. Output over 32 KiB keeps its start and end and names a file with all of it. Timeout: 120 s by default, 600 at most. For servers and watchers set background, then use bash_job.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600},"background":{"type":"boolean"}},"required":["command"]}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			var arguments struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
				Background     bool   `json:"background"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Command == "" {
				return invalidInput(errors.New("command is required")), nil
			}
			if arguments.Background {
				if arguments.TimeoutSeconds != 0 {
					return invalidInput(errors.New("timeout_seconds does not apply to a background command; stop the job with bash_job action kill")), nil
				}
				// An interrupted turn must not leave a new process behind.
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
				return jobs.start(root, arguments.Command)
			}
			timeout := defaultCommandTimeout
			if arguments.TimeoutSeconds > 0 {
				timeout = time.Duration(arguments.TimeoutSeconds) * time.Second
			}
			if timeout > maxCommandTimeout {
				return invalidInput(fmt.Errorf("timeout_seconds %d exceeds the maximum of %d", arguments.TimeoutSeconds, int(maxCommandTimeout.Seconds()))), nil
			}
			return runCommand(ctx, root, jobs, arguments.Command, timeout)
		},
	}
}

func runCommand(ctx context.Context, root string, jobs *Jobs, commandLine string, timeout time.Duration) (Result, error) {
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
	output := newOutputCollector(jobs.outputDirectory, jobs.writing)
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
