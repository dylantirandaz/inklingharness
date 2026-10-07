// Command think runs a coding agent on Inkling through OpenRouter.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/credentials"
	"github.com/dylantirandaz/inklingharness/internal/eval"
	"github.com/dylantirandaz/inklingharness/internal/latency"
	"github.com/dylantirandaz/inklingharness/internal/openrouter"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/record"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/terminal"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const usageText = `usage:
  think                            open an interactive session
  think chat [flags]               chat; use -resume last or -resume ID
  think sessions                   list saved sessions in this directory
  think sessions export ID         write a session in the format of older releases
  think login [flags]              check and store an OpenRouter API key
  think run [flags] <prompt...>    run one task in the current directory; -json for events, -plan to only plan
  think rpc [flags]                serve one conversation to an editor over stdin and stdout (JSON lines)
  think acp [flags]                serve editors that speak the Agent Client Protocol, such as Zed
  think eval -yes [flags] <file>   run a JSONL task set with tool approval
  think timings <file>             summarize stage timing JSONL records

The key comes from OPENROUTER_API_KEY, else from the file written by login.
File changes and commands need approval. -yes allows them without a prompt.
Tools are not a sandbox. The default is paid Inkling Small; OpenRouter credits are required.
Run a command with -h for its flags.
`

const keyEnvironmentVariable = "OPENROUTER_API_KEY"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		args = []string{"chat"}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	if args[0] != "chat" {
		var stopInterrupt context.CancelFunc
		ctx, stopInterrupt = signal.NotifyContext(ctx, os.Interrupt)
		defer stopInterrupt()
	}
	switch args[0] {
	case "chat":
		return chatCommand(ctx, args[1:], stdin, stdout, stderr)
	case "sessions":
		return sessionsCommand(args[1:], stdout, stderr)
	case "login":
		return loginCommand(ctx, args[1:], stdin, stderr)
	case "run":
		return runCommand(ctx, args[1:], stdin, stdout, stderr)
	case "rpc":
		return rpcCommand(ctx, args[1:], stdin, stdout, stderr)
	case "acp":
		return acpCommand(ctx, args[1:], stdin, stdout, stderr)
	case "eval":
		return evalCommand(ctx, args[1:], stdout, stderr)
	case "timings":
		return timingsCommand(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	default:
		fmt.Fprint(stderr, usageText)
		return 2
	}
}

// effortLevels are the values the Messages endpoint accepts in
// output_config.effort.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

type options struct {
	model         string
	maxTokens     int
	systemFile    string
	effort        string
	thinking      string
	extra         string
	maxTurns      int
	recordDir     string
	baseURL       string
	showThinking  bool
	approveAll    bool
	compactTokens int
	enableTasks   bool
	verbose       bool
	plain         bool
	files         filePaths
	timings       string
	cpuProfile    string
	runtimeTrace  string
}

func bindOptions(flags *flag.FlagSet) *options {
	var o options
	flags.StringVar(&o.model, "model", openrouter.DefaultModel, "model ID")
	flags.IntVar(&o.maxTokens, "max-tokens", 16384, "max_tokens for each reply; reasoning tokens count against it")
	flags.StringVar(&o.systemFile, "system", "", "system prompt file; default uses built-in coding guidance")
	flags.StringVar(&o.effort, "effort", "", "reasoning effort: "+strings.Join(effortLevels, ", ")+"; empty keeps the model default")
	flags.StringVar(&o.thinking, "thinking", "", `raw JSON for the "thinking" request field, for example {"type":"disabled"}`)
	flags.StringVar(&o.extra, "extra", "", "JSON object of extra top-level request fields")
	flags.IntVar(&o.maxTurns, "max-turns", 50, "stop after this many model turns")
	flags.StringVar(&o.recordDir, "record", "", "directory that receives each request body and raw reply stream")
	flags.StringVar(&o.baseURL, "base-url", openrouter.BaseURL, "API root; /v1/messages is appended")
	flags.BoolVar(&o.showThinking, "show-thinking", false, "print reasoning text to stderr as it streams")
	flags.BoolVar(&o.approveAll, "yes", false, "allow file changes and commands without a prompt; no sandbox")
	flags.IntVar(&o.compactTokens, "compact-tokens", 200000, "compact older context at this estimated token count; 0 disables it")
	flags.BoolVar(&o.enableTasks, "tasks", true, "allow read-only research subagents")
	flags.BoolVar(&o.verbose, "verbose", false, "show per-turn protocol details in chat and run")
	flags.BoolVar(&o.plain, "plain", false, "use plain line input instead of the terminal interface")
	flags.StringVar(&o.timings, "timings", "", "append private stage timing records to this JSONL file")
	flags.StringVar(&o.cpuProfile, "cpu-profile", "", "write a CPU profile to a new private file")
	flags.StringVar(&o.runtimeTrace, "runtime-trace", "", "write a Go runtime trace to a new private file")
	return &o
}

