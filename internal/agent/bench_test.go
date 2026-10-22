package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// The benchmarks use a synthetic conversation in the shape of the recorded
// SWE-bench trials of Inkling Small: one tool call in each assistant message,
// reasoning in each reply, and tool results with the measured sizes (bash
// median 313 and p90 2,183 characters, read_file median 2,386 and p90 9,940).
// Sixty turns give about 40K tokens of context.

var benchResultSizes = []int{313, 2386, 313, 520, 2183, 9940, 313, 1200, 2386, 313}

const benchTurns = 60

func benchText(seed string, size int) string {
	var text strings.Builder
	text.Grow(size)
	for line := 0; text.Len() < size; line++ {
		fmt.Fprintf(&text, "%s line %d: \"value\" <tag> & tab\t%d\n", seed, line, line*7)
	}
	return text.String()[:size]
}

func benchReply(turn int) anthropic.Message {
	return anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{
		anthropic.ThinkingBlock{Thinking: benchText("reasoning", 600), Signature: strings.Repeat("s", 120)},
		anthropic.ToolUseBlock{ID: fmt.Sprintf("toolu_%024d", turn), Name: "bash", Input: json.RawMessage(`{"command":"python -m pytest tests/test_a.py -x -q"}`)},
	}}
}

func benchResult(turn int) anthropic.Message {
	return anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.ToolResultBlock{
		ToolUseID: fmt.Sprintf("toolu_%024d", turn), Content: benchText("output", benchResultSizes[turn%len(benchResultSizes)]),
	}}}
}

func benchHistory(tb testing.TB, turns int) []anthropic.EncodedMessage {
	tb.Helper()
	history, err := appendMessage(nil, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "Fix the bug in a.py."}}})
	if err != nil {
		tb.Fatal(err)
	}
	for turn := range turns {
		for _, message := range []anthropic.Message{benchReply(turn), benchResult(turn)} {
			if history, err = appendMessage(history, message); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return history
}

// BenchmarkAppendTurn adds one reply and its tool result to a 60-turn
// history, as each turn does.
func BenchmarkAppendTurn(b *testing.B) {
	history := benchHistory(b, benchTurns)
	reply, result := benchReply(benchTurns), benchResult(benchTurns)
	b.ReportAllocs()
	for b.Loop() {
		// The three-index slice makes each append copy, as a full backing
		// array does in a real run.
		next, err := appendMessage(history[:len(history):len(history)], reply)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := appendMessage(next, result); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWithoutOldReasoning removes the old reasoning of a 60-turn
// history, as each new prompt does.
func BenchmarkWithoutOldReasoning(b *testing.B) {
	history := benchHistory(b, benchTurns)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := withoutOldReasoning(history); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkShouldCompact is the compaction check of each turn.
func BenchmarkShouldCompact(b *testing.B) {
	history := benchHistory(b, benchTurns)
	config := Config{CompactTokens: 200000}
	b.ReportAllocs()
	for b.Loop() {
		if ShouldCompact(config, history, 40000) {
			b.Fatal("compaction at 40K tokens")
		}
	}
}

// scriptedTransport answers every request with a reply that calls bash, and
// ends the conversation after turns replies. It reads the whole request body,
// as a server does.
type scriptedTransport struct {
	turns    int64
	requests atomic.Int64
	toolUse  [][]byte
	endTurn  []byte
}

func newScriptedTransport(turns int) *scriptedTransport {
	event := func(reply *bytes.Buffer, name, data string) {
		fmt.Fprintf(reply, "event: %s\ndata: %s\n\n", name, data)
	}
	transport := &scriptedTransport{turns: int64(turns)}
	for turn := range turns {
		var reply bytes.Buffer
		event(&reply, "message_start", `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":812,"cache_read_input_tokens":39424,"output_tokens":1}}}`)
		event(&reply, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		for delta := range 40 {
			event(&reply, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step %d of the plan "}}`, delta))
		}
		event(&reply, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
		event(&reply, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		event(&reply, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_%d","name":"bash","input":{}}}`, turn))
		event(&reply, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"run %d\"}"}}`, turn))
		event(&reply, "content_block_stop", `{"type":"content_block_stop","index":1}`)
		event(&reply, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":420}}`)
		event(&reply, "message_stop", `{"type":"message_stop"}`)
		transport.toolUse = append(transport.toolUse, reply.Bytes())
	}
	var end bytes.Buffer
	event(&end, "message_start", `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":10,"output_tokens":1}}}`)
	event(&end, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	event(&end, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`)
	event(&end, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	event(&end, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
	event(&end, "message_stop", `{"type":"message_stop"}`)
	transport.endTurn = end.Bytes()
	return transport
}

func (t *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if _, err := io.Copy(io.Discard, request.Body); err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}
	reply := t.endTurn
	if index := t.requests.Add(1) - 1; index < t.turns {
		reply = t.toolUse[index]
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(bytes.NewReader(reply)), Request: request,
	}, nil
}

// BenchmarkRunTurns runs a whole 60-turn conversation against an in-memory
// API and a bash tool that returns results of the measured sizes. It reports
// the time and memory of the harness for each turn, without network or
// model time.
func BenchmarkRunTurns(b *testing.B) {
	var calls atomic.Int64
	bash := tools.Tool{
		Name: "bash", Description: benchText("bash", 400),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		Run: func(context.Context, json.RawMessage) (tools.Result, error) {
			call := calls.Add(1)
			return tools.Result{Content: benchText("output", benchResultSizes[int(call)%len(benchResultSizes)])}, nil
		},
	}
	toolSet, err := tools.NewSet(bash)
	if err != nil {
		b.Fatal(err)
	}
	config := Config{
		Model: "thinkingmachines/inkling-small", MaxTokens: 16384, MaxTurns: benchTurns + 1, CompactTokens: 200000,
		System: benchText("system", 8000), Approve: func(context.Context, anthropic.ToolUseBlock) (bool, error) { return true, nil },
	}
	transport := newScriptedTransport(benchTurns)
	client := anthropic.NewClientWithTransport("key", "http://127.0.0.1:1", nil, transport)
	b.ReportAllocs()
	for b.Loop() {
		transport.requests.Store(0)
		outcome, err := Run(context.Background(), client, config, toolSet, nil, Prompt{Text: "Fix the bug in a.py."}, SilentObserver{})
		if err != nil {
			b.Fatal(err)
		}
		if outcome.Turns != benchTurns+1 {
			b.Fatalf("turns = %d", outcome.Turns)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*(benchTurns+1)), "ns/turn")
}
