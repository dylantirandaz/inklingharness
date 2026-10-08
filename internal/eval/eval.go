// Package eval runs a fixed task set and measures pass rate, tokens, and
// latency, so changes to prompts and models can be compared.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/attachment"
	"github.com/dylantirandaz/inklingharness/internal/latency"
	"github.com/dylantirandaz/inklingharness/internal/project"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const checkTimeout = 120 * time.Second

// Task is one line of a tasks.jsonl file.
type Task struct {
	Name   string   `json:"name"`
	Prompt string   `json:"prompt"`
	Files  []string `json:"files,omitempty"`
	// Template is a directory, relative to the task file, that is copied into
	// a fresh working directory before the run. Empty means an empty directory.
	Template string `json:"template"`
	// Check is a bash command that runs in the working directory after the
	// run. Exit code 0 means the task passed.
	Check string `json:"check"`
}

// Result is the measured outcome of one task.
type Result struct {
	Task          string          `json:"task"`
	Pass          bool            `json:"pass"`
	Error         string          `json:"error,omitempty"`
	CheckOutput   string          `json:"check_output,omitempty"`
	Turns         int             `json:"turns"`
	Usage         anthropic.Usage `json:"usage"`
	Wall          time.Duration   `json:"wall_ns"`
	TurnLatencies []time.Duration `json:"turn_latencies_ns"`
	WorkDir       string          `json:"workdir,omitempty"`
}

// LoadTasks reads a JSONL task file. Blank lines are skipped.
func LoadTasks(path string) ([]Task, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var tasks []Task
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var task Task
		if err := json.Unmarshal([]byte(line), &task); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
		if task.Name == "" || task.Prompt == "" || task.Check == "" {
			return nil, fmt.Errorf("%s:%d: name, prompt, and check are required", path, lineNumber)
		}
		tasks = append(tasks, task)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return tasks, nil
}

// Setup builds the tools of one task and changes its configuration, as a
// real session would. It gets the fresh working directory, the output
// directory, and the background jobs, which RunTask closes.
type Setup func(workDir, outputDirectory string, jobs *tools.Jobs, config *agent.Config) (*tools.Set, error)

// RunTask runs one task in a fresh working directory and checks the result.
// Bash output files go to a separate temporary directory, so the task check
// sees only the files that the agent made and the model sees the same working
// directory path as before output files existed. When keepWorkDir is true both
// directories stay on disk and Result.WorkDir names the working directory.
func RunTask(ctx context.Context, client *anthropic.Client, config agent.Config, setup Setup, task Task, taskFileDir string, keepWorkDir bool) (result Result) {
	result = Result{Task: task.Name}
	workDir, err := os.MkdirTemp("", "inkling-eval-"+task.Name+"-")
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if !keepWorkDir {
		defer os.RemoveAll(workDir)
	}
	outputDirectory, err := os.MkdirTemp("", "inkling-eval-"+task.Name+"-outputs-")
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if !keepWorkDir {
		defer os.RemoveAll(outputDirectory)
	}
	if keepWorkDir {
		result.WorkDir = workDir
	}
	if task.Template != "" {
		if err := os.CopyFS(workDir, os.DirFS(filepath.Join(taskFileDir, task.Template))); err != nil {
			result.Error = fmt.Sprintf("copy template: %v", err)
			return result
		}
	}
	config.ContextStore = session.NewStore(filepath.Join(outputDirectory, "context-store"))
	// This deferred call runs before the directories are removed, so no job
	// writes in a removed directory.
	jobs := tools.NewJobs(outputDirectory)
	defer func() {
		if err := jobs.Close(); err != nil {
			if result.Error != "" {
				result.Error += "; "
			}
			result.Error += "stop background jobs: " + err.Error()
		}
	}()
	toolSet, err := setup(workDir, outputDirectory, jobs, &config)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	projectContext, err := project.Inspect(ctx, workDir)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	config.System += "\n\n" + projectContext.SystemPrompt()
	config.Checkpoint = nil
	started := time.Now()
	preparation := latency.Begin(ctx, latency.Preparation, "attachments")
	prepared, err := attachment.Prepare(ctx, workDir, task.Prompt, task.Files, config.ContextStore)
	preparation.End(err)
	if err != nil {
		result.Error = err.Error()
		result.Wall = time.Since(started)
		return result
	}
	outcome, err := agent.Run(ctx, client, config, toolSet, nil, agent.Prompt{Text: prepared.Prompt}, agent.SilentObserver{})
	result.Wall = time.Since(started)
	result.Turns = outcome.Turns
	result.Usage = outcome.Usage
	result.TurnLatencies = outcome.TurnLatencies
	if err != nil {
		result.Error = err.Error()
		return result
	}

	result.Pass, result.CheckOutput = runCheck(ctx, toolSet, task.Check)
	return result
}

func runCheck(ctx context.Context, toolSet *tools.Set, check string) (passed bool, output string) {
	span := latency.Begin(ctx, latency.Tool, "check")
	if span != nil {
		defer func() {
			err := ctx.Err()
			if err == nil && !passed {
				err = errors.New("evaluation check failed")
			}
			span.End(err)
		}()
	}
	tool, found := toolSet.Lookup("bash")
	if !found {
		return false, "evaluation requires the bash tool"
	}
	input, err := json.Marshal(struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}{Command: check, TimeoutSeconds: int(checkTimeout / time.Second)})
	if err != nil {
		return false, err.Error()
	}
	result, err := tool.Run(ctx, input)
	if err != nil {
		return false, err.Error()
	}
	return !result.IsError, result.Content
}

// Summary aggregates a task set run.
type Summary struct {
	Passed      int
	Total       int
	Usage       anthropic.Usage
	TurnP50     time.Duration
	TurnP95     time.Duration
	TotalTurns  int
	TotalErrors int
}

// Summarize folds the results of one task set run.
func Summarize(results []Result) Summary {
	summary := Summary{Total: len(results)}
	var latencies []time.Duration
	for _, result := range results {
		if result.Pass {
			summary.Passed++
		}
		if result.Error != "" {
			summary.TotalErrors++
		}
		summary.Usage = summary.Usage.Add(result.Usage)
		summary.TotalTurns += result.Turns
		latencies = append(latencies, result.TurnLatencies...)
	}
	summary.TurnP50 = percentile(latencies, 0.50)
	summary.TurnP95 = percentile(latencies, 0.95)
	return summary
}

// percentile returns the nearest-rank percentile, or zero for no samples.
func percentile(samples []time.Duration, fraction float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(fraction*float64(len(sorted))+0.999999) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
