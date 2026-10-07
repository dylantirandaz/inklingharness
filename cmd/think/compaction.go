package main

import (
	"context"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/latency"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/terminal"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// compactingStatus is the terminal status while a compaction runs in the background.
const compactingStatus = "Compacting context in background"

// compactionResult is the one value that a background worker sends.
type compactionResult struct {
	outcome *agent.Outcome
	err     error
}

// backgroundCompaction compacts a history snapshot while the chat waits for
// input. A nil pointer means that no compaction exists. A non-nil value is
// running while finished is nil, and finished after the chat goroutine
// receives the result. Only the chat goroutine reads or changes the fields.
// The worker goroutine uses only the values captured at start: it finishes
// its own timing record, which locks itself, and sends exactly one result.
type backgroundCompaction struct {
	snapshot []anthropic.EncodedMessage
	cancel   context.CancelFunc
	timing   *latency.Recorder
	results  <-chan compactionResult
	finished *compactionResult
}

// startBackgroundCompaction starts a worker on history. The caller must not
// change the elements of history until the worker result is received.
func startBackgroundCompaction(ctx context.Context, client *anthropic.Client, config agent.Config, toolSet *tools.Set, history []anthropic.EncodedMessage, options *options) *backgroundCompaction {
	workerConfig := config
	// The worker must not call back into chat state: approval reads the
	// terminal, and a checkpoint changes the session.
	workerConfig.Approve, workerConfig.Checkpoint = nil, nil
	workerContext, cancel := context.WithCancel(ctx)
	workerContext, timing := timedContext(workerContext, options)
	// The buffer lets the worker end even when the chat never receives.
	results := make(chan compactionResult, 1)
	go func() {
		outcome, err := agent.Compact(workerContext, client, workerConfig, toolSet, history, agent.SilentObserver{})
		// The worker finishes its own record, so the duration does not include
		// the time that the result waits for the next user action.
		timing.Finish(err)
		results <- compactionResult{outcome: outcome, err: err}
	}()
	return &backgroundCompaction{snapshot: history, cancel: cancel, timing: timing, results: results}
}

func (b *backgroundCompaction) running() bool {
	return b.finished == nil
}

// receive records the worker result. Call it only with a value received from results.
func (b *backgroundCompaction) receive(result compactionResult) {
	b.finished = &result
}

// wait returns the worker result. If ctx ends first, wait cancels the worker
// and returns the result of the canceled request, which can include usage.
func (b *backgroundCompaction) wait(ctx context.Context) compactionResult {
	if b.running() {
		select {
		case result := <-b.results:
			b.receive(result)
		case <-ctx.Done():
			b.cancel()
			b.receive(<-b.results)
		}
	}
	// The worker has ended; cancel releases the context resources.
	b.cancel()
	return *b.finished
}

// stop cancels the worker and waits until it ends.
func (b *backgroundCompaction) stop() compactionResult {
	b.cancel()
	return b.wait(context.Background())
}

// promptWithCompaction reads one terminal prompt while a compaction runs or
// waits to start. When start is not nil, the chat goroutine calls it at the
// first key that edits the prompt, so a resumed session that the user leaves
// without typing sends no compaction request. settle applies a finished
// compaction while the prompt stays open, so the footer shows the smaller
// history at once. Screen methods lock the screen, so the prompt helper and
// the chat goroutine can both use it. The helper always ends before this
// function returns. If settle fails, the open prompt closes and the error
// returns.
func promptWithCompaction(ctx context.Context, screen *terminal.Screen, running *backgroundCompaction, start func() *backgroundCompaction, settle func() error) (string, error) {
	promptContext, closePrompt := context.WithCancel(ctx)
	defer closePrompt()
	var typing <-chan struct{}
	if start != nil {
		typing = screen.ArmTyping()
	}
	var results <-chan compactionResult
	if running != nil {
		screen.SetBackgroundWork(compactingStatus)
		results = running.results
	} else {
		// A compaction that ended while a turn ran no longer runs.
		screen.SetBackgroundWork("")
	}
	prompts := make(chan inputLine, 1)
	go func() {
		text, err := screen.Prompt(promptContext, promptPlaceholder)
		prompts <- inputLine{text: text, err: err}
	}()
	for {
		select {
		case <-typing:
			typing = nil
			running = start()
			results = running.results
			screen.SetBackgroundWork(compactingStatus)
		case result := <-results:
			results = nil
			running.receive(result)
			if err := settle(); err != nil {
				closePrompt()
				<-prompts
				return "", err
			}
		case line := <-prompts:
			return line.text, line.err
		}
	}
}

// compactionEnd tells how a finished background compaction changed the session.
type compactionEnd int

const (
	// compactionApplied: the compacted history replaced the session history.
	compactionApplied compactionEnd = iota
	// compactionStale: the session history changed after the snapshot, so the
	// compacted history does not describe it.
	compactionStale
	// compactionFailed: the request or the summary failed; history is unchanged.
	compactionFailed
)

// settleCompaction returns current with the result of a compaction of
// snapshot. The usage is always added, because a failed or discarded request
// still used tokens.
func settleCompaction(current session.Session, snapshot []anthropic.EncodedMessage, result compactionResult) (session.Session, compactionEnd) {
	current.Usage = current.Usage.Add(result.outcome.Usage)
	if result.err != nil {
		return current, compactionFailed
	}
	if !sameHistory(snapshot, current.Messages) {
		return current, compactionStale
	}
	current.Messages = result.outcome.Messages
	return current, compactionApplied
}

// sameHistory reports whether history still has the length and the last
// message of snapshot. The chat settles a compaction before it changes
// history, so this guard keeps a summary of an old history out of the session
// if a later change misses that rule.
func sameHistory(snapshot, history []anthropic.EncodedMessage) bool {
	if len(snapshot) != len(history) {
		return false
	}
	if len(history) == 0 {
		return true
	}
	return history[len(history)-1].Equal(snapshot[len(snapshot)-1])
}
