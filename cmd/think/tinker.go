package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/tinker"
)

// tinkerWatcher runs /tinker for one chat. After a round that this chat
// started finishes, the chat switches to the new checkpoint, so the model
// keeps improving on the user's sessions while they work.
type tinkerWatcher struct {
	// watching is the round number this chat waits for; 0 means none.
	watching int
}

func (w *tinkerWatcher) command(out io.Writer, argument string, store *session.Store, workDir, system string, tools []anthropic.ToolDefinition) error {
	dir, err := tinker.Dir()
	if err != nil {
		return err
	}
	state, err := tinker.Load(dir)
	if err != nil {
		return err
	}
	if changed, err := tinker.Settle(&state); err != nil {
		return err
	} else if changed {
		if err := tinker.Save(dir, state); err != nil {
			return err
		}
	}
	if argument == "" {
		fmt.Fprint(out, tinker.Describe(state))
		return nil
	}
	if strings.ContainsAny(argument, " \t\n") || strings.HasPrefix(argument, tinker.Prefix) {
		return fmt.Errorf("use one base model ID, for example Qwen/Qwen3-30B-A3B-Instruct-2507")
	}
	if state.Running != nil {
		fmt.Fprint(out, tinker.Describe(state))
		return nil
	}
	if state.BaseModel != argument {
		// A new base model starts a new adapter from the first session.
		state = tinker.State{BaseModel: argument, Trained: map[string]int{}}
	}
	traces, sessions, err := sessionTraces(store, workDir, system, tools, state.Trained)
	if err != nil {
		return err
	}
	if len(traces) == 0 {
		fmt.Fprintln(out, "No new session turns to train on since the last round.")
		return nil
	}
	round, err := tinker.Start(dir, &state, traces, sessions)
	if err != nil {
		return err
	}
	w.watching = round.Number
	fmt.Fprintf(out, "Tinker round %d: fine-tuning %s on %d sessions in the background. The chat switches to the new checkpoint when it is ready; /tinker shows progress.\n", round.Number, argument, len(traces))
	return nil
}

// sessionTraces exports every saved session of workDir that grew since its
// last trained round. It returns the traces and their message counts.
func sessionTraces(store *session.Store, workDir, system string, tools []anthropic.ToolDefinition, trained map[string]int) ([]tinker.Trace, map[string]int, error) {
	summaries, err := store.List(workDir)
	if err != nil {
		return nil, nil, err
	}
	var traces []tinker.Trace
	counts := map[string]int{}
	for _, summary := range summaries {
		if summary.MessageCount <= trained[summary.ID] {
			continue
		}
		loaded, err := store.Load(summary.ID)
		if err != nil {
			return nil, nil, err
		}
		trace := tinker.NewTrace(loaded.ID, loaded.Model, system, tools, loaded.Messages)
		if !trace.Trainable() {
			continue
		}
		traces = append(traces, trace)
		counts[loaded.ID] = len(loaded.Messages)
	}
	return traces, counts, nil
}

// poll applies a finished round. It returns the checkpoint to switch to when
// the round this chat waits for produced one.
func (w *tinkerWatcher) poll(out io.Writer) (string, bool) {
	if w.watching == 0 {
		return "", false
	}
	dir, err := tinker.Dir()
	if err != nil {
		return "", false
	}
	state, err := tinker.Load(dir)
	if err != nil {
		return "", false
	}
	changed, err := tinker.Settle(&state)
	if err != nil {
		fmt.Fprintf(out, "inkling: tinker: %v\n", err)
		return "", false
	}
	if changed {
		if err := tinker.Save(dir, state); err != nil {
			fmt.Fprintf(out, "inkling: tinker: %v\n", err)
		}
	}
	if state.Running != nil && state.Running.Number == w.watching {
		return "", false
	}
	round := w.watching
	w.watching = 0
	if state.Rounds < round || state.SamplerPath == "" {
		fmt.Fprintf(out, "Tinker round %d did not finish: %s\n", round, state.LastError)
		return "", false
	}
	fmt.Fprintf(out, "Tinker round %d finished (loss %.4f). Now using %s; /model switches back.\n", round, state.LastLoss, state.SamplerPath)
	return state.SamplerPath, true
}
