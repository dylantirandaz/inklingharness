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
		Description: "Run a bash command in the working directory. Returns stdout and stderr together, then the exit code when it is not zero. Output larger than 32 KiB returns only its start and end, plus the path of a file with the full output; use read_file to read that file. The default timeout is 120 seconds; the maximum is 600. For a command that does not stop by itself, for example a dev server or a watcher, set background to true: the command then runs as a job, the call returns at once, and bash_job reads the job output or stops the job. At most 8 jobs run at the same time.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Command line for bash -c."},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600,"description":"Kill the command after this many seconds. Default 120. Not for background commands."},"background":{"type":"boolean","description":"Run the command as a background job and return at once. Default false."}},"required":["command"]}`),
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
