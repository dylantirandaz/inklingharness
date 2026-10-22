package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The benchmarks use a synthetic conversation in the shape of the recorded
// SWE-bench trials of Inkling Small: one tool call in each assistant message,
// reasoning in each reply, and tool results with the measured sizes (bash
// median 313 and p90 2,183 characters, read_file median 2,386 and p90 9,940).
// Sixty turns give about 40K tokens of context.

// benchResultSizes cycles through the measured result sizes.
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

// benchHistory returns the encoded messages of a conversation with turns
// tool calls and results.
func benchHistory(tb testing.TB, turns int) []EncodedMessage {
	tb.Helper()
	messages := []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "Fix the bug in a.py. The test test_add fails."}}}}
	for turn := range turns {
		id := fmt.Sprintf("toolu_%024d", turn)
		messages = append(messages,
			Message{Role: RoleAssistant, Content: []ContentBlock{
				ThinkingBlock{Thinking: benchText("reasoning", 600), Signature: strings.Repeat("s", 120)},
				ToolUseBlock{ID: id, Name: "bash", Input: json.RawMessage(`{"command":"python -m pytest tests/test_a.py -x -q"}`)},
			}},
			Message{Role: RoleUser, Content: []ContentBlock{ToolResultBlock{ToolUseID: id, Content: benchText("output", benchResultSizes[turn%len(benchResultSizes)])}}},
		)
	}
	history := make([]EncodedMessage, 0, len(messages))
	for _, message := range messages {
		encoded, err := EncodeMessage(message)
		if err != nil {
			tb.Fatal(err)
		}
		history = append(history, encoded)
	}
	return history
}

func benchTools() []ToolDefinition {
	definitions := make([]ToolDefinition, 0, 9)
	for _, name := range []string{"read_file", "read_files", "write_file", "edit_file", "bash", "bash_job", "glob", "grep", "context_read"} {
		definitions = append(definitions, ToolDefinition{
			Name:        name,
			Description: benchText(name, 400),
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"]}`),
		})
	}
	return definitions
}

func benchRequest(tb testing.TB) Request {
	return Request{
		Model: "thinkingmachines/inkling-small", MaxTokens: 16384,
		System: benchText("system", 8000), Messages: benchHistory(tb, benchTurns), Tools: benchTools(),
		Extra: map[string]json.RawMessage{"output_config": json.RawMessage(`{"effort":"high"}`)},
	}
}

// benchReply is one streamed reply: 300 reasoning deltas, a signature, and a
// bash call whose input arrives in five parts.
func benchReply() []byte {
	var reply bytes.Buffer
	event := func(name, data string) { fmt.Fprintf(&reply, "event: %s\ndata: %s\n\n", name, data) }
	event("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"thinkingmachines/inkling-small","role":"assistant","content":[],"usage":{"input_tokens":812,"cache_read_input_tokens":39424,"output_tokens":1}}}`)
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
	for delta := range 300 {
		event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step %d of the plan "}}`, delta))
	}
	event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"`+strings.Repeat("s", 120)+`"}}`)
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"bash","input":{}}}`)
	for _, part := range []string{`{\"comm`, `and\":\"python -m`, ` pytest tests/`, `test_a.py -x`, ` -q\"}`} {
		event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"`+part+`"}}`)
	}
	event("content_block_stop", `{"type":"content_block_stop","index":1}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":420}}`)
	event("message_stop", `{"type":"message_stop"}`)
	return reply.Bytes()
}

// BenchmarkRequestBody builds and writes one request body of a 60-turn
// conversation with the same client, as each turn does.
func BenchmarkRequestBody(b *testing.B) {
	request := benchRequest(b)
	client := new(Client)
	b.ReportAllocs()
	for b.Loop() {
		body, err := client.encode(request)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, body.reader()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseReply parses one streamed reply with reasoning and a tool
// call.
func BenchmarkParseReply(b *testing.B) {
	reply := benchReply()
	b.SetBytes(int64(len(reply)))
	b.ReportAllocs()
	for b.Loop() {
		scanner := newEventScanner(bytes.NewReader(reply))
		builder := newResponseBuilder()
		for {
			event, err := scanner.next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			if err := builder.apply(event, func(StreamEvent) error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
		scanner.release()
		if _, err := builder.response(); err != nil {
			b.Fatal(err)
		}
	}
}

// replayTransport answers each request with the same reply and reads the
// whole request body, as a server does.
type replayTransport struct {
	reply []byte
}

func (t replayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if _, err := io.Copy(io.Discard, request.Body); err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(bytes.NewReader(t.reply)), Request: request,
	}, nil
}

// BenchmarkStreamTurn is one model turn of the client: build and send the
// request of a 60-turn conversation, then parse the streamed reply.
func BenchmarkStreamTurn(b *testing.B) {
	request := benchRequest(b)
	client := NewClientWithTransport("key", "http://127.0.0.1:1", nil, replayTransport{reply: benchReply()})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := client.Stream(context.Background(), request, func(StreamEvent) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeMessage encodes one reply and one tool result, as each turn
// does once.
func BenchmarkEncodeMessage(b *testing.B) {
	reply := Message{Role: RoleAssistant, Content: []ContentBlock{
		ThinkingBlock{Thinking: benchText("reasoning", 600), Signature: strings.Repeat("s", 120)},
		ToolUseBlock{ID: "toolu_01", Name: "read_file", Input: json.RawMessage(`{"path":"django/forms/models.py"}`)},
	}}
	result := Message{Role: RoleUser, Content: []ContentBlock{ToolResultBlock{ToolUseID: "toolu_01", Content: benchText("output", 9940)}}}
	b.ReportAllocs()
	for b.Loop() {
		for _, message := range []Message{reply, result} {
			if _, err := EncodeMessage(message); err != nil {
				b.Fatal(err)
			}
		}
	}
}
