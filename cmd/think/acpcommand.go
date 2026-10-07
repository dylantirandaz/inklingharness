package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

// acpCommand serves editors that speak the Agent Client Protocol, such as
// Zed, over stdin and stdout; see serveACP.
func acpCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int) {
	flags := flag.NewFlagSet("think acp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	o := bindOptions(flags)
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "think acp: prompts come over stdin")
		return 2
	}
	if _, err := o.agentConfig(); err != nil {
		fmt.Fprintf(stderr, "inkling: %v\n", err)
		return 2
	}
	backend := &acpSessions{options: o, warnings: stderr, sessions: map[string]*acpConversation{}}
	defer func() {
		if err := backend.close(); err != nil {
			fmt.Fprintf(stderr, "inkling: %v\n", err)
			exitCode = 1
		}
	}()
	if err := serveACP(ctx, stdin, stdout, backend); err != nil {
		fmt.Fprintf(stderr, "inkling: acp: %v\n", err)
		return 1
	}
	return 0
}

// acpSessions gives each ACP session its own workspace, rooted at the folder
// that the editor names, and its own history in memory.
type acpSessions struct {
	options  *options
	warnings io.Writer
	mutex    sync.Mutex
	next     int
	sessions map[string]*acpConversation
}

type acpConversation struct {
	work    *workspace
	history []anthropic.EncodedMessage
}

func (a *acpSessions) NewSession(ctx context.Context, cwd string) (string, error) {
	work, err := newWorkspace(ctx, a.options, cwd, a.warnings)
	if err != nil {
		return "", err
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.next++
	id := "session-" + strconv.Itoa(a.next)
	a.sessions[id] = &acpConversation{work: work}
	return id, nil
}

// Prompt runs one turn. serveACP runs at most one prompt for each session at
// a time, so the history of a session needs no lock of its own.
func (a *acpSessions) Prompt(ctx context.Context, sessionID string, prompt agent.Prompt, observer agent.Observer, approve func(context.Context, anthropic.ToolUseBlock) (bool, error)) (*agent.Outcome, error) {
	a.mutex.Lock()
	current, found := a.sessions[sessionID]
	a.mutex.Unlock()
	if !found {
		return nil, fmt.Errorf("unknown session %q", sessionID)
	}
	work := current.work
	config := work.config
	ask := approve
	if a.options.approveAll {
		ask = func(context.Context, anthropic.ToolUseBlock) (bool, error) { return true, nil }
	}
	work.configure(&config, turnPolicy{}, ask)
	_, prepared, err := work.prompt(ctx, prompt.Text, nil)
	if err != nil {
		return &agent.Outcome{Messages: current.history}, err
	}
	prepared.Images = append(prepared.Images, prompt.Images...)
	outcome, err := agent.Run(ctx, work.client, config, work.toolSet, current.history, prepared, observer)
	current.history = outcome.Messages
	return outcome, err
}

func (a *acpSessions) close() error {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	var errs []error
	for _, current := range a.sessions {
		errs = append(errs, current.work.close())
	}
	return errors.Join(errs...)
}
