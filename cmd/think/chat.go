package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/attachment"
	"github.com/dylantirandaz/inklingharness/internal/latency"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/project"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/terminal"
)

const chatHelp = `Commands:
  /help             show these commands
  /status           show the session, model, directory, and context size
  /usage            show total token use, including summaries and child tasks
  /tools            show the full output of the latest tool batch
  /verbose [on|off]  show or hide per-turn protocol details
  /model [ID]       show or change the model
  /effort [LEVEL]   show or change effort; default restores the model default
  /compact          summarize older messages; keep the last message or tool pair
  /clear            start a new session; keep the old session on disk
  /quit             save and exit
Enter sends. Ctrl-J adds a line. Up/Down recalls input. Ctrl-U clears it.
Pasted text stays in the input area until you press Enter.
Attach files with @path or @"path with spaces". Use @@ for a literal @.
In plain mode, end a line with a backslash to enter more lines.
Ctrl-C stops the active request or tool. At an idle prompt, Ctrl-C exits.
`

const promptPlaceholder = "Ask Inkling to work on a task"

type inputLine struct {
	text string
	err  error
}
type lineInput struct{ lines <-chan inputLine }

// One reader owns stdin for the whole session. A cancelled approval does not
// leave a second reader that can consume the next user prompt.
func newLineInput(ctx context.Context, source io.Reader) *lineInput {
	lines := make(chan inputLine)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(source)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			select {
			case lines <- inputLine{text: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- inputLine{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return &lineInput{lines: lines}
}

func (input *lineInput) next(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case line, open := <-input.lines:
		if !open {
			return "", io.EOF
		}
		return line.text, line.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type approvals struct {
	input    *lineInput
	output   io.Writer
	allowAll bool
	screen   *terminal.Screen
	theme    presentation.Theme
	signals  shipSignals
}

func (approval *approvals) check(ctx context.Context, call anthropic.ToolUseBlock) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if approval.allowAll {
		return true, nil
	}
	if approval.input == nil && approval.screen == nil {
		fmt.Fprintf(approval.output, "denied %s: no interactive terminal; use -yes to approve commands and file changes\n", call.Name)
		return false, nil
	}
	view := describeTool(call.Name, call.Input, approval.theme)
	fmt.Fprintf(approval.output, "\n%s\n", presentation.Approval(view, approval.theme))
	if approval.screen != nil {
		approval.screen.SetStatus("Waiting for approval")
		approval.signals.approval(presentation.Safe(view.Title))
		choice, err := approval.screen.Choose(ctx)
		approval.signals.working()
		if err != nil {
			return false, err
		}
		approval.screen.SetStatus(presentation.Safe(view.Title))
		outcome := func(text string, style func(string) string) {
			fmt.Fprintln(approval.output, presentation.ApprovalBar(approval.theme)+style(text))
		}
		switch choice {
		case terminal.AllowOnce:
			outcome("allowed once", approval.theme.Green)
			return true, nil
		case terminal.AllowSession:
			approval.allowAll = true
			outcome("allowed for this session", approval.theme.Green)
			return true, nil
		case terminal.Deny:
			outcome("denied", approval.theme.Red)
			return false, nil
		default:
			return false, fmt.Errorf("unknown approval choice %v", choice)
		}
	}
	for {
		fmt.Fprint(approval.output, "[y] once, [a] all for this session, [n] deny: ")
		line, err := approval.input.next(ctx)
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		case "a", "all":
			approval.allowAll = true
			return true, nil
		case "n", "no", "":
			return false, nil
		default:
			fmt.Fprintln(approval.output, "Enter y, a, or n.")
		}
	}
}

// The interrupt handler changes only cancellation state. It never prints or
// reads input while the agent or approval prompt owns the terminal.
type turnControl struct {
	mutex  sync.Mutex
	cancel context.CancelFunc
}

func (control *turnControl) begin(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	control.mutex.Lock()
	control.cancel = cancel
	control.mutex.Unlock()
	return ctx, func() { control.mutex.Lock(); control.cancel = nil; control.mutex.Unlock(); cancel() }
}
func (control *turnControl) interrupt(exit context.CancelFunc) {
	control.mutex.Lock()
	defer control.mutex.Unlock()
	if control.cancel != nil {
		control.cancel()
	} else {
		exit()
	}
}

func chatCommand(parent context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) (exitCode int) {
	flags := flag.NewFlagSet("think chat", flag.ContinueOnError)
	flags.SetOutput(stderr)
	o := bindOptions(flags)
	flags.Var(&o.files, "file", "attach a text file to the first request; repeat for more files")
	resume := flags.String("resume", "", "saved session ID, or last for this directory")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "think chat: enter prompts inside the session")
		return 2
	}
	activeProfiles, err := startProfiles(o)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	profileErrors := stderr
	defer func() {
		if err := activeProfiles.close(); err != nil {
			fmt.Fprintf(profileErrors, "inkling: finish profiles: %v\n", err)
			exitCode = 1
		}
	}()
	ctx, exit := context.WithCancel(parent)
	defer exit()
	control := &turnControl{}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		for {
			select {
			case <-interrupts:
				control.interrupt(exit)
			case <-ctx.Done():
				return
			}
		}
	}()
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	projectContext, err := project.Inspect(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	store, err := session.DefaultStore()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	removeStaleOutputs(store, stderr)
	var current session.Session
	switch *resume {
	case "":
		current, err = session.New(projectContext.WorkDir, o.model, o.effort)
	case "last":
		current, err = store.Latest(projectContext.WorkDir)
	default:
		current, err = store.Load(*resume)
	}
	if err != nil {
		fmt.Fprintf(stderr, "inkling: open session: %v\n", err)
		return 1
	}
	if current.WorkDir != projectContext.WorkDir {
		fmt.Fprintf(stderr, "inkling: session belongs to %s; start chat there\n", current.WorkDir)
		return 1
	}
	if *resume != "" {
		modelSet, effortSet := false, false
		flags.Visit(func(flag *flag.Flag) {
			if flag.Name == "model" {
				modelSet = true
			}
			if flag.Name == "effort" {
				effortSet = true
			}
		})
		if !modelSet {
			o.model = current.Model
		}
		if !effortSet {
			o.effort = current.Effort
		}
	}
	if _, err := o.agentConfig(); err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	client, err := o.client()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	prewarm(ctx, client)
	toolSet, err := sessionTools(store, projectContext.WorkDir, current.ID)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	interactive := terminal.IsTerminal(stdin)
	var input *lineInput
	var screen *terminal.Screen
	displayOutput, progressOutput := stdout, stderr
	var theme presentation.Theme
	if outputFile, ok := stdout.(*os.File); ok && interactive && !o.plain && os.Getenv("TERM") != "dumb" && terminal.IsTerminal(outputFile) {
		screen, err = terminal.New(ctx, stdin, outputFile, func() { control.interrupt(exit) },
			presentation.DetectColorDepth(os.Getenv), presentation.AccentFor(o.model))
		if err != nil {
			fmt.Fprintf(stderr, "inkling: terminal: %v\n", err)
			return 1
		}
		theme = screen.Theme()
		originalStderr := stderr
		defer func() {
			if err := screen.Close(); err != nil {
				fmt.Fprintf(originalStderr, "inkling: terminal: %s\n", presentation.Safe(err.Error()))
				exitCode = 1
			}
		}()
		displayOutput, progressOutput = screen, screen
		stderr = safeWriter{target: screen}
	} else {
		input = newLineInput(ctx, stdin)
	}
	signals := shipSignals{screen: screen, place: filepath.Base(projectContext.WorkDir)}
	approval := &approvals{output: progressOutput, allowAll: o.approveAll, screen: screen, theme: theme, signals: signals}
	if interactive && screen == nil {
		approval.input = input
	}
	observer := newConsoleObserver(displayOutput, progressOutput, screen, true, theme, o.showThinking, o.verbose)
	saveOnStart := *resume != "last" || current.Model != o.model || current.Effort != o.effort
	current.Model, current.Effort = o.model, o.effort
	if saveOnStart {
		if err := store.Save(current); err != nil {
			fmt.Fprintf(stderr, "inkling: save session: %v\n", err)
			return 1
		}
	}
	bannerWidth := 0
	if screen != nil {
		bannerWidth = screen.Width()
	}
	fmt.Fprintln(displayOutput, presentation.Banner(o.model, displayEffort(o.effort), projectContext.WorkDir, bannerWidth, theme))
	printPrivacyNotice(stderr, o.model)
	if *resume != "" {
		if err := printLastReply(displayOutput, current.Messages, theme); err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			return 1
		}
	}
	var background *backgroundCompaction
	// endBackground records a background compaction that ended: it saves the
	// session and appends the compaction timing record.
	endBackground := func(saveContext context.Context) error {
		saving := latency.Begin(saveContext, latency.Checkpoint, "save")
		saveError := store.Save(current)
		saving.End(saveError)
		var timingError error
		if background.timing != nil {
			timingError = background.timing.AppendFile(o.timings)
		}
		background = nil
		return errors.Join(saveError, timingError)
	}
	// settleBackground applies the background compaction before an action
	// that reads or changes history. If waitContext ends first, the compaction
	// stops and counts as failed.
	settleBackground := func(waitContext context.Context) error {
		if background == nil {
			return nil
		}
		if screen != nil && background.running() {
			screen.SetStatus(compactingStatus)
		}
		result := background.wait(waitContext)
		var end compactionEnd
		current, end = settleCompaction(current, background.snapshot, result)
		switch end {
		case compactionApplied:
			observer.Status(fmt.Sprintf("context reduced: about %d -> %d tokens", agent.EstimateTokens(background.snapshot), agent.EstimateTokens(current.Messages)))
		case compactionStale:
			// The summary does not describe the current history. The next turn
			// checks the compaction trigger again.
		case compactionFailed:
			fmt.Fprintf(stderr, "inkling: background compaction: %v\n", result.err)
		}
		return endBackground(waitContext)
	}
	// awaitBackground settles the compaction for a command. Ctrl-C during the
	// wait stops the compaction, as it stops any active request, and keeps the chat.
	awaitBackground := func() error {
		if background == nil {
			return nil
		}
		waitContext, finish := control.begin(ctx)
		defer finish()
		return settleBackground(waitContext)
	}
	// stopBackground cancels the compaction and discards its result. The
	// session that was compacted keeps the usage of the canceled request.
	stopBackground := func() error {
		if background == nil {
			return nil
		}
		result := background.stop()
		current.Usage = current.Usage.Add(result.outcome.Usage)
		return endBackground(ctx)
	}
	// A resumed history can already pass the trigger. The terminal interface
	// compacts it after the first key, while the user types; a user who reads
	// the last reply and leaves spends no tokens. Plain mode cannot see keys,
	// so its first turn applies the normal trigger.
	startConfig, err := chatConfig(o, projectContext)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	compactOnTyping := screen != nil && agent.ShouldCompact(startConfig, current.Messages, 0)
	for {
		var line string
		var err error
		if screen != nil {
			screen.SetFooter(contextFooter(o, current.Messages, theme))
			signals.ready()
			var start func() *backgroundCompaction
			if compactOnTyping {
				compactOnTyping = false
				start = func() *backgroundCompaction {
					background = startBackgroundCompaction(ctx, client, startConfig, toolSet, current.Messages, o)
					return background
				}
			}
			var running *backgroundCompaction
			if background != nil && background.running() {
				running = background
			}
			// Apply a result while the user types, so the footer and the saved
			// session show the compacted history at once.
			var settleError error
			line, err = promptWithCompaction(ctx, screen, running, start, func() error {
				if settleError = settleBackground(ctx); settleError != nil {
					return settleError
				}
				screen.SetFooter(contextFooter(o, current.Messages, theme))
				screen.SetBackgroundWork("")
				return nil
			})
			if settleError != nil {
				fmt.Fprintf(stderr, "inkling: %v\n", settleError)
				return 1
			}
		} else {
			fmt.Fprint(stderr, "inkling> ")
			line, err = readPrompt(ctx, input, stderr)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr)
			if err := stopBackground(); err != nil {
				fmt.Fprintf(stderr, "inkling: %v\n", err)
				return 1
			}
			if parent.Err() != nil {
				return 1
			}
			return 0
		}
		if err != nil {
			fmt.Fprintf(stderr, "inkling: read prompt: %v\n", errors.Join(err, stopBackground()))
			return 1
		}
		commandText := strings.TrimSpace(line)
		if commandText == "" {
			continue
		}
		if strings.HasPrefix(commandText, "/") && !strings.Contains(commandText, "\n") {
			command, argument, _ := strings.Cut(commandText, " ")
			argument = strings.TrimSpace(argument)
			saveChanges := false
			commandContext := ctx
			var commandTiming *latency.Recorder
			var commandError error
			switch command {
			case "/quit", "/exit":
				if err := stopBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				return 0
			case "/help":
				fmt.Fprint(stderr, chatHelp)
			case "/status":
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				fmt.Fprintf(stderr, "session=%s\nmodel=%s effort=%s\nworkdir=%s\nmessages=%d context_estimate=%d tokens approve_all=%t\n", current.ID, o.model, displayEffort(o.effort), current.WorkDir, len(current.Messages), agent.EstimateTokens(current.Messages), approval.allowAll)
			case "/usage":
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				fmt.Fprintln(stderr, formatUsage(current.Usage))
			case "/tools":
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				if err := printLatestTools(progressOutput, current.Messages, theme); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
				}
			case "/verbose":
				switch argument {
				case "":
					o.verbose = !o.verbose
				case "on":
					o.verbose = true
				case "off":
					o.verbose = false
				default:
					fmt.Fprintln(stderr, "Use /verbose on or /verbose off.")
					continue
				}
				observer.verbose = o.verbose
				fmt.Fprintf(stderr, "Verbose output: %s\n", yesNo(o.verbose))
			case "/model":
				if argument == "" {
					fmt.Fprintln(stderr, o.model)
					continue
				}
				if strings.ContainsAny(argument, " \t\n") {
					fmt.Fprintln(stderr, "Use one model ID.")
					continue
				}
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				o.model, current.Model = argument, argument
				printPrivacyNotice(stderr, o.model)
				saveChanges = true
			case "/effort":
				if argument == "" {
					fmt.Fprintln(stderr, displayEffort(o.effort))
					continue
				}
				candidate := *o
				if argument == "default" {
					candidate.effort = ""
				} else {
					candidate.effort = argument
				}
				if _, err := candidate.agentConfig(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					continue
				}
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				o.effort, current.Effort = candidate.effort, candidate.effort
				saveChanges = true
			case "/clear":
				if err := stopBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				current, err = session.New(projectContext.WorkDir, o.model, o.effort)
				if err == nil {
					toolSet, err = sessionTools(store, projectContext.WorkDir, current.ID)
				}
				if err != nil {
					fmt.Fprintf(stderr, "inkling: new session: %v\n", err)
					return 1
				}
				approval.allowAll = o.approveAll
				fmt.Fprintf(stderr, "New session %s; the old session is still saved.\n", current.ID)
				saveChanges = true
			case "/compact":
				if background != nil {
					// One compaction of this history already runs, so wait for it
					// instead of sending a second request.
					if err := awaitBackground(); err != nil {
						fmt.Fprintf(stderr, "inkling: %v\n", err)
						return 1
					}
					break
				}
				config, configErr := chatConfig(o, projectContext)
				if configErr != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", configErr)
					continue
				}
				turnCtx, finish := control.begin(ctx)
				turnCtx, commandTiming = timedContext(turnCtx, o)
				commandContext = turnCtx
				observer.Begin()
				compacted, compactErr := agent.Compact(turnCtx, client, config, toolSet, current.Messages, observer)
				finish()
				observer.Finish()
				current.Usage = current.Usage.Add(compacted.Usage)
				if compactErr != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", compactErr)
				} else {
					current.Messages = compacted.Messages
				}
				commandError = compactErr
				saveChanges = true
			default:
				fmt.Fprintln(stderr, "Unknown command. Use /help.")
				continue
			}
			var saveError error
			if saveChanges {
				saving := latency.Begin(commandContext, latency.Checkpoint, "save")
				saveError = store.Save(current)
				saving.End(saveError)
			}
			var timingError error
			if commandTiming != nil {
				commandTiming.Finish(errors.Join(commandError, saveError))
				timingError = commandTiming.AppendFile(o.timings)
			}
			if err := errors.Join(saveError, timingError); err != nil {
				fmt.Fprintf(stderr, "inkling: %v\n", err)
				return 1
			}
			continue
		}
		config, err := chatConfig(o, projectContext)
		if err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			continue
		}
		config.Approve = approval.check
		printUser(displayOutput, line, theme)
		signals.working()
		turnCtx, finish := control.begin(ctx)
		turnCtx, timing := timedContext(turnCtx, o)
		// The wait belongs to this turn, so Ctrl-C stops the compaction and the
		// turn together.
		if err := settleBackground(turnCtx); err != nil {
			finish()
			fmt.Fprintf(stderr, "inkling: %v\n", finishTiming(timing, o.timings, err))
			return 1
		}
		priorUsage := current.Usage
		config.Checkpoint = func(outcome agent.Outcome) error {
			current.Messages = outcome.Messages
			current.Usage = priorUsage.Add(outcome.Usage)
			current.UpdatedAt = time.Now().UTC()
			return store.Save(current)
		}
		observer.Begin()
		started := time.Now()
		preparation := latency.Begin(turnCtx, latency.Preparation, "attachments")
		prepared, prepareError := attachment.Prepare(turnCtx, projectContext.WorkDir, line, o.files)
		preparation.End(prepareError)
		if prepareError != nil {
			finish()
			observer.Finish()
			var timingError error
			if timing != nil {
				timing.Finish(prepareError)
				timingError = timing.AppendFile(o.timings)
			}
			fmt.Fprintf(stderr, "inkling: %v\n", errors.Join(prepareError, timingError))
			if timingError != nil {
				return 1
			}
			continue
		}
		o.files = nil
		printAttachedFiles(progressOutput, prepared.Files)
		outcome, runErr := agent.Run(turnCtx, client, config, toolSet, current.Messages, prepared.Prompt, observer)
		finish()
		observer.Finish()
		var saveError error
		if runErr != nil {
			saving := latency.Begin(turnCtx, latency.Checkpoint, "save")
			saveError = config.Checkpoint(*outcome)
			saving.End(saveError)
		}
		var timingError error
		if timing != nil {
			timing.Finish(errors.Join(runErr, saveError))
			timingError = timing.AppendFile(o.timings)
		}
		if err := errors.Join(saveError, timingError); err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			return 1
		}
		if o.verbose {
			fmt.Fprintf(stderr, "%d turns, %s, %s\n", outcome.Turns, formatUsage(outcome.Usage), time.Since(started).Round(time.Millisecond))
		}
		if runErr != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", runErr)
		}
		signals.finished(time.Since(started), outcome.FinalText, runErr)
		// Compact while the user reads and types, so the next turn does not
		// wait for it. Nothing changes history until the chat settles it.
		if ctx.Err() == nil && agent.ShouldCompact(config, current.Messages, outcome.LastInputTokens) {
			background = startBackgroundCompaction(ctx, client, config, toolSet, current.Messages, o)
		}
	}
}

