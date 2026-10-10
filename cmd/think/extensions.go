package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/extend"
	"github.com/dylantirandaz/inklingharness/internal/mcp"
	"github.com/dylantirandaz/inklingharness/internal/permission"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/rewind"
	"github.com/dylantirandaz/inklingharness/internal/tools"
	"github.com/dylantirandaz/inklingharness/internal/web"
)

// extensions are the user's settings, tools, hooks, commands, skills, and
// project memory for one working directory. Loading reads a few small files
// and starts nothing; an MCP server starts when the model first uses it.
type extensions struct {
	workDir  string
	rules    permission.Rules
	hooks    extend.Hooks
	servers  *mcp.Manager
	tools    []tools.Tool
	commands []extend.Command
	skills   []extend.Skill
	memory   string
}

func loadExtensions(workDir string) (*extensions, error) {
	settings, err := extend.LoadSettings(workDir)
	if err != nil {
		return nil, err
	}
	rules, err := permission.Parse(settings.Allow, settings.Deny)
	if err != nil {
		return nil, fmt.Errorf("permission rules: %w", err)
	}
	custom, err := extend.LoadTools(workDir)
	if err != nil {
		return nil, err
	}
	userCommands, err := extend.LoadCommands(workDir)
	if err != nil {
		return nil, err
	}
	for _, command := range userCommands {
		if slices.Contains(chatCommands, command.Name) {
			return nil, fmt.Errorf("command %s: the name /%s belongs to chat; rename the file", command.Source, command.Name)
		}
	}
	skills, err := extend.LoadSkills(workDir)
	if err != nil {
		return nil, err
	}
	memory, err := extend.LoadMemory(workDir)
	if err != nil {
		return nil, err
	}
	loaded := &extensions{workDir: workDir, rules: rules, hooks: settings.Hooks, tools: custom,
		commands: append(extend.BuiltinCommands(), userCommands...), skills: skills, memory: memory}
	if len(settings.MCPServers) > 0 {
		configs := make([]mcp.ServerConfig, len(settings.MCPServers))
		for index, server := range settings.MCPServers {
			configs[index] = mcp.ServerConfig{Name: server.Name, Command: server.Command, Args: server.Args, Env: server.Env, Description: server.Description}
		}
		if loaded.servers, err = mcp.NewManager(configs, workDir); err != nil {
			return nil, fmt.Errorf("mcp servers: %w", err)
		}
		loaded.tools = append(loaded.tools, loaded.servers.Tools()...)
	}
	return loaded, nil
}

// close stops the MCP servers that started.
func (e *extensions) close() error {
	if e.servers == nil {
		return nil
	}
	return e.servers.Close()
}

// systemPrompt is the part of the system prompt that the extensions add. It
// is the same for every request of a session, so the cached prefix stays
// valid; a fact saved with remember loads in the next session.
func (e *extensions) systemPrompt() string {
	var sections []string
	if e.memory != "" {
		sections = append(sections, e.memory)
	}
	if text := extend.SkillsPrompt(e.skills); text != "" {
		sections = append(sections, text)
	}
	if e.servers != nil {
		if text := e.servers.PromptSection(); text != "" {
			sections = append(sections, text)
		}
	}
	return strings.Join(sections, "\n\n")
}

// toolSet adds web_search, web_fetch, remember, ask_user, then custom and MCP
// tools in a fixed order. ask_user is absent when no user can answer.
func (e *extensions) toolSet(standard *tools.Set, ask questionAsker) (*tools.Set, error) {
	all := append([]tools.Tool(nil), standard.All()...)
	all = append(all, web.SearchTool(), web.FetchTool(), extend.MemoryTool(e.workDir))
	if ask != nil {
		all = append(all, askUserTool(ask))
	}
	return tools.NewSet(append(all, e.tools...)...)
}

// planCommand is the built-in read-only plan command. Its absence is a
// programmer error in package extend.
func planCommand() extend.Command {
	for _, command := range extend.BuiltinCommands() {
		if command.Name == "plan" {
			return command
		}
	}
	panic("extend: the built-in plan command is missing")
}

func (e *extensions) command(name string) (extend.Command, bool) {
	for _, command := range e.commands {
		if command.Name == name {
			return command, true
		}
	}
	return extend.Command{}, false
}

// prompt adds the context of prompt hooks after the user's text.
func (e *extensions) prompt(ctx context.Context, text string) (string, error) {
	extra, err := e.hooks.RunPrompt(ctx, e.workDir, text)
	if err != nil {
		return "", fmt.Errorf("prompt hook: %w", err)
	}
	if extra == "" {
		return text, nil
	}
	return text + "\n\n" + extra, nil
}

