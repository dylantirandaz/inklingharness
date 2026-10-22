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
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/attachment"
	"github.com/dylantirandaz/inklingharness/internal/latency"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/project"
	"github.com/dylantirandaz/inklingharness/internal/rewind"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/terminal"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// chatCommandTable lists the commands of chat itself for /help and the slash
// menu. Usage names the arguments; the first word is the command name.
var chatCommandTable = []terminal.Command{
	{Name: "help", Usage: "help", Description: "show these commands"},
	{Name: "status", Usage: "status", Description: "show the session, model, directory, and context size"},
	{Name: "usage", Usage: "usage", Description: "show total token use, including summaries and child tasks"},
	{Name: "tools", Usage: "tools [N]", Description: "open full output of this turn's tools; N selects a tab"},
	{Name: "diff", Usage: "diff", Description: "view current Git changes against HEAD, one file per tab"},
	{Name: "verbose", Usage: "verbose [on|off]", Description: "show or hide per-turn protocol details"},
	{Name: "model", Usage: "model [ID]", Description: "show or change the model"},
	{Name: "effort", Usage: "effort [LEVEL]", Description: "show or change effort; default restores the model default"},
	{Name: "compact", Usage: "compact", Description: "summarize older messages; keep the last message or tool pair"},
	{Name: "clear", Usage: "clear", Description: "start a new session; keep the old session on disk"},
	{Name: "fork", Usage: "fork", Description: "continue in a copy of this session; keep the original"},
	{Name: "title", Usage: "title [TEXT]", Description: "show or set the session title"},
	{Name: "export", Usage: "export [PATH]", Description: "write the session as an interactive HTML page"},
	{Name: "plan", Usage: "plan TASK", Description: "investigate with read-only tools and reply with a plan"},
	{Name: "goal", Usage: "goal [CMD|off]", Description: "keep working until CMD passes (at most 5 rounds); off clears it"},
	{Name: "context", Usage: "context", Description: "show what fills the context window"},
	{Name: "tinker", Usage: "tinker [MODEL]", Description: "fine-tune MODEL on your sessions with Tinker and switch to it; no MODEL shows progress"},
	{Name: "undo", Usage: "undo", Description: "restore the files and conversation from before the last prompt"},
	{Name: "rewind", Usage: "rewind [N]", Description: "list prompts, or restore the state from before prompt N"},
	{Name: "quit", Usage: "quit", Description: "save and exit"},
}

// chatKeys is the key table of /help: each key and what it does, in the order
// that a user meets them.
var chatKeys = [...]struct{ key, action string }{
	{"Enter", "send; while Inkling works, steer the running turn"},
	{"Alt-Enter", "while Inkling works, queue a message for the next turn"},
	{"Alt-Up", "move the queued messages back into the input"},
	{"Ctrl-J", "add a line"},
	{"Ctrl-U", "clear the input"},
	{"Up/Down", "move through input rows, then recall history at the edges"},
	{"Ctrl-P/Ctrl-N", "recall history directly and restore the draft"},
	{"Alt-Left/Right", "move by word"},
	{"Ctrl-W", "delete the previous word"},
	{"@path", "attach a file; @\"path with spaces\" for spaces, @@ for an @"},
	{"/", "open the command menu: Up/Down selects, Tab completes, Esc closes"},
	{"Ctrl-C", "stop the request or tool; at an idle prompt, exit"},
	{"In /tools, /diff", "arrows or j/k scroll, Left/Right or a digit picks a tab, Esc returns"},
	{"\\ at line end", "in plain mode, continue the prompt on the next line"},
}

