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

// openWorkspace returns an exit code other than 0 when it fails; it has then
// written the reason to stderr.
func openWorkspace(ctx context.Context, o *options, stderr io.Writer) (*workspace, int) {
	config, err := o.agentConfig()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 1
	}
	projectContext, err := project.Inspect(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 1
	}
	ext, err := loadExtensions(projectContext.WorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 1
	}
	config.System += "\n\n" + projectContext.SystemPrompt()
	if text := ext.systemPrompt(); text != "" {
		config.System += "\n\n" + text
	}
	client, err := o.client()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 2
	}
	prewarm(ctx, client)
	store, err := session.DefaultStore()
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 1
	}
	removeStaleOutputs(store, stderr)
	// run and rpc never save this session. Its ID only names the directory
	// that keeps large command outputs after the command ends.
	outputs, err := session.New(projectContext.WorkDir, o.model, o.effort)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 1
	}
	toolSet, jobs, err := sessionTools(store, projectContext.WorkDir, outputs.ID, ext)
	if err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return nil, 1
	}
	return &workspace{config: config, client: client, project: projectContext, toolSet: toolSet, ext: ext, jobs: jobs}, 0
}

// close stops background jobs and MCP servers.
func (w *workspace) close() error {
	return errors.Join(w.jobs.Close(), w.ext.close())
}

// prompt prepares one request: attachments and @mentions, then the context
// of prompt hooks.
func (w *workspace) prompt(ctx context.Context, text string, files []string) (attachment.Prepared, agent.Prompt, error) {
	prepared, err := attachment.Prepare(ctx, w.project.WorkDir, text, files)
	if err != nil {
		return attachment.Prepared{}, agent.Prompt{}, err
	}
	withHooks, err := w.ext.prompt(ctx, prepared.Prompt)
	if err != nil {
		return attachment.Prepared{}, agent.Prompt{}, err
	}
	return prepared, agent.Prompt{Text: withHooks, Images: prepared.Images}, nil
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
		work.ext.configure(&config, work.toolSet, turnPolicy{}, ask)
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
