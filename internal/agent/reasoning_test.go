package agent

import (
	"encoding/json"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

func TestWithoutOldReasoning(t *testing.T) {
	encode := func(role anthropic.Role, blocks ...anthropic.ContentBlock) anthropic.EncodedMessage {
		t.Helper()
		encoded, err := anthropic.EncodeMessage(anthropic.Message{Role: role, Content: blocks})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	thinking := anthropic.ThinkingBlock{Thinking: "plan", Signature: "sig"}
	redacted := anthropic.RedactedThinkingBlock{Data: "opaque"}
	call := anthropic.ToolUseBlock{ID: "1", Name: "look", Input: json.RawMessage(`{}`)}
	history := []anthropic.EncodedMessage{
		encode(anthropic.RoleUser, anthropic.TextBlock{Text: "first"}),
		encode(anthropic.RoleAssistant, thinking, redacted, anthropic.TextBlock{Text: "done"}),
		encode(anthropic.RoleUser, anthropic.TextBlock{Text: "second"}),
		encode(anthropic.RoleAssistant, thinking),
		encode(anthropic.RoleUser, anthropic.TextBlock{Text: "third"}),
		encode(anthropic.RoleAssistant, thinking, call),
		encode(anthropic.RoleUser, anthropic.ToolResultBlock{ToolUseID: "1", Content: "seen"}),
	}
	original := append([]anthropic.EncodedMessage(nil), history...)
	got, err := withoutOldReasoning(history)
	if err != nil {
		t.Fatal(err)
	}
	want := []anthropic.EncodedMessage{
		history[0],
		encode(anthropic.RoleAssistant, anthropic.TextBlock{Text: "done"}),
		history[2],
		// Only reasoning: removing it would leave an empty message.
		history[3],
		history[4],
		// The last assistant message keeps the reasoning of its open call.
		history[5],
		history[6],
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for index := range want {
		if !got[index].Equal(want[index]) {
			t.Errorf("message %d = %s, want %s", index, got[index].Wire(), want[index].Wire())
		}
		if !history[index].Equal(original[index]) {
			t.Errorf("input message %d changed", index)
		}
	}
}