func (o *options) agentConfig() (agent.Config, error) {
	if o.maxTokens <= 0 || o.maxTurns <= 0 || o.compactTokens < 0 || strings.TrimSpace(o.model) == "" {
		return agent.Config{}, errors.New("model, positive token/turn limits, and nonnegative compact-tokens are required")
	}
	config := agent.Config{Model: o.model, MaxTokens: o.maxTokens, MaxTurns: o.maxTurns, System: agent.CodingInstructions, CompactTokens: o.compactTokens, EnableTasks: o.enableTasks}
	if o.systemFile != "" {
		system, err := os.ReadFile(o.systemFile)
		if err != nil {
			return agent.Config{}, fmt.Errorf("-system: %w", err)
		}
		config.System = string(system)
	}
	if o.thinking != "" {
		if !json.Valid([]byte(o.thinking)) {
			return agent.Config{}, fmt.Errorf("-thinking is not valid JSON")
		}
		config.Thinking = json.RawMessage(o.thinking)
	}
	if o.extra != "" {
		if err := json.Unmarshal([]byte(o.extra), &config.Extra); err != nil {
			return agent.Config{}, fmt.Errorf("-extra: %w", err)
		}
	}
	if o.effort != "" {
		outputConfig, err := effortField(o.effort)
		if err != nil {
			return agent.Config{}, err
		}
		if config.Extra == nil {
			config.Extra = map[string]json.RawMessage{}
		}
		if _, taken := config.Extra["output_config"]; taken {
			return agent.Config{}, fmt.Errorf("-effort and -extra output_config both set; use one")
		}
		config.Extra["output_config"] = outputConfig
	}
	return config, nil
}

func effortField(effort string) (json.RawMessage, error) {
	for _, level := range effortLevels {
		if effort == level {
			return json.RawMessage(fmt.Sprintf(`{"effort":%q}`, effort)), nil
		}
	}
	return nil, fmt.Errorf("-effort %q is not one of %s", effort, strings.Join(effortLevels, ", "))
}

func resolveAPIKey() (string, error) {
	if apiKey := os.Getenv(keyEnvironmentVariable); apiKey != "" {
		return apiKey, nil
	}
	store, err := credentials.DefaultStore()
	if err != nil {
		return "", err
	}
	apiKey, found, err := store.Load()
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no API key: set %s or run `think login`", keyEnvironmentVariable)
	}
	return apiKey, nil
}

func (o *options) client() (*anthropic.Client, error) {
	apiKey, err := resolveAPIKey()
	if err != nil {
		return nil, err
	}
	var recorder anthropic.Recorder
	if o.recordDir != "" {
		directoryRecorder, err := record.NewDirectoryRecorder(o.recordDir)
		if err != nil {
			return nil, err
		}
		recorder = directoryRecorder
	}
	return anthropic.NewClient(apiKey, o.baseURL, recorder), nil
}

// prewarm opens the API connection while the command prepares its first
// request. The caller cancels ctx when the command returns, so the request
// never outlives the command.
func prewarm(ctx context.Context, client *anthropic.Client) {
	go func() {
		// The error is ignored on purpose: prewarm only opens a connection,
		// and the first real request reports every real failure.
		_ = client.Prewarm(ctx)
	}()
}