// chatHelp lists the chat commands, then the extension help, then the keys,
// as two aligned tables. With a theme the commands and keys take the model
// color and their descriptions the gray of the command menu, so help reads
// like the menu it explains.
func chatHelp(extensionHelp string, theme presentation.Theme) string {
	var out strings.Builder
	out.WriteString("  " + theme.Bold(theme.Ink("Commands")) + "\n")
	for _, command := range chatCommandTable {
		usage := fmt.Sprintf("/%-17s", command.Usage)
		fmt.Fprintf(&out, "  %s %s\n", theme.Accent(usage), theme.Graphite(command.Description))
	}
	out.WriteString(extensionHelp)
	out.WriteString("\n  " + theme.Bold(theme.Ink("Keys")) + "\n")
	for _, entry := range chatKeys {
		fmt.Fprintf(&out, "  %s %s\n", theme.Ink(fmt.Sprintf("%-18s", entry.key)), theme.Graphite(entry.action))
	}
	// The row of air that ends the block, as after a card.
	return out.String() + "\n"
}

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
	// project is the folder that the label of a request names files from.
	project string
	// waited is the time spent on the last answered approval.
	waited time.Duration
}

// takeWait returns the time the last approval waited for the user, once.
func (approval *approvals) takeWait() time.Duration {
	waited := approval.waited
	approval.waited = 0
	return waited
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
	request := presentation.NewApprovalRequest(call.Name, view)
	// The label names files from the project; the work keeps its paths,
	// because the user approves exactly what will run.
	request.Title = presentation.TrimProject(approval.project, request.Title)
	request.Caption = presentation.TrimProject(approval.project, request.Caption)
	// A request that fits hangs above the choices and leaves only its
	// outcome; a longer one, such as a large diff, goes to the scrollback.
	shown := approval.screen != nil && approval.screen.RequestFits(request)
	if !shown {
		text := presentation.Approval(view, approval.theme)
		if approval.screen != nil && approval.theme.Soft() {
			text = strings.Join(approval.theme.ApprovalRows(request, approval.screen.Width()-1, false), "\n")
		}
		fmt.Fprintf(approval.output, "\n%s\n", text)
	}
	if approval.screen != nil {
		approval.screen.SetStatus("Waiting for approval")
		approval.signals.approval(presentation.Safe(view.Title))
		started := time.Now()
		choose := approval.screen.Choose
		if shown {
			choose = func(ctx context.Context) (terminal.Choice, error) { return approval.screen.ChooseRequest(ctx, request) }
		}
		choice, err := choose(ctx)
		approval.waited = time.Since(started)
		approval.signals.working()
		if err != nil {
			return false, err
		}
		approval.screen.SetStatus(presentation.Safe(view.Title))
		outcome := func(text string, style func(string) string) {
			if shown || approval.theme.Soft() {
				fmt.Fprintln(approval.output, "  "+approval.theme.Graphite("· ")+style(text))
				return
			}
			fmt.Fprintln(approval.output, presentation.ApprovalBar(approval.theme)+style(text))
		}
		switch choice {
		case terminal.AllowOnce:
			if !shown {
				outcome("allowed once", approval.theme.Green)
			}
			return true, nil
		case terminal.AllowSession:
			approval.allowAll = true
			outcome("allowed for this session", approval.theme.Green)
			return true, nil
		case terminal.Deny:
			if !shown {
				outcome("denied", approval.theme.Red)
			}
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
	flags.Var(&o.files, "file", "attach a text or image file to the first request; repeat for more files")
	resume := flags.String("resume", "", "saved session ID, last for the newest in this directory, or pick to choose from a list")
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
	case "pick":
		var picked string
		picked, err = pickSession(ctx, store, projectContext.WorkDir, stdin, stderr)
		if err == nil {
			current, err = store.Load(picked)
		}
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
	ext, err := loadExtensions(projectContext.WorkDir, o.noWeb)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 1
	}
	// The screen does not exist yet; ask_user reaches it through answerer,
	// which is set before the first prompt.
	var answerer questionAsker
	ask := func(ctx context.Context, asked question) (string, error) { return answerer(ctx, asked) }
	toolSet, jobs, err := sessionTools(store, projectContext.WorkDir, current.ID, ext, ask)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", errors.Join(err, ext.close()))
		return 1
	}
	// Later, stderr writes to the terminal screen, and the screen closes
	// before this deferred call. Thus the call keeps the stderr of this point.
	jobErrors := stderr
	defer func() {
		if err := errors.Join(jobs.Close(), ext.close()); err != nil {
			fmt.Fprintf(jobErrors, "inkling: stop background jobs and MCP servers: %v\n", err)
			exitCode = 1
		}
	}()
	interactive := terminal.IsTerminal(stdin)
	var input *lineInput
	var screen *terminal.Screen
	displayOutput, progressOutput := stdout, stderr
	var theme presentation.Theme
	if outputFile, ok := stdout.(*os.File); ok && interactive && !o.plain && os.Getenv("TERM") != "dumb" && terminal.IsTerminal(outputFile) {
		motion, err := motionSetting(os.Getenv(motionVariable))
		if err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			return 2
		}
		screen, err = terminal.New(ctx, stdin, outputFile, func() { control.interrupt(exit) }, terminal.Style{
			Depth:  presentation.DetectColorDepth(os.Getenv),
			Accent: presentation.AccentFor(o.model),
			Motion: motion,
		})
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
	if screen != nil {
		screen.SetCommands(append(slices.Clone(chatCommandTable), ext.userCommands()...))
		screen.SetPlace(homeRelative(projectContext.WorkDir), gitBranch(projectContext.WorkDir))
		answerer = screenAsker(screen, progressOutput, theme)
	} else {
		answerer = lineAsker(input, progressOutput)
	}
	signals := shipSignals{screen: screen, place: filepath.Base(projectContext.WorkDir)}
	approval := &approvals{output: progressOutput, allowAll: o.approveAll, screen: screen, theme: theme, signals: signals, project: projectContext.WorkDir}
	if interactive && screen == nil {
		approval.input = input
	}
	observer := newConsoleObserver(displayOutput, progressOutput, screen, true, theme, o.showThinking, o.verbose)
	observer.approval = approval
	observer.model = o.model
	observer.project = projectContext.WorkDir
	saveOnStart := *resume != "last" || current.Model != o.model || current.Effort != o.effort
	current.Model, current.Effort = o.model, o.effort
	if saveOnStart {
		if err := store.Save(current); err != nil {
			fmt.Fprintf(stderr, "inkling: save session: %v\n", err)
			return 1
		}
	}
	banner := presentation.BannerInfo{
		Directory: projectContext.WorkDir,
		Model:     presentation.ModelName(o.model),
		Effort:    displayEffort(o.effort),
	}
	if screen != nil {
		banner.Width, banner.Height = screen.Width(), screen.Height()
	}
	animation := presentation.BannerAnimation(banner, theme)
	if screen != nil && animation != nil {
		screen.Intro(animation)
	} else {
		fmt.Fprintln(displayOutput, presentation.Banner(banner, theme))
	}
	if screen == nil {
		fmt.Fprintf(displayOutput, "%s · %s effort\n", presentation.Safe(o.model), displayEffort(o.effort))
	}
	printPrivacyNotice(stderr, o.model)
	if *resume != "" {
		resumedWidth := 0
		if screen != nil {
			resumedWidth = screen.Width()
		}
		if err := printLastReply(displayOutput, current.Messages, theme, resumedWidth); err != nil {
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
	startConfig, err := chatConfig(o, projectContext, ext, store)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	compactOnTyping := screen != nil && agent.ShouldCompact(startConfig, current.Messages, 0)
	// checkpoints hold the state before each prompt of this process, oldest
	// first, for /undo and /rewind. They are not saved with the session.
	var checkpoints []checkpoint
	// activeGoal is the check of /goal; queued is a prompt that a failed check
	// sends next, without user input. The screen queue of the user comes
	// around it: steering that came after the last boundary of a turn goes
	// first, because it changes the current work; a message for later waits
	// until the goal stops.
	var activeGoal goal
	var queued string
	// tinkerWatch switches the chat to each checkpoint that /tinker trains.
	tinkerWatch := &tinkerWatcher{}
	for {
		var line string
		var err error
		var steering []string
		followUp, haveFollowUp := "", false
		if screen != nil {
			steering = screen.TakeSteering()
			if len(steering) == 0 && queued == "" {
				followUp, haveFollowUp = screen.TakeFollowUp()
			}
		}
		goalTurn := len(steering) == 0 && queued != ""
		switch {
		case len(steering) > 0:
			line = strings.Join(steering, "\n\n")
		case goalTurn:
			line, queued = queued, ""
		case haveFollowUp:
			line = followUp
		case screen != nil:
			screen.SetContext(contextUse(o, current.Messages, &current.Usage))
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
				screen.SetContext(contextUse(o, current.Messages, &current.Usage))
				screen.SetBackgroundWork("")
				return nil
			})
			if settleError != nil {
				fmt.Fprintf(stderr, "inkling: %v\n", settleError)
				return 1
			}
		default:
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
		if model, switched := tinkerWatch.poll(stderr); switched {
			o.model, current.Model, observer.model = model, model, model
		}
		commandText := strings.TrimSpace(line)
		if commandText == "" {
			continue
		}
		custom, customArgument, isCustom := ext.customCommand(commandText)
		if !isCustom && strings.HasPrefix(commandText, "/") && !strings.Contains(commandText, "\n") {
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
				fmt.Fprint(progressOutput, chatHelp(ext.commandHelp(), theme))
			case "/status":
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				switch {
				case screen == nil:
					fmt.Fprintf(stderr, "session=%s\nmodel=%s effort=%s\nworkdir=%s\nmessages=%d context_estimate=%d tokens approve_all=%t\n", current.ID, o.model, displayEffort(o.effort), current.WorkDir, len(current.Messages), agent.EstimateTokens(current.Messages), approval.allowAll)
				case cardFits(theme, screen.Width()):
					printCard(progressOutput, theme, screen.Width(), statusCard(theme, screen.Width(), statusFacts{
						model: o.model, effort: displayEffort(o.effort), directory: current.WorkDir, sessionID: current.ID,
						autoApprove: approval.allowAll, messages: len(current.Messages),
						estimate: agent.EstimateTokens(current.Messages), compactTokens: o.compactTokens,
					}))
				default:
					fmt.Fprintf(progressOutput, "\n%s\n  %s  %s\n  %s  %s\n  %s  %s\n  %s  %s\n  %s  %d messages · about %d tokens\n  %s  %s\n\n",
						presentation.Section("Session", theme),
						theme.Graphite("ID          "), presentation.Safe(current.ID),
						theme.Graphite("Model       "), presentation.Safe(o.model),
						theme.Graphite("Effort      "), displayEffort(o.effort),
						theme.Graphite("Directory   "), presentation.Safe(current.WorkDir),
						theme.Graphite("Context     "), len(current.Messages), agent.EstimateTokens(current.Messages),
						theme.Graphite("Auto-approve"), yesNo(approval.allowAll))
				}
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
				documents, err := latestToolDocuments(current.Messages, theme)
				if err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					continue
				}
				if len(documents) == 0 {
					fmt.Fprintln(stderr, "No tool results are stored for the last prompt.")
					continue
				}
				selected := 0
				if argument != "" {
					number, err := strconv.Atoi(argument)
					if err != nil || number < 1 || number > len(documents) {
						fmt.Fprintf(stderr, "Use /tools N with N between 1 and %d.\n", len(documents))
						continue
					}
					selected = number - 1
				}
				if screen != nil {
					if err := screen.View(ctx, "Turn tools", documents, selected); err != nil {
						fmt.Fprintf(stderr, "inkling: %v\n", err)
					}
				} else {
					if argument != "" {
						documents = documents[selected : selected+1]
					}
					for _, document := range documents {
						fmt.Fprintf(progressOutput, "\n%s\n%s\n", presentation.Safe(document.Title), presentation.Safe(document.Text))
					}
				}
			case "/diff":
				if argument != "" {
					fmt.Fprintln(stderr, "Use /diff without arguments.")
					continue
				}
				diffContext, finish := control.begin(ctx)
				if screen != nil {
					screen.SetStatus("Reading changes")
				}
				diff, err := rewind.Diff(diffContext, projectContext.WorkDir)
				finish()
				if screen != nil {
					screen.SetStatus("")
				}
				if err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					continue
				}
				if diff == "" {
					fmt.Fprintln(stderr, "No changes against HEAD.")
					continue
				}
				if screen != nil {
					if err := screen.View(ctx, "Changes against HEAD", diffDocuments(diff), 0); err != nil {
						fmt.Fprintf(stderr, "inkling: %v\n", err)
					}
				} else {
					fmt.Fprint(progressOutput, presentation.Safe(diff))
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
				o.model, current.Model, observer.model = argument, argument, argument
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
					// The old session ends here, so its background jobs end too.
					err = jobs.Close()
				}
				var nextJobs *tools.Jobs
				if err == nil {
					toolSet, nextJobs, err = sessionTools(store, projectContext.WorkDir, current.ID, ext, ask)
				}
				if err != nil {
					fmt.Fprintf(stderr, "inkling: new session: %v\n", err)
					return 1
				}
				jobs = nextJobs
				checkpoints = nil
				activeGoal = goal{}
				approval.allowAll = o.approveAll
				fmt.Fprintf(stderr, "New session %s; the old session is still saved.\n", current.ID)
				saveChanges = true
			case "/undo", "/rewind":
				if err := stopBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				target, ok := rewindTarget(command, argument, checkpoints, stderr)
				if !ok {
					continue
				}
				restored := checkpoints[target]
				changed, restoreErr := restoreFiles(ctx, projectContext.WorkDir, restored)
				if restoreErr != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", restoreErr)
					continue
				}
				current.Messages = restored.messages
				current.UpdatedAt = time.Now().UTC()
				checkpoints = checkpoints[:target]
				printRewind(stderr, restored, changed)
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
				config, configErr := chatConfig(o, projectContext, ext, store)
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
			case "/goal":
				switch argument {
				case "":
					if activeGoal.active() {
						fmt.Fprintf(stderr, "Goal: %s (round %d of %d)\n", activeGoal.command, activeGoal.rounds, maxGoalRounds)
					} else {
						fmt.Fprintln(stderr, "No goal. Use /goal COMMAND, for example /goal go test ./...")
					}
					continue
				case "off":
					activeGoal = goal{}
					fmt.Fprintln(stderr, "Goal cleared.")
					continue
				}
				activeGoal = goal{command: argument}
				step, prompt, goalErr := activeGoal.next(ctx, projectContext.WorkDir, stderr, theme)
				if goalErr != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", goalErr)
					activeGoal = goal{}
					continue
				}
				if step == goalContinue {
					queued = prompt
				}
				continue
			case "/tinker":
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				if err := store.Save(current); err != nil {
					fmt.Fprintf(stderr, "inkling: save session: %v\n", err)
					return 1
				}
				config, configErr := chatConfig(o, projectContext, ext, store)
				if configErr == nil {
					configErr = tinkerWatch.command(stderr, argument, store, projectContext.WorkDir, config.System, agent.RequestTools(config, toolSet))
				}
				if configErr != nil {
					fmt.Fprintf(stderr, "inkling: tinker: %v\n", configErr)
				}
				continue
			case "/context":
				config, configErr := chatConfig(o, projectContext, ext, store)
				if configErr == nil {
					if screen != nil && cardFits(theme, screen.Width()) {
						configErr = writeContextCard(progressOutput, theme, screen.Width(), config, toolSet, current.Messages, o.compactTokens)
					} else {
						configErr = writeContext(stderr, config, toolSet, current.Messages, o.compactTokens)
					}
				}
				if configErr != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", configErr)
				}
				continue
			case "/export":
				if err := awaitBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				path, exportErr := exportSession(current, argument, time.Now())
				if exportErr != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", exportErr)
					continue
				}
				fmt.Fprintf(stderr, "Exported %s\n", theme.Underline(presentation.Safe(path)))
				continue
			case "/title":
				if argument == "" {
					fmt.Fprintln(stderr, describeTitle(current.Title))
					continue
				}
				if err := session.ValidTitle(argument); err != nil {
					fmt.Fprintf(stderr, "inkling: title: %v\n", err)
					continue
				}
				current.Title = argument
				saveChanges = true
			case "/fork":
				if err := stopBackground(); err != nil {
					fmt.Fprintf(stderr, "inkling: %v\n", err)
					return 1
				}
				if err := store.Save(current); err != nil {
					fmt.Fprintf(stderr, "inkling: save session: %v\n", err)
					return 1
				}
				forked, forkErr := store.Fork(current.ID)
				if forkErr != nil {
					fmt.Fprintf(stderr, "inkling: fork: %v\n", forkErr)
					continue
				}
				// The fork is a new session with its own output folder, so the
				// jobs of the original end, as with /clear.
				err = jobs.Close()
				var nextJobs *tools.Jobs
				if err == nil {
					toolSet, nextJobs, err = sessionTools(store, projectContext.WorkDir, forked.ID, ext, ask)
				}
				if err != nil {
					fmt.Fprintf(stderr, "inkling: fork: %v\n", err)
					return 1
				}
				jobs = nextJobs
				fmt.Fprintf(stderr, "Forked into %s (%s); the original %s stays saved.\n", forked.ID, presentation.Safe(forked.Title), current.ID)
				current = forked
				checkpoints = nil
				activeGoal = goal{}
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
		config, err := chatConfig(o, projectContext, ext, store)
		if err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			continue
		}
		shown := line
		if goalTurn {
			shown = fmt.Sprintf("goal round %d of %d: %s", activeGoal.rounds, maxGoalRounds, activeGoal.command)
		}
		var policy turnPolicy
		if isCustom {
			line = custom.Expand(customArgument)
			policy.readOnly = custom.ReadOnly
		}
		// The snapshot runs while the first request streams; the first tool
		// that can change files waits for it, so it holds the state before
		// any change.
		pending := startSnapshot(ctx, projectContext.WorkDir)
		policy.beforeChange = pending.settled
		ext.configure(&config, toolSet, policy, approval.check)
		config.Notices = jobs.Notices
		if screen != nil {
			config.Steering = func() []string {
				steering := screen.TakeSteering()
				if len(steering) > 0 {
					observer.steered(steering)
				}
				return steering
			}
		}
		width := 0
		if screen != nil {
			width = screen.Width()
		}
		printUser(displayOutput, shown, width, theme)
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
		before := current.Messages
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
		prepared, prepareError := attachment.Prepare(turnCtx, projectContext.WorkDir, line, o.files, store)
		if prepareError == nil {
			prepared.Prompt, prepareError = ext.prompt(turnCtx, prepared.Prompt)
		}
		preparation.End(prepareError)
		if prepareError != nil {
			finish()
			observer.Finish()
			if screen != nil {
				screen.RestoreQueue()
			}
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
		outcome, runErr := agent.Run(turnCtx, client, config, toolSet, current.Messages, agent.Prompt{Text: prepared.Prompt}, observer)
		finish()
		observer.Finish()
		if screen != nil && runErr != nil {
			// After a stop or a failure the user decides again what to send,
			// so the queued messages go back into the editor.
			screen.RestoreQueue()
		}
		snapshot, snapshotErr := pending.wait(context.Background())
		if snapshotErr != nil {
			fmt.Fprintf(stderr, "inkling: undo is off for this prompt: %v\n", snapshotErr)
		}
		checkpoints = append(checkpoints, checkpoint{prompt: shown, messages: before, snapshot: snapshot})
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
			if screen != nil && theme.Soft() {
				title := "Turn failed"
				if errors.Is(runErr, context.Canceled) {
					title = "Turn stopped"
				}
				fmt.Fprintln(progressOutput, strings.Join(theme.FailureRows(title, failureCause(runErr), screen.Width()-1), "\n")+"\n")
			} else {
				fmt.Fprintf(stderr, "inkling: %v\n", runErr)
			}
		}
		if screen != nil && runErr == nil && outcome.Turns > 0 {
			inputTokens := outcome.Usage.InputTokens + outcome.Usage.CacheReadInputTokens + outcome.Usage.CacheCreationInputTokens
			if theme.Soft() {
				summary := presentation.TurnSummaryText(time.Since(started), outcome.Turns, inputTokens, outcome.Usage.OutputTokens)
				// The score of drops and the stats close the turn, then a
				// row of air.
				fmt.Fprintln(progressOutput, "\n"+theme.TurnRule(summary, observer.steps)+"\n")
			} else {
				fmt.Fprintln(progressOutput, "\n  "+presentation.TurnSummary(time.Since(started), outcome.Turns, inputTokens, outcome.Usage.OutputTokens, theme))
			}
		}
		if screen != nil {
			// A turn can switch branches, so the footer asks again.
			screen.SetPlace(homeRelative(projectContext.WorkDir), gitBranch(projectContext.WorkDir))
		}
		signals.finished(time.Since(started), outcome.FinalText, runErr)
		if activeGoal.active() {
			switch {
			case runErr != nil || ctx.Err() != nil:
				// A failed or cancelled turn ends the goal; the user decides.
				fmt.Fprintln(stderr, "Goal stopped. Use /goal COMMAND to start it again.")
				activeGoal = goal{}
			default:
				step, prompt, goalErr := activeGoal.next(ctx, projectContext.WorkDir, progressOutput, theme)
				switch {
				case goalErr != nil:
					fmt.Fprintf(stderr, "inkling: %v\n", goalErr)
					activeGoal = goal{}
				case step == goalContinue:
					queued = prompt
				}
			}
		}
		// Compact while the user reads and types, so the next turn does not
		// wait for it. Nothing changes history until the chat settles it.
		if ctx.Err() == nil && agent.ShouldCompact(config, current.Messages, outcome.LastInputTokens) {
			background = startBackgroundCompaction(ctx, client, config, toolSet, current.Messages, o)
		}
	}
}

