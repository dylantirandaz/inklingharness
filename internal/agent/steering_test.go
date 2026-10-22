package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

// scriptedServer answers request N with replies[N-1] and records every
// request body.
func scriptedServer(t *testing.T, replies ...string) (*anthropic.Client, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		bodies = append(bodies, body)
		if len(bodies) > len(replies) {
			t.Errorf("unexpected request %d", len(bodies))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = fmt.Fprint(w, replies[len(bodies)-1])
	}))
	t.Cleanup(server.Close)
	return anthropic.NewClient("key", server.URL, nil), &bodies
}

// lastMessage decodes the last message of a request body.
func lastMessage(t *testing.T, body []byte) wireMessage {
	t.Helper()
	var request struct {
		Messages []wireMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	return request.Messages[len(request.Messages)-1]
}

// blockKinds lists the type and, for text, the text of each block.
func blockKinds(t *testing.T, message wireMessage) []string {
	t.Helper()
	var kinds []string
	for _, raw := range message.Content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		if block.Type == "text" {
			kinds = append(kinds, "text:"+block.Text)
			continue
		}
		kinds = append(kinds, block.Type)
	}
	return kinds
}

// queue returns a Steering hook that gives each batch once, in order.
func queue(batches ...[]string) (func() []string, *int) {
	calls := 0
	return func() []string {
		calls++
		if len(batches) == 0 {
			return nil
		}
		batch := batches[0]
		batches = batches[1:]
		return batch
	}, &calls
}

// A steering message sent while a tool ran reaches the model with the tool
// results: after them and after the notices, in the same user message.
func TestSteeringFollowsToolResults(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	client, bodies := scriptedServer(t,
		messageStart()+toolUseBlock(0, "toolu_a", "read_file", `{"path":"a.txt"}`)+messageEnd("tool_use", 3),
		messageStart()+textBlock(0, "done")+messageEnd("end_turn", 1),
	)
	notices := 0
	steering, _ := queue([]string{"make the fix minimal", "keep the tests"})
	config := Config{Model: "m", MaxTokens: 50, MaxTurns: 5, Steering: steering, Notices: func() []string {
		notices++
		if notices == 2 {
			return []string{"job 1 ended"}
		}
		return nil
	}}
	outcome, err := Run(context.Background(), client, config, toolSet, nil, Prompt{Text: "read a"}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 2 || outcome.FinalText != "done" {
		t.Fatalf("requests = %d, final text %q", len(*bodies), outcome.FinalText)
	}
	got := blockKinds(t, lastMessage(t, (*bodies)[1]))
	want := []string{"tool_result", "text:Notices since the last message:\n- job 1 ended", "text:make the fix minimal", "text:keep the tests"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("results message = %q, want %q", got, want)
	}
}

// A steering message that comes after the final answer makes the model
// answer again, with the message as the last user message.
func TestSteeringAfterFinalAnswerAsksAgain(t *testing.T) {
	client, bodies := scriptedServer(t,
		messageStart()+textBlock(0, "first answer")+messageEnd("end_turn", 1),
		messageStart()+textBlock(0, "second answer")+messageEnd("end_turn", 1),
	)
	steering, _ := queue([]string{"also check the README"})
	var saved []Outcome
	config := Config{Model: "m", MaxTokens: 50, MaxTurns: 5, Steering: steering, Checkpoint: func(outcome Outcome) error {
		saved = append(saved, outcome)
		return nil
	}}
	toolSet, err := standardTools(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := Run(context.Background(), client, config, toolSet, nil, Prompt{Text: "fix it"}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 2 || outcome.Turns != 2 || outcome.FinalText != "second answer" {
		t.Fatalf("requests = %d, turns %d, final text %q", len(*bodies), outcome.Turns, outcome.FinalText)
	}
	last := lastMessage(t, (*bodies)[1])
	if got := blockKinds(t, last); last.Role != "user" || fmt.Sprint(got) != fmt.Sprint([]string{"text:also check the README"}) {
		t.Fatalf("last message = %s %q", last.Role, got)
	}
	// The saved history holds the steering message before the request that
	// answers it, so a crash cannot lose it.
	steered := saved[len(saved)-2].Messages
	if role := steered[len(steered)-1].Role(); role != anthropic.RoleUser || len(steered) != 3 {
		t.Fatalf("checkpoint before the second request ends with %s after %d messages", role, len(steered))
	}
}

// When no request can follow, Run leaves the queue alone, so the caller can
// still send the message.
func TestSteeringWaitsWhenNoTurnIsLeft(t *testing.T) {
	client, bodies := scriptedServer(t, messageStart()+textBlock(0, "answer")+messageEnd("end_turn", 1))
	steering, calls := queue([]string{"too late"})
	config := Config{Model: "m", MaxTokens: 50, MaxTurns: 1, Steering: steering}
	toolSet, err := standardTools(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), client, config, toolSet, nil, Prompt{Text: "fix it"}, SilentObserver{}); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 || *calls != 0 {
		t.Fatalf("requests = %d, steering calls %d", len(*bodies), *calls)
	}
}

// A hook with nothing queued sends the same bytes as no hook.
func TestEmptySteeringLeavesRequestsUnchanged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	var runs [][][]byte
	for _, steering := range []func() []string{nil, func() []string { return nil }} {
		client, bodies := scriptedServer(t,
			messageStart()+toolUseBlock(0, "toolu_a", "read_file", `{"path":"a.txt"}`)+messageEnd("tool_use", 3),
			messageStart()+textBlock(0, "done")+messageEnd("end_turn", 1),
		)
		config := Config{Model: "m", MaxTokens: 50, MaxTurns: 5, Steering: steering}
		if _, err := Run(context.Background(), client, config, toolSet, nil, Prompt{Text: "read a"}, SilentObserver{}); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, *bodies)
	}
	if len(runs[0]) != 2 || len(runs[1]) != 2 {
		t.Fatalf("requests = %d and %d", len(runs[0]), len(runs[1]))
	}
	for index := range runs[0] {
		if !bytes.Equal(runs[0][index], runs[1][index]) {
			t.Fatalf("request %d differs:\n%s\n%s", index+1, runs[0][index], runs[1][index])
		}
	}
}