// sessionTools keeps large command outputs in the output directory of the
// session, under the state directory. The caller must close the jobs when
// the session ends, so that no background command outlives the session.
// ask is nil when no user can answer questions.
func sessionTools(store *session.Store, workDir, sessionID string, ext *extensions, ask questionAsker) (*tools.Set, *tools.Jobs, error) {
	outputDirectory, err := store.OutputDirectory(sessionID)
	if err != nil {
		return nil, nil, err
	}
	jobs := tools.NewJobs(outputDirectory)
	standard, err := tools.Standard(workDir, outputDirectory, jobs)
	if err != nil {
		return nil, nil, errors.Join(err, jobs.Close())
	}
	toolSet, err := ext.toolSet(standard, ask)
	if err != nil {
		return nil, nil, errors.Join(err, jobs.Close())
	}
	return toolSet, jobs, nil
}

func loginCommand(ctx context.Context, args []string, stdin *os.File, stderr io.Writer) int {
	flags := flag.NewFlagSet("think login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseURL := flags.String("base-url", openrouter.BaseURL, "API root; /v1/key is appended for the check")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	store, err := credentials.DefaultStore()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}

	fmt.Fprintln(stderr, "Paste your OpenRouter API key, then press Enter. The input is hidden.")
	apiKey, err := readSecret(ctx, stdin, stderr, "Key: ")
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	if apiKey == "" {
		fmt.Fprintln(stderr, "inkling: no key entered")
		return 2
	}

	fmt.Fprintf(stderr, "Checking the key at %s/v1/key ...\n", *baseURL)
	info, err := openrouter.CheckKey(ctx, *baseURL, apiKey)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: key not accepted: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "Key accepted: %s, free tier: %s", info.Label, yesNo(info.IsFreeTier))
	if quota := info.FreeModelDailyRequests; quota != nil {
		fmt.Fprintf(stderr, ", free-model requests today: %d of %d remaining", quota.Remaining, quota.Limit)
	}
	fmt.Fprintln(stderr)

	if err := store.Save(apiKey); err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "Saved to %s (mode 0600). Default model: %s\n", store.Path(), openrouter.DefaultModel)
	return 0
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// readSecret prints the prompt and reads one line. On a terminal it turns
// echo off before the prompt appears, so a fast paste is never shown and the
// key stays out of scrollback.
func readSecret(ctx context.Context, stdin *os.File, stderr io.Writer, prompt string) (string, error) {
	info, err := stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect stdin: %w", err)
	}
	interactive := info.Mode()&os.ModeCharDevice != 0
	if interactive {
		if err := setTerminalEcho(stdin, false); err != nil {
			return "", err
		}
		defer func() {
			_ = setTerminalEcho(stdin, true)
			fmt.Fprintln(stderr)
		}()
	}
	fmt.Fprint(stderr, prompt)

	type lineResult struct {
		line string
		err  error
	}
	results := make(chan lineResult, 1)
	go func() {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			results <- lineResult{err: err}
			return
		}
		results <- lineResult{line: strings.TrimSpace(line)}
	}()
	select {
	case result := <-results:
		return result.line, result.err
	case <-ctx.Done():
		return "", errors.New("interrupted")
	}
}

