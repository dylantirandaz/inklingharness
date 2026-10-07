package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func TestDeniedWriteDoesNotChangeFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			fmt.Fprint(w, messageStart(), toolUseBlock(0, "write", "write_file", `{"path":"file.txt","content":"replace"}`), messageEnd("tool_use", 2))
		} else {
			fmt.Fprint(w, messageStart(), textBlock(0, "Write denied."), messageEnd("end_turn", 2))
		}
	}))
	defer server.Close()
	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := Run(context.Background(), anthropic.NewClient("key", server.URL, nil), Config{Model: "m", MaxTokens: 100, MaxTurns: 2}, toolSet, nil, Prompt{Text: "replace the file"}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "keep" {
		t.Fatalf("denied operation changed file: %q, %v", content, err)
	}
	result, ok := decodeMessage(t, outcome.Messages[2]).Content[0].(anthropic.ToolResultBlock)
	if !ok || !result.IsError || result.ToolUseID != "write" {
		t.Fatalf("denial was not paired with the call: %#v", outcome.Messages)
	}
}

func TestCancellationKeepsEffectsAndPairsUnrunCalls(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, messageStart(),
			toolUseBlock(0, "first", "write_file", `{"path":"first","content":"done"}`),
			toolUseBlock(1, "second", "write_file", `{"path":"second","content":"no"}`),
			toolUseBlock(2, "third", "write_file", `{"path":"third","content":"no"}`), messageEnd("tool_use", 7))
	}))
	defer server.Close()
	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Model: "m", MaxTokens: 100, MaxTurns: 2, Approve: func(ctx context.Context, call anthropic.ToolUseBlock) (bool, error) {
		if call.ID == "second" {
			cancel()
			return true, nil
		}
		return true, nil
	}}
	outcome, err := Run(ctx, anthropic.NewClient("key", server.URL, nil), config, toolSet, nil, Prompt{Text: "write three files"}, SilentObserver{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "first"))
	if err != nil || string(content) != "done" {
		t.Fatalf("completed change was lost: %q, %v", content, err)
	}
	for _, name := range []string{"second", "third"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unrun file %s exists: %v", name, err)
		}
	}
	if len(outcome.Messages) != 3 || outcome.Usage.OutputTokens != 7 {
		t.Fatalf("completed response lost: %+v", outcome)
	}
	results := decodeMessage(t, outcome.Messages[2])
	for index, id := range []string{"first", "second", "third"} {
		result, ok := results.Content[index].(anthropic.ToolResultBlock)
		if !ok || result.ToolUseID != id || result.IsError != (index > 0) {
			t.Fatalf("result %d = %#v", index, results.Content[index])
		}
	}
}

func TestExpectedEditFailureStopsDependentBatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	config := Config{Approve: func(context.Context, anthropic.ToolUseBlock) (bool, error) {
		approvals++
		return true, nil
	}}
	calls := []anthropic.ToolUseBlock{
		{ID: "edit", Name: "edit_file", Input: json.RawMessage(`{"path":"file.txt","old_string":"not present","new_string":"replace"}`)},
		{ID: "write", Name: "write_file", Input: json.RawMessage(`{"path":"file.txt","content":"must not write"}`)},
		{ID: "check", Name: "read_file", Input: json.RawMessage(`{"path":"file.txt"}`)},
	}
	results, _, err := runTools(context.Background(), nil, config, toolSet, calls, SilentObserver{})
	if err != nil {
		t.Fatalf("expected edit failure should allow a repair turn: %v", err)
	}
	if approvals != 1 {
		t.Fatalf("approved %d calls, want only the failed edit", approvals)
	}
	for index, block := range results {
		result := block.(anthropic.ToolResultBlock)
		if result.ToolUseID != calls[index].ID || !result.IsError {
			t.Fatalf("unpaired failure at %d: %+v", index, result)
		}
		if index > 0 && !strings.HasPrefix(result.Content, "not run:") {
			t.Fatalf("dependent call ran: %+v", result)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "keep" {
		t.Fatalf("dependent write changed file: %q, %v", content, err)
	}
	reads := []anthropic.ToolUseBlock{
		{ID: "missing", Name: "read_file", Input: json.RawMessage(`{"path":"missing.txt"}`)},
		{ID: "existing", Name: "read_file", Input: json.RawMessage(`{"path":"file.txt"}`)},
	}
	results, _, err = runTools(context.Background(), nil, config, toolSet, reads, SilentObserver{})
	if err != nil || results[1].(anthropic.ToolResultBlock).IsError || !strings.Contains(results[1].(anthropic.ToolResultBlock).Content, "keep") {
		t.Fatalf("independent read was stopped: %#v, %v", results, err)
	}
}

// A prompt that follows a user message is joined to it. The join must not
// change the caller's history, which a session may still hold.
func TestFailedRequestKeepsHistoryWithoutMutatingCaller(t *testing.T) {
	history := encodeHistory(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "first request"}}})
	original := history[0]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "access denied", http.StatusForbidden) }))
	defer server.Close()
	toolSet, err := tools.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := Run(context.Background(), anthropic.NewClient("key", server.URL, nil), Config{Model: "m", MaxTokens: 100, MaxTurns: 2}, toolSet, history, Prompt{Text: "second request"}, SilentObserver{})
	var apiError *anthropic.APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 403 {
		t.Fatalf("error = %v", err)
	}
	if len(history) != 1 || !history[0].Equal(original) || len(outcome.Messages) != 1 {
		t.Fatalf("merge changed caller or added a message: %s %d", history[0].Wire(), len(outcome.Messages))
	}
	merged := decodeMessage(t, outcome.Messages[0])
	if len(merged.Content) != 2 || merged.Content[0] != (anthropic.TextBlock{Text: "first request"}) || merged.Content[1] != (anthropic.TextBlock{Text: "second request"}) {
		t.Fatalf("merge lost a prompt: %s", outcome.Messages[0].Wire())
	}
}

