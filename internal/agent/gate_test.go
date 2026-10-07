package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// The gate sees every call, read-only ones too, before approval. A refusal
// keeps the tool from running and reaches the model with its reason; an error
// stops the run; an after-tool error stops the run after the tool ran.
func TestGateAndAfterTool(t *testing.T) {
	var reads, writes, approvals atomic.Int32
	toolSet, err := tools.NewSet(
		tools.Tool{Name: "look", ReadOnly: true, InputSchema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (tools.Result, error) {
			reads.Add(1)
			return tools.Result{Content: "seen"}, nil
		}},
		tools.Tool{Name: "change", InputSchema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (tools.Result, error) {
			writes.Add(1)
			return tools.Result{Content: "changed"}, nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	approve := func(context.Context, anthropic.ToolUseBlock) (bool, error) {
		approvals.Add(1)
		return true, nil
	}
	look := anthropic.ToolUseBlock{ID: "1", Name: "look", Input: json.RawMessage(`{}`)}
	change := anthropic.ToolUseBlock{ID: "2", Name: "change", Input: json.RawMessage(`{}`)}

	var gated []string
	config := Config{Approve: approve, Gate: func(_ context.Context, call anthropic.ToolUseBlock) (*Refusal, error) {
		gated = append(gated, call.Name)
		if call.Name == "change" {
			return &Refusal{Reason: "plan mode is read-only"}, nil
		}
		return nil, nil
	}}
	results, _, err := runTools(context.Background(), nil, config, toolSet, []anthropic.ToolUseBlock{look, change}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	refused := results[1].(anthropic.ToolResultBlock)
	if !refused.IsError || !strings.Contains(refused.Content, "plan mode is read-only") {
		t.Fatalf("refusal result = %+v", refused)
	}
	if writes.Load() != 0 || approvals.Load() != 0 || reads.Load() != 1 || strings.Join(gated, ",") != "look,change" {
		t.Fatalf("writes %d approvals %d reads %d gated %v", writes.Load(), approvals.Load(), reads.Load(), gated)
	}

	broken := errors.New("hook exited 1")
	config = Config{Approve: approve, Gate: func(context.Context, anthropic.ToolUseBlock) (*Refusal, error) { return nil, broken }}
	if _, _, err := runTools(context.Background(), nil, config, toolSet, []anthropic.ToolUseBlock{change}, SilentObserver{}); !errors.Is(err, broken) || writes.Load() != 0 {
		t.Fatalf("gate error = %v, writes %d", err, writes.Load())
	}

	var seen []string
	config = Config{Approve: approve, AfterTool: func(_ context.Context, call anthropic.ToolUseBlock, result tools.Result) (string, error) {
		seen = append(seen, call.Name+"="+result.Content)
		if call.Name == "change" {
			return "", broken
		}
		return "vet: line 3: unused x", nil
	}}
	results, _, err = runTools(context.Background(), nil, config, toolSet, []anthropic.ToolUseBlock{look}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if got := results[0].(anthropic.ToolResultBlock).Content; got != "seen\n\nvet: line 3: unused x" {
		t.Fatalf("result with after-tool text = %q", got)
	}
	_, _, err = runTools(context.Background(), nil, config, toolSet, []anthropic.ToolUseBlock{change}, SilentObserver{})
	if !errors.Is(err, broken) || writes.Load() != 1 || strings.Join(seen, ",") != "look=seen,change=changed" {
		t.Fatalf("after-tool error = %v, writes %d, seen %v", err, writes.Load(), seen)
	}
}

// A notice reaches the model once: with the prompt when it is pending at
// the start, else with the next tool results.
func TestNoticesReachTheModelOnce(t *testing.T) {
	var pending []string
	config := Config{Notices: func() []string {
		taken := pending
		pending = nil
		return taken
	}}
	if got := withNotices("fix it", config); got != "fix it" {
		t.Fatalf("no notices: %q", got)
	}
	pending = []string{"background job 1 exited with code 2", "background job 2 exited with code 0"}
	if got := withNotices("fix it", config); got != "fix it\n\nNotices since the last message:\n- background job 1 exited with code 2\n- background job 2 exited with code 0" {
		t.Fatalf("with notices: %q", got)
	}
	if got := withNotices("", config); got != "" {
		t.Fatalf("a notice repeated: %q", got)
	}
}

// standardTools is the standard tool set for a test, with its background
// jobs closed when the test ends.
func standardTools(t *testing.T, root string) (*tools.Set, error) {
	t.Helper()
	output := t.TempDir()
	jobs := tools.NewJobs(output)
	t.Cleanup(func() {
		if err := jobs.Close(); err != nil {
			t.Error(err)
		}
	})
	return tools.Standard(root, output, jobs)
}
