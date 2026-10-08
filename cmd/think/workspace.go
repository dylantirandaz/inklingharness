package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/attachment"
	"github.com/dylantirandaz/inklingharness/internal/project"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// workspace is the setup that run and rpc share: the request settings, the
// client, the project, the tools, and the user's extensions.
type workspace struct {
	config  agent.Config
	client  *anthropic.Client
	project project.Context
	toolSet *tools.Set
	ext     *extensions
	jobs    *tools.Jobs
}

// usageError is a setup failure that the user's flags or key cause, which
// exits with code 2 like other usage errors.
type usageError struct{ error }

// openWorkspace opens the workspace of the current directory. It returns an
// exit code other than 0 when it fails; it has then written the reason to
// stderr.
func openWorkspace(ctx context.Context, o *options, stderr io.Writer) (*workspace, int) {
	root, err := os.Getwd()
	if err == nil {
		var work *workspace
		if work, err = newWorkspace(ctx, o, root, stderr); err == nil {
			return work, 0
		}
	}
	fmt.Fprintf(stderr, "inkling: %v\n", err)
	var usage usageError
	if errors.As(err, &usage) {
		return nil, 2
	}
	return nil, 1
}

// newWorkspace sets up tools and extensions rooted at root. Warnings, such as
// stale output folders that cannot be removed, go to stderr. No user can
// answer ask_user in a workspace.
func newWorkspace(ctx context.Context, o *options, root string, stderr io.Writer) (*workspace, error) {
	config, err := o.agentConfig()
	if err != nil {
		return nil, usageError{err}
	}
	projectContext, err := project.Inspect(ctx, root)
	if err != nil {
		return nil, err
	}
	ext, err := loadExtensions(projectContext.WorkDir)
	if err != nil {
		return nil, err
	}
	config.System += "\n\n" + projectContext.SystemPrompt()
	if text := ext.systemPrompt(); text != "" {
		config.System += "\n\n" + text
	}
	client, err := o.client()
	if err != nil {
		return nil, errors.Join(usageError{err}, ext.close())
	}
	prewarm(ctx, client)
	store, err := session.DefaultStore()
	if err != nil {
		return nil, errors.Join(err, ext.close())
	}
	config.ContextStore = store
	removeStaleOutputs(store, stderr)
	// run, rpc, and acp never save this session. Its ID only names the
	// directory that keeps large command outputs after the command ends.
	outputs, err := session.New(projectContext.WorkDir, o.model, o.effort)
	if err != nil {
		return nil, errors.Join(err, ext.close())
	}
	toolSet, jobs, err := sessionTools(store, projectContext.WorkDir, outputs.ID, ext, nil)
	if err != nil {
		return nil, errors.Join(err, ext.close())
	}
	return &workspace{config: config, client: client, project: projectContext, toolSet: toolSet, ext: ext, jobs: jobs}, nil
}

// configure gives a turn the policy of the workspace: rules, hooks,
// approval, and job notices.
func (w *workspace) configure(config *agent.Config, policy turnPolicy, ask func(context.Context, anthropic.ToolUseBlock) (bool, error)) {
	w.ext.configure(config, w.toolSet, policy, ask)
	config.Notices = w.jobs.Notices
}

// close stops background jobs and MCP servers.
func (w *workspace) close() error {
	return errors.Join(w.jobs.Close(), w.ext.close())
}

// prompt prepares one request: attachments and @mentions, then the context
// of prompt hooks.
func (w *workspace) prompt(ctx context.Context, text string, files []string) (attachment.Prepared, agent.Prompt, error) {
	prepared, err := attachment.Prepare(ctx, w.project.WorkDir, text, files, w.config.ContextStore)
	if err != nil {
		return attachment.Prepared{}, agent.Prompt{}, err
	}
	withHooks, err := w.ext.prompt(ctx, prepared.Prompt)
	if err != nil {
		return attachment.Prepared{}, agent.Prompt{}, err
	}
	return prepared, agent.Prompt{Text: withHooks}, nil
}

// rpcCommand serves one conversation to an editor over stdin and stdout; see
// serveRPC for the protocol. History lives only in this process.
func rpcCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int) {
	flags := flag.NewFlagSet("think rpc", flag.ContinueOnError)
	flags.SetOutput(stderr)
	o := bindOptions(flags)
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "think rpc: prompts come over stdin")
		return 2
	}
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
	var history []anthropic.EncodedMessage
	run := func(ctx context.Context, text string, observer agent.Observer, approve func(context.Context, anthropic.ToolUseBlock) (bool, error)) (*agent.Outcome, error) {
		config := work.config
		ask := approve
		if o.approveAll {
			ask = func(context.Context, anthropic.ToolUseBlock) (bool, error) { return true, nil }
		}
		work.configure(&config, turnPolicy{}, ask)
		_, prompt, err := work.prompt(ctx, text, nil)
		if err != nil {
			return &agent.Outcome{Messages: history}, err
		}
		outcome, err := agent.Run(ctx, work.client, config, work.toolSet, history, prompt, observer)
		history = outcome.Messages
		return outcome, err
	}
	if err := serveRPC(ctx, stdin, stdout, run); err != nil {
		fmt.Fprintf(stderr, "inkling: rpc: %v\n", err)
		return 1
	}
	return 0
}