// turnPolicy is how one prompt may use tools.
type turnPolicy struct {
	// readOnly refuses every tool that changes files or runs commands, as in
	// plan mode. The tool list stays the same, so the prompt cache stays
	// valid.
	readOnly bool
	// beforeChange runs once before the first tool that can change files,
	// so an undo snapshot is complete before any change. Nil does nothing.
	beforeChange func(context.Context) error
}

// configure adds the gate, the after-tool hooks, and the permission rules to
// config. ask is the interactive approval; it runs only when no rule decides.
func (e *extensions) configure(config *agent.Config, toolSet *tools.Set, policy turnPolicy, ask func(context.Context, anthropic.ToolUseBlock) (bool, error)) {
	config.Gate = func(ctx context.Context, call anthropic.ToolUseBlock) (*agent.Refusal, error) {
		verdict, rule := e.rules.Check(e.workDir, call.Name, call.Input)
		switch verdict {
		case permission.Deny:
			return &agent.Refusal{Reason: "the permission rule " + rule + " denies this call"}, nil
		case permission.Allow, permission.Ask:
		default:
			return nil, fmt.Errorf("unknown permission verdict %v", verdict)
		}
		changes := !readOnlyCall(toolSet, call)
		if changes && policy.readOnly {
			return &agent.Refusal{Reason: "this is a read-only plan; investigate and reply with a plan, without changes"}, nil
		}
		decision, err := e.hooks.RunBeforeTool(ctx, e.workDir, extend.ToolEvent{Tool: call.Name, Input: call.Input})
		if err != nil {
			return nil, fmt.Errorf("before-tool hook: %w", err)
		}
		if decision.Deny {
			return &agent.Refusal{Reason: "a before-tool hook denies this call: " + decision.Reason}, nil
		}
		if changes && policy.beforeChange != nil {
			if err := policy.beforeChange(ctx); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	config.AfterTool = func(ctx context.Context, call anthropic.ToolUseBlock, result tools.Result) (string, error) {
		output, err := e.hooks.RunAfterTool(ctx, e.workDir, extend.ToolEvent{Tool: call.Name, Input: call.Input, Result: &result})
		if err != nil || output == "" {
			return "", err
		}
		return "After-tool hook output:\n" + output, nil
	}
	config.Approve = func(ctx context.Context, call anthropic.ToolUseBlock) (bool, error) {
		if verdict, _ := e.rules.Check(e.workDir, call.Name, call.Input); verdict == permission.Allow && staysInside(e.workDir, call) {
			return true, nil
		}
		if ask == nil {
			return false, nil
		}
		return ask(ctx, call)
	}
}

// readOnlyCall reports whether a call cannot change files or run commands.
// The research task is read-only by construction.
func readOnlyCall(toolSet *tools.Set, call anthropic.ToolUseBlock) bool {
	// ask_user changes nothing; plan mode may ask the user.
	if call.Name == "task" || call.Name == "ask_user" || call.Name == "inspect_images" || call.Name == "context_read" || call.Name == "context_checkpoint" {
		return true
	}
	tool, found := toolSet.Lookup(call.Name)
	return found && tool.ReadOnly
}

// checkpoint is the state before one prompt: the conversation and the files
// of the working tree. messages shares its backing array with the history,
// which is never changed in place, so a checkpoint costs no copy and stays
// exact after a compaction. Snapshot is the zero value outside git.
type checkpoint struct {
	prompt   string
	messages []anthropic.EncodedMessage
	snapshot rewind.Snapshot
}

// pendingSnapshot takes a snapshot in the background while the first model
// request runs; the first tool that can change files waits for it.
type pendingSnapshot struct {
	done     chan struct{}
	snapshot rewind.Snapshot
	err      error
}

func startSnapshot(ctx context.Context, workDir string) *pendingSnapshot {
	pending := &pendingSnapshot{done: make(chan struct{})}
	go func() {
		defer close(pending.done)
		pending.snapshot, pending.err = rewind.Take(ctx, workDir)
	}()
	return pending
}

// wait returns the snapshot. Outside git there is none, and that is not an
// error: undo then restores only the conversation.
func (p *pendingSnapshot) wait(ctx context.Context) (rewind.Snapshot, error) {
	select {
	case <-p.done:
	case <-ctx.Done():
		return rewind.Snapshot{}, ctx.Err()
	}
	if errors.Is(p.err, rewind.ErrNotRepository) {
		return rewind.Snapshot{}, nil
	}
	if p.err != nil {
		return rewind.Snapshot{}, fmt.Errorf("undo snapshot: %w", p.err)
	}
	return p.snapshot, nil
}

// settled waits until the snapshot ends, whatever its result, so that no tool
// changes a file while git reads the tree. Only a cancelled ctx is an error;
// a failed snapshot is reported after the turn, and the turn goes on.
func (p *pendingSnapshot) settled(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// chatCommands are the commands of chat itself. A user command cannot take
// one of these names.
var chatCommands = []string{"help", "status", "usage", "tools", "verbose", "model", "effort", "compact", "clear", "fork", "title", "goal", "context", "undo", "rewind", "quit", "exit"}

// customCommand finds the user or built-in command that a one-line
// "/name arguments" input names.
func (e *extensions) customCommand(input string) (extend.Command, string, bool) {
	if !strings.HasPrefix(input, "/") || strings.Contains(input, "\n") {
		return extend.Command{}, "", false
	}
	name, arguments, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	if slices.Contains(chatCommands, name) {
		return extend.Command{}, "", false
	}
	command, found := e.command(name)
	return command, strings.TrimSpace(arguments), found
}

// commandHelp lists the user's commands for /help; plan is in the fixed text.
func (e *extensions) commandHelp() string {
	var out strings.Builder
	for _, command := range e.commands {
		if command.Source == extend.BuiltinSource {
			continue
		}
		if out.Len() == 0 {
			out.WriteString("Your commands:\n")
		}
		fmt.Fprintf(&out, "  /%-16s %s\n", command.Name, command.Description)
	}
	if len(e.skills) > 0 {
		fmt.Fprintf(&out, "Skills: %d (the model reads one when a task needs it)\n", len(e.skills))
	}
	return out.String()
}

// rewindTarget chooses the checkpoint for /undo (the last one) or /rewind N.
// /rewind without N lists the prompts. It writes the reason when there is no
// target.
func rewindTarget(command, argument string, checkpoints []checkpoint, output io.Writer) (int, bool) {
	if len(checkpoints) == 0 {
		fmt.Fprintln(output, "Nothing to undo in this run of think.")
		return 0, false
	}
	switch command {
	case "/undo":
		if argument != "" {
			fmt.Fprintln(output, "/undo takes no argument; use /rewind N.")
			return 0, false
		}
		return len(checkpoints) - 1, true
	case "/rewind":
		if argument == "" {
			for index, saved := range checkpoints {
				first, _, _ := strings.Cut(saved.prompt, "\n")
				fmt.Fprintf(output, "%3d  %s\n", index+1, presentation.Safe(first))
			}
			fmt.Fprintln(output, "Use /rewind N to restore the state from before prompt N.")
			return 0, false
		}
		number, err := strconv.Atoi(argument)
		if err != nil || number < 1 || number > len(checkpoints) {
			fmt.Fprintf(output, "Enter a prompt number from 1 to %d.\n", len(checkpoints))
			return 0, false
		}
		return number - 1, true
	default:
		panic("rewindTarget: unknown command " + command)
	}
}

// restoreFiles brings the working tree back to a checkpoint. Outside git
// there is no snapshot, and only the conversation goes back.
func restoreFiles(ctx context.Context, workDir string, target checkpoint) ([]string, error) {
	if target.snapshot.Tree == "" {
		return nil, nil
	}
	changed, err := rewind.Restore(ctx, workDir, target.snapshot)
	if err != nil {
		return nil, fmt.Errorf("restore files: %w", err)
	}
	return changed, nil
}

func printRewind(output io.Writer, target checkpoint, changed []string) {
	first, _, _ := strings.Cut(target.prompt, "\n")
	fmt.Fprintf(output, "Back to before: %s\n", presentation.Safe(first))
	switch {
	case target.snapshot.Tree == "":
		fmt.Fprintln(output, "Files are unchanged: this folder is not in a git repository.")
	case len(changed) == 0:
		fmt.Fprintln(output, "No file had changed.")
	case len(changed) == 1:
		fmt.Fprintln(output, "Restored 1 file:")
		fmt.Fprintln(output, "  "+presentation.Safe(changed[0]))
	default:
		fmt.Fprintf(output, "Restored %d files:\n", len(changed))
		for _, path := range changed {
			fmt.Fprintln(output, "  "+presentation.Safe(path))
		}
	}
}

// staysInside reports whether a file tool writes inside root after symbolic
// links resolve. Permission rules match paths as text, so an allow rule must
// not approve a write through a link that points out of root. Other tools
// have no path and pass.
func staysInside(root string, call anthropic.ToolUseBlock) bool {
	switch call.Name {
	case "write_file", "edit_file":
	default:
		return true
	}
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Input, &input); err != nil || input.Path == "" {
		return false
	}
	path := input.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	// The file may not exist yet: resolve the nearest directory that exists.
	directory, rest := filepath.Clean(path), ""
	for {
		resolved, err := filepath.EvalSymlinks(directory)
		if err == nil {
			relative, err := filepath.Rel(realRoot, filepath.Join(resolved, rest))
			return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return false
		}
		rest = filepath.Join(filepath.Base(directory), rest)
		directory = parent
	}
}