func setTerminalEcho(terminal *os.File, enabled bool) error {
	argument := "-echo"
	if enabled {
		argument = "echo"
	}
	command := exec.Command("stty", argument)
	command.Stdin = terminal
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("stty %s: %w: %s", argument, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runCommand(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) (exitCode int) {
	flags := flag.NewFlagSet("think run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	o := bindOptions(flags)
	flags.Var(&o.files, "file", "attach a text or image file to this request; repeat for more files")
	jsonEvents := flags.Bool("json", false, "write events as JSON lines on stdout instead of text")
	readOnly := flags.Bool("plan", false, "investigate with read-only tools and reply with a plan; change nothing")
	goalCommand := flags.String("goal", "", "after the task, run this bash command; while it fails, continue (at most 5 rounds); exit 1 if it never passes")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	prompt := strings.Join(flags.Args(), " ")
	interactive := terminal.IsTerminal(stdin)
	if prompt == "" && !interactive {
		piped, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "inkling: read prompt: %v\n", err)
			return 1
		}
		prompt = strings.TrimSpace(string(piped))
	}
	if prompt == "" {
		fmt.Fprintln(stderr, "think: provide a prompt, or use think chat")
		return 2
	}
	if *readOnly {
		prompt = planCommand().Expand(prompt)
	}
	activeProfiles, err := startProfiles(o)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	defer func() {
		if err := activeProfiles.close(); err != nil {
			fmt.Fprintf(stderr, "inkling: finish profiles: %v\n", err)
			exitCode = 1
		}
	}()
	work, code := openWorkspace(ctx, o, stderr)
	if work == nil {
		return code
	}
	defer func() {
		if err := work.close(); err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			exitCode = 1
		}
	}()
	var input *lineInput
	if interactive && !*jsonEvents {
		input = newLineInput(ctx, stdin)
	}
	approval := &approvals{input: input, output: stderr, allowAll: o.approveAll}
	config := work.config
	work.configure(&config, turnPolicy{readOnly: *readOnly}, approval.check)
	ctx, timing := timedContext(ctx, o)
	preparation := latency.Begin(ctx, latency.Preparation, "attachments")
	prepared, request, err := work.prompt(ctx, prompt, o.files)
	preparation.End(err)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", finishTiming(timing, o.timings, err))
		return 1
	}
	printAttachedFiles(stderr, prepared.Files)
	printPrivacyNotice(stderr, o.model)
	if *jsonEvents {
		events := newJSONObserver(stdout)
		outcome, runErr := runWithGoal(ctx, work, config, request, events, *goalCommand, stderr)
		runErr = finishTiming(timing, o.timings, runErr)
		if runErr != nil {
			events.writeError(runErr)
		} else {
			events.writeDone(outcome)
		}
		if err := events.err(); err != nil {
			fmt.Fprintf(stderr, "inkling: write events: %v\n", err)
			return 1
		}
		if runErr != nil {
			return 1
		}
		return 0
	}
	observer := newConsoleObserver(stdout, stderr, nil, false, presentation.Theme{}, o.showThinking, o.verbose)
	started := time.Now()
	observer.Begin()
	outcome, err := runWithGoal(ctx, work, config, request, observer, *goalCommand, stderr)
	observer.Finish()
	err = finishTiming(timing, o.timings, err)
	if o.verbose {
		fmt.Fprintf(stderr, "%d turns, %s, %s\n", outcome.Turns, formatUsage(outcome.Usage), time.Since(started).Round(time.Millisecond))
	}
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	return 0
}

var errGoalNotMet = errors.New("the goal check did not pass")

// runWithGoal runs the task, then, when goalCommand is set, runs the check
// and continues the same conversation while the check fails, for at most
// maxGoalRounds more prompts. Goal progress goes to status.
func runWithGoal(ctx context.Context, work *workspace, config agent.Config, request agent.Prompt, observer agent.Observer, goalCommand string, status io.Writer) (*agent.Outcome, error) {
	outcome, err := agent.Run(ctx, work.client, config, work.toolSet, nil, request, observer)
	if err != nil || goalCommand == "" {
		return outcome, err
	}
	total := *outcome
	current := goal{command: goalCommand}
	for {
		step, prompt, err := current.next(ctx, work.project.WorkDir, status, presentation.Theme{})
		if err != nil {
			return &total, err
		}
		switch step {
		case goalMet:
			return &total, nil
		case goalGiveUp:
			return &total, errGoalNotMet
		case goalContinue:
		}
		next, err := agent.Run(ctx, work.client, config, work.toolSet, total.Messages, agent.Prompt{Text: prompt}, observer)
		total.Messages = next.Messages
		total.Turns += next.Turns
		total.Usage = total.Usage.Add(next.Usage)
		total.TurnLatencies = append(total.TurnLatencies, next.TurnLatencies...)
		total.FinalText = next.FinalText
		total.LastInputTokens = next.LastInputTokens
		if err != nil {
			return &total, err
		}
	}
}

func evalCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (exitCode int) {
	flags := flag.NewFlagSet("think eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	o := bindOptions(flags)
	keepWorkDirs := flags.Bool("keep", false, "keep each task's working directory for inspection")
	outPath := flags.String("out", "", "write one JSON result per line to this file")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "think eval: exactly one tasks.jsonl path is required")
		return 2
	}
	if !o.approveAll {
		fmt.Fprintln(stderr, "think eval: -yes is required; model tools and task checks can run commands without a sandbox")
		return 2
	}
	activeProfiles, err := startProfiles(o)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	defer func() {
		if err := activeProfiles.close(); err != nil {
			fmt.Fprintf(stderr, "inkling: finish profiles: %v\n", err)
			exitCode = 1
		}
	}()
	taskPath := flags.Arg(0)
	config, err := o.agentConfig()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	config.Approve = func(context.Context, anthropic.ToolUseBlock) (bool, error) { return true, nil }
	printPrivacyNotice(stderr, o.model)
	client, err := o.client()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	tasks, err := eval.LoadTasks(taskPath)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			return 1
		}
		defer outFile.Close()
	}

	taskFileDir := filepath.Dir(taskPath)
	results := make([]eval.Result, 0, len(tasks))
	for _, task := range tasks {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "inkling: interrupted")
			return 1
		}
		taskContext, timing := timedContext(ctx, o)
		result := eval.RunTask(taskContext, client, config, evalSetup, task, taskFileDir, *keepWorkDirs)
		var taskError error
		if !result.Pass {
			taskError = errors.New("evaluation failed")
			if ctx.Err() != nil {
				taskError = ctx.Err()
			}
		}
		if timing != nil {
			timing.Finish(taskError)
			if err := timing.AppendFile(o.timings); err != nil {
				fmt.Fprintf(stderr, "inkling: write timings: %v\n", err)
				return 1
			}
		}
		results = append(results, result)
		fmt.Fprintln(stdout, formatResult(result))
		if outFile != nil {
			if err := json.NewEncoder(outFile).Encode(result); err != nil {
				fmt.Fprintf(stderr, "inkling: write %s: %v\n", *outPath, err)
				return 1
			}
		}
	}

	summary := eval.Summarize(results)
	fmt.Fprintf(stdout, "\npass %d/%d  errors %d  turns %d  turn latency p50=%s p95=%s  %s\n",
		summary.Passed, summary.Total, summary.TotalErrors, summary.TotalTurns,
		summary.TurnP50.Round(time.Millisecond), summary.TurnP95.Round(time.Millisecond), formatUsage(summary.Usage))
	if summary.Passed == summary.Total && summary.Total > 0 {
		return 0
	}
	return 1
}

// evalSetup gives each eval task the tools and turn policy of a real session,
// so a feature that changes the model's success shows in the score. It reads
// no user settings, hooks, or memory, so the score does not depend on the
// files of this machine. No user answers ask_user.
func evalSetup(workDir, outputDirectory string, jobs *tools.Jobs, config *agent.Config) (*tools.Set, error) {
	ext := &extensions{workDir: workDir}
	standard, err := tools.Standard(workDir, outputDirectory, jobs)
	if err != nil {
		return nil, err
	}
	toolSet, err := ext.toolSet(standard, nil)
	if err != nil {
		return nil, err
	}
	ext.configure(config, toolSet, turnPolicy{}, config.Approve)
	config.Notices = jobs.Notices
	return toolSet, nil
}

func formatResult(result eval.Result) string {
	status := "FAIL"
	if result.Pass {
		status = "PASS"
	}
	line := fmt.Sprintf("%-24s %s  turns=%-3d %s  %s", result.Task, status, result.Turns, formatUsage(result.Usage), result.Wall.Round(time.Millisecond))
	if result.Error != "" {
		line += "  error: " + result.Error
	}
	if result.WorkDir != "" {
		line += "  workdir: " + result.WorkDir
	}
	return line
}

func formatUsage(usage anthropic.Usage) string {
	return fmt.Sprintf("in=%d cache_read=%d cache_write=%d out=%d",
		usage.InputTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, usage.OutputTokens)
}

func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func printPrivacyNotice(output io.Writer, model string) {
	if strings.HasPrefix(model, "thinkingmachines/") && strings.HasSuffix(model, ":free") {
		fmt.Fprintln(output, "Inkling free logs prompts and outputs for training. Do not send private code, secrets, or personal data.")
	}
}

// removeStaleOutputs reports a failed cleanup and continues. Old output files
// only use disk space; the current session does not depend on them.
func removeStaleOutputs(store *session.Store, stderr io.Writer) {
	if err := store.RemoveStaleOutputs(time.Now()); err != nil {
		fmt.Fprintf(stderr, "inkling: remove old output files: %v\n", err)
	}
}