func chatConfig(options *options, projectContext project.Context) (agent.Config, error) {
	config, err := options.agentConfig()
	if err != nil {
		return agent.Config{}, err
	}
	config.System += "\n\n" + projectContext.SystemPrompt()
	return config, nil
}

func readPrompt(ctx context.Context, input *lineInput, output io.Writer) (string, error) {
	var prompt strings.Builder
	for {
		line, err := input.next(ctx)
		if err != nil {
			return "", err
		}
		continued := strings.HasSuffix(line, "\\")
		if continued {
			line = strings.TrimSuffix(line, "\\")
		}
		prompt.WriteString(line)
		if !continued {
			return prompt.String(), nil
		}
		prompt.WriteByte('\n')
		fmt.Fprint(output, "... ")
	}
}

func displayEffort(effort string) string {
	if effort == "" {
		return "default"
	}
	return effort
}

func printLastReply(output io.Writer, messages []anthropic.EncodedMessage, theme presentation.Theme) error {
	if len(messages) == 0 {
		return nil
	}
	encoded := messages[len(messages)-1]
	if encoded.Role() != anthropic.RoleAssistant {
		return nil
	}
	last, err := encoded.Decode()
	if err != nil {
		return fmt.Errorf("show previous reply: %w", err)
	}
	markdown := presentation.NewMarkdown(theme)
	fmt.Fprintln(output, "\n"+presentation.Section("previous reply", theme))
	for _, block := range last.Content {
		if text, ok := block.(anthropic.TextBlock); ok {
			for _, line := range strings.Split(text.Text, "\n") {
				fmt.Fprintln(output, "  "+markdown.Line(line))
			}
		}
	}
	return nil
}

func sessionsCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "export" {
		return sessionsExportCommand(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("think sessions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	all := flags.Bool("all", false, "list sessions from all directories")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "think sessions: no positional arguments are accepted; use think sessions export ID")
		return 2
	}
	root := ""
	if !*all {
		var err error
		root, err = os.Getwd()
		if err == nil {
			root, err = filepath.EvalSymlinks(root)
		}
		if err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			return 1
		}
	}
	store, err := session.DefaultStore()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	sessions, err := store.List(root)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	for _, saved := range sessions {
		fmt.Fprintf(stdout, "%s  %s  %d messages  %s  %s\n", saved.ID, saved.UpdatedAt.Format(time.RFC3339), saved.MessageCount, saved.Model, saved.WorkDir)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No saved sessions.")
	}
	return 0
}

// sessionsExportCommand writes one session in the format of releases before
// the session log, so an older build can open it.
func sessionsExportCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("think sessions export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "think sessions export: exactly one session ID is required")
		return 2
	}
	store, err := session.DefaultStore()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	path, err := store.Export(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}