func TestCompactionKeepsNewestToolPair(t *testing.T) {
	history := encodeHistory(t,
		anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("old requirements ", 100)}}},
		anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("old work ", 100)}}},
		anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "read the file"}}},
		anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.ThinkingBlock{Thinking: "read", Signature: "signature"}, anthropic.ToolUseBlock{ID: "read", Name: "read_file", Input: json.RawMessage(`{"path":"file"}`)}}},
		anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.ToolResultBlock{ToolUseID: "read", Content: "exact source"}}},
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, messageStart(), textBlock(0, "Keep the required change. The last task is to read the file."), messageEnd("end_turn", 10))
	}))
	defer server.Close()
	toolSet, err := tools.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := Compact(context.Background(), anthropic.NewClient("key", server.URL, nil), Config{Model: "m", MaxTokens: 100}, toolSet, history, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Messages) != 3 || !outcome.Messages[1].Equal(history[3]) || !outcome.Messages[2].Equal(history[4]) {
		t.Fatalf("compaction changed the latest tool pair: %d messages", len(outcome.Messages))
	}
	if EstimateTokens(outcome.Messages) >= EstimateTokens(history) || outcome.Usage.OutputTokens != 10 {
		t.Fatalf("compaction did not reduce context or count usage: %+v", outcome)
	}
}

// The summary request must send the same tool list as a normal turn and no
// tool_choice. A different list, or tool_choice "none", changes the prompt
// prefix, and the provider then cannot reuse the prompt cache.
func TestCompactionRequestKeepsNormalTools(t *testing.T) {
	type requestTail struct {
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	var requests []requestTail
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request requestTail
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, request)
		if len(requests) == 1 {
			fmt.Fprint(w, messageStart(), textBlock(0, "The user wants a short change."), messageEnd("end_turn", 3))
		} else {
			fmt.Fprint(w, messageStart(), textBlock(0, "done"), messageEnd("end_turn", 2))
		}
	}))
	defer server.Close()
	toolSet, err := standardTools(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	history := encodeHistory(t,
		anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("old requirements ", 100)}}},
		anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("old work ", 100)}}},
		anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("more requirements ", 100)}}},
		anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("more work ", 100)}}},
	)
	config := Config{Model: "m", MaxTokens: 100, MaxTurns: 2, CompactTokens: 1, EnableTasks: true}
	outcome, err := Run(context.Background(), anthropic.NewClient("key", server.URL, nil), config, toolSet, history, Prompt{Text: "next request"}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || outcome.FinalText != "done" {
		t.Fatalf("requests=%d outcome=%+v", len(requests), outcome)
	}
	summary, normal := requests[0], requests[1]
	if !strings.Contains(string(normal.Tools), `"name":"task"`) || !bytes.Equal(summary.Tools, normal.Tools) {
		t.Fatalf("summary tools differ from normal tools:\n%s\n%s", summary.Tools, normal.Tools)
	}
	if summary.ToolChoice != nil || normal.ToolChoice != nil {
		t.Fatalf("tool_choice summary=%s normal=%s", summary.ToolChoice, normal.ToolChoice)
	}
	if outcome.LastInputTokens != 140 {
		t.Fatalf("last input tokens = %d, want only the latest request", outcome.LastInputTokens)
	}
}

func TestResearchTaskCannotWriteOrSpawnTasks(t *testing.T) {
	root := t.TempDir()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []anthropic.ToolDefinition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		for _, tool := range request.Tools {
			if tool.Name == "bash" || tool.Name == "write_file" || tool.Name == "edit_file" || tool.Name == "task" {
				t.Errorf("child received tool %s", tool.Name)
			}
		}
		if requests.Add(1) == 1 {
			fmt.Fprint(w, messageStart(), toolUseBlock(0, "write", "write_file", `{"path":"forbidden","content":"no"}`), messageEnd("tool_use", 3))
		} else {
			fmt.Fprint(w, messageStart(), textBlock(0, "Cannot write."), messageEnd("end_turn", 4))
		}
	}))
	defer server.Close()
	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	result, usage, err := runTask(context.Background(), anthropic.NewClient("key", server.URL, nil), Config{Model: "m", MaxTokens: 100, MaxTurns: 5, EnableTasks: true}, toolSet, json.RawMessage(`{"prompt":"inspect the project"}`))
	if err != nil || result.IsError || usage.OutputTokens != 7 {
		t.Fatalf("child result=%+v usage=%+v err=%v", result, usage, err)
	}
	if _, err := os.Stat(filepath.Join(root, "forbidden")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child wrote a file: %v", err)
	}
}
