package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func sseEvent(eventType string, payload string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, payload)
}

func messageStart() string {
	return sseEvent("message_start", `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":100,"cache_read_input_tokens":40,"output_tokens":1}}}`)
}

func toolUseBlock(index int, id, name, input string) string {
	return sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name)) +
		sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, index, input)) +
		sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
}

func textBlock(index int, text string) string {
	return sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)) +
		sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, text)) +
		sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
}

func thinkingBlock(index int, signature string) string {
	return sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, index)) +
		sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":%q}}`, index, signature)) +
		sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
}

func messageEnd(stopReason string, outputTokens int) string {
	return sseEvent("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}`, stopReason, outputTokens)) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
}

type wireMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

func encodeHistory(t *testing.T, messages ...anthropic.Message) []anthropic.EncodedMessage {
	t.Helper()
	history := make([]anthropic.EncodedMessage, 0, len(messages))
	for _, message := range messages {
		encoded, err := anthropic.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		history = append(history, encoded)
	}
	return history
}

func decodeMessage(t *testing.T, message anthropic.EncodedMessage) anthropic.Message {
	t.Helper()
	decoded, err := message.Decode()
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// TestRunRoundTripsToolCalls scripts two model turns: the first asks for two
// parallel read_file calls, the second must receive the assistant turn
// unchanged plus both results in call order, then ends the turn.
func TestRunRoundTripsToolCalls(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("bravo"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var secondRequest []wireMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			_, _ = fmt.Fprint(w, messageStart(),
				thinkingBlock(0, "sig-1"),
				toolUseBlock(1, "toolu_a", "read_file", `{"path":"a.txt"}`),
				toolUseBlock(2, "toolu_b", "read_file", `{"path":"b.txt"}`),
				messageEnd("tool_use", 20))
		case 2:
			var body struct {
				Messages []wireMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode second request: %v", err)
			}
			secondRequest = body.Messages
			_, _ = fmt.Fprint(w, messageStart(), textBlock(0, "done"), messageEnd("end_turn", 5))
		default:
			t.Errorf("unexpected third request")
		}
	}))
	defer server.Close()

	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	client := anthropic.NewClient("key", server.URL, nil)
	outcome, err := Run(context.Background(), client, Config{Model: "m", MaxTokens: 50, MaxTurns: 5}, toolSet, nil, Prompt{Text: "read both"}, recordingObserver{t: t})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if outcome.Turns != 2 || outcome.FinalText != "done" {
		t.Errorf("outcome turns=%d text=%q", outcome.Turns, outcome.FinalText)
	}
	wantUsage := anthropic.Usage{InputTokens: 200, CacheReadInputTokens: 80, OutputTokens: 25}
	if outcome.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", outcome.Usage, wantUsage)
	}
	if len(outcome.TurnLatencies) != 2 {
		t.Errorf("latencies = %v", outcome.TurnLatencies)
	}

	if len(secondRequest) != 3 {
		t.Fatalf("second request carried %d messages, want 3", len(secondRequest))
	}
	assistant := secondRequest[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 3 {
		t.Fatalf("assistant turn = %+v", assistant)
	}
	if !strings.Contains(string(assistant.Content[0]), `"signature":"sig-1"`) {
		t.Errorf("thinking signature lost: %s", assistant.Content[0])
	}
	results := secondRequest[2]
	if results.Role != "user" || len(results.Content) != 2 {
		t.Fatalf("tool results turn = %+v", results)
	}
	for index, expected := range []struct{ id, text string }{{"toolu_a", "alpha"}, {"toolu_b", "bravo"}} {
		var result struct {
			ID      string `json:"tool_use_id"`
			Content string `json:"content"`
			IsError bool   `json:"is_error"`
		}
		if err := json.Unmarshal(results.Content[index], &result); err != nil {
			t.Fatal(err)
		}
		if result.ID != expected.id || result.IsError || !strings.Contains(result.Content, expected.text) {
			t.Errorf("read result %d lost content or call order: %+v", index, result)
		}
	}
}

// The provider reuses its prompt cache only when the request prefix is
// byte-identical. Each request must therefore repeat the earlier messages
// exactly as an earlier request sent them.
func TestRunResendsHistoryBytesUnchanged(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("<data> & \"quotes\" é"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var requests [][]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, body.Messages)
		switch len(requests) {
		case 1:
			_, _ = fmt.Fprint(w, messageStart(), thinkingBlock(0, "sig-1"), toolUseBlock(1, "toolu_a", "read_file", `{"path":"a.txt"}`), messageEnd("tool_use", 3))
		case 2:
			_, _ = fmt.Fprint(w, messageStart(), textBlock(0, "next <file>"), toolUseBlock(1, "toolu_b", "read_file", `{"path":"b.txt"}`), messageEnd("tool_use", 3))
		default:
			_, _ = fmt.Fprint(w, messageStart(), textBlock(0, "done"), messageEnd("end_turn", 1))
		}
	}))
	defer server.Close()
	toolSet, err := standardTools(t, root)
	if err != nil {
		t.Fatal(err)
	}
	history := encodeHistory(t,
		anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "earlier <request> & \u2028"}}},
		anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "earlier answer"}}},
	)
	_, err = Run(context.Background(), anthropic.NewClient("key", server.URL, nil), Config{Model: "m", MaxTokens: 50, MaxTurns: 5}, toolSet, history, Prompt{Text: "read <both>"}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	for index, message := range history {
		if !bytes.Equal(requests[0][index], message.Wire()) {
			t.Fatalf("history message %d was encoded again:\n%s\n%s", index, requests[0][index], message.Wire())
		}
	}
	for turn := 1; turn < len(requests); turn++ {
		previous, current := requests[turn-1], requests[turn]
		if len(current) != len(previous)+2 {
			t.Fatalf("request %d has %d messages after %d", turn+1, len(current), len(previous))
		}
		for index, message := range previous {
			if !bytes.Equal(current[index], message) {
				t.Fatalf("request %d changed message %d:\n%s\n%s", turn+1, index, message, current[index])
			}
		}
	}
}

type recordingObserver struct {
	t *testing.T
}

func (recordingObserver) Text(string)                                                        {}
func (recordingObserver) Thinking(string)                                                    {}
func (recordingObserver) ToolCallStart(string)                                               {}
func (recordingObserver) ToolCall(string, json.RawMessage)                                   {}
func (recordingObserver) ToolResult(string, tools.Result, time.Duration)                     {}
func (recordingObserver) TurnDone(int, anthropic.Usage, anthropic.StopReason, time.Duration) {}
func (recordingObserver) Status(string)                                                      {}
func (o recordingObserver) UnknownEvent(eventType string) {
	o.t.Errorf("unexpected unknown event %q", eventType)
}
