package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamFixture mirrors the documented event sequence: a thinking block with
// a signature, text, an unknown block kind, an unknown event type, and a
// tool_use block whose input arrives as partial JSON.
const streamFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me read"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" the file."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"I will "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"read it."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"future_block","payload":{"a":1}}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: future_event
data: {"type":"future_event","detail":"x"}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\": "}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

data: [DONE]

`

func TestStreamAccumulatesReplyAndKeepsWireFormat(t *testing.T) {
	var receivedBody map[string]json.RawMessage
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(streamFixture))
	}))
	defer server.Close()

	var record bytes.Buffer
	client := NewClient("test-key", server.URL, bufferRecorder{&record})
	var unknownEvents []string
	var textDeltas []string
	response, err := client.Stream(context.Background(), Request{
		Model:     "claude-test",
		MaxTokens: 100,
		System:    "be brief",
		Messages:  mustEncode(t, Message{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "hi"}}}),
		Tools:     []ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Extra:     map[string]json.RawMessage{"effort": json.RawMessage(`"high"`)},
	}, func(event StreamEvent) error {
		switch typed := event.(type) {
		case TextDelta:
			textDeltas = append(textDeltas, typed.Text)
		case UnknownEvent:
			unknownEvents = append(unknownEvents, typed.Type)
		case ThinkingDelta, ToolUseStart, BlockStop:
		default:
			t.Errorf("unexpected event %T", event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if got := receivedHeaders.Get("authorization"); got != "Bearer test-key" {
		t.Errorf("authorization = %q", got)
	}
	if got := receivedHeaders.Get("anthropic-version"); got != apiVersion {
		t.Errorf("anthropic-version = %q", got)
	}
	if string(receivedBody["stream"]) != "true" {
		t.Errorf("stream field = %s", receivedBody["stream"])
	}
	if string(receivedBody["cache_control"]) != `{"type":"ephemeral"}` {
		t.Errorf("cache_control = %s", receivedBody["cache_control"])
	}
	if string(receivedBody["effort"]) != `"high"` {
		t.Errorf("extra field effort = %s", receivedBody["effort"])
	}
	if string(receivedBody["system"]) != `"be brief"` {
		t.Errorf("system = %s", receivedBody["system"])
	}

	if response.StopReason != StopToolUse {
		t.Errorf("stop reason = %q", response.StopReason)
	}
	if response.Model != "claude-test" {
		t.Errorf("model = %q", response.Model)
	}
	wantUsage := Usage{InputTokens: 10, CacheCreationInputTokens: 3, CacheReadInputTokens: 5, OutputTokens: 42}
	if response.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", response.Usage, wantUsage)
	}
	if strings.Join(textDeltas, "") != "I will read it." {
		t.Errorf("text deltas = %q", textDeltas)
	}
	if len(unknownEvents) != 1 || unknownEvents[0] != "future_event" {
		t.Errorf("unknown events = %v", unknownEvents)
	}

	content := response.Message.Content
	if len(content) != 4 {
		t.Fatalf("content has %d blocks, want 4", len(content))
	}
	if got, want := content[0], (ThinkingBlock{Thinking: "Let me read the file.", Signature: "sig123"}); got != want {
		t.Errorf("block 0 = %+v, want %+v", got, want)
	}
	if got, want := content[1], (TextBlock{Text: "I will read it."}); got != want {
		t.Errorf("block 1 = %+v, want %+v", got, want)
	}
	opaque, isOpaque := content[2].(OpaqueBlock)
	if !isOpaque || string(opaque.Raw) != `{"type":"future_block","payload":{"a":1}}` {
		t.Errorf("block 2 = %+v", content[2])
	}
	call, isCall := content[3].(ToolUseBlock)
	if !isCall || call.ID != "toolu_1" || call.Name != "read_file" {
		t.Fatalf("block 3 = %+v", content[3])
	}
	var input map[string]string
	if err := json.Unmarshal(call.Input, &input); err != nil || input["path"] != "a.txt" {
		t.Errorf("tool input = %s (err %v)", call.Input, err)
	}

	encoded, err := json.Marshal(response.Message)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	for _, fragment := range []string{
		`{"type":"thinking","thinking":"Let me read the file.","signature":"sig123"}`,
		`{"type":"future_block","payload":{"a":1}}`,
		`{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}}`,
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Errorf("encoded reply lacks %s:\n%s", fragment, encoded)
		}
	}
	if record.String() != streamFixture {
		t.Errorf("recorder did not receive the raw stream")
	}
}

func TestStreamReturnsAPIErrorForNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer server.Close()

	client := NewClient("test-key", server.URL, nil)
	_, err := client.Stream(context.Background(), Request{Model: "m", MaxTokens: 1}, func(StreamEvent) error { return nil })
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusTooManyRequests || !strings.Contains(apiError.Body, "slow down") {
		t.Fatalf("err = %v, want *APIError with status 429", err)
	}
}

func TestStreamRejectsExtraFieldThatCollides(t *testing.T) {
	client := NewClient("test-key", "http://unused", nil)
	_, err := client.Stream(context.Background(), Request{
		Model:     "m",
		MaxTokens: 1,
		Extra:     map[string]json.RawMessage{"model": json.RawMessage(`"other"`)},
	}, func(StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("err = %v, want collision error", err)
	}
}

type bufferRecorder struct {
	buffer *bytes.Buffer
}

func (b bufferRecorder) Begin([][]byte) (io.WriteCloser, error) {
	return nopCloser{b.buffer}, nil
}

type nopCloser struct {
	*bytes.Buffer
}

func (nopCloser) Close() error { return nil }