// steered shows each steering message that went to the model as a sent
// prompt, at the point where it took effect. The reply text before it
// commits first, and the reply after it starts a new block.
func (c *consoleObserver) steered(messages []string) {
	c.flushThinking()
	c.flushText()
	width := 0
	if c.screen != nil {
		width = c.screen.Width()
	}
	for _, message := range messages {
		printUser(c.stdout, message, width, c.theme)
	}
	c.bodyStarted, c.leadShown, c.afterTools = false, false, false
}

func chatConfig(options *options, projectContext project.Context, ext *extensions, store *session.Store) (agent.Config, error) {
	config, err := options.agentConfig()
	if err != nil {
		return agent.Config{}, err
	}
	config.ContextStore = store
	config.System += "\n\n" + projectContext.SystemPrompt()
	if text := ext.systemPrompt(); text != "" {
		config.System += "\n\n" + text
	}
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

// printLastReply shows the end of the saved conversation on resume. A width
// above zero is the width of the screen: prose wraps to it and code sits on a
// card, as in a live reply.
func printLastReply(output io.Writer, messages []anthropic.EncodedMessage, theme presentation.Theme, screenWidth int) error {
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
		text, ok := block.(anthropic.TextBlock)
		if !ok {
			continue
		}
		for _, line := range strings.Split(text.Text, "\n") {
			inCode := markdown.InCode()
			rendered := markdown.Line(line)
			if inCode && !markdown.InCode() {
				// The fence that closes a code block draws nothing.
				continue
			}
			if screenWidth <= 0 {
				fmt.Fprintln(output, "  "+rendered)
				continue
			}
			for _, row := range terminal.WrapWords(rendered, max(screenWidth-3, 1)) {
				fmt.Fprintln(output, "  "+row)
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
		title := ""
		if saved.Title != "" {
			title = "  " + strconv.Quote(saved.Title)
		}
		fmt.Fprintf(stdout, "%s  %s  %d messages  %s  %s%s\n", saved.ID, saved.UpdatedAt.Format(time.RFC3339), saved.MessageCount, saved.Model, saved.WorkDir, title)
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

// failureCause says why a turn failed in the words of the provider: an HTTP
// error shows its status and the message of its JSON body, not the body, and
// an error inside the stream shows its kind and message.
func failureCause(err error) string {
	var streamError *anthropic.StreamError
	if errors.As(err, &streamError) {
		return "provider " + strings.ReplaceAll(streamError.Type, "_", " ") + " · " + streamError.Message
	}
	var apiError *anthropic.APIError
	if !errors.As(err, &apiError) {
		return err.Error()
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(apiError.Body), &body) != nil || body.Error.Message == "" {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d · %s", apiError.StatusCode, body.Error.Message)
}
