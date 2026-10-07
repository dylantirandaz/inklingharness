package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// The parts add up to the estimate that starts compaction, and the history
// shows its largest kind first.
func TestContextPartsMatchTheCompactionEstimate(t *testing.T) {
	toolSet, err := tools.NewSet(tools.Tool{Name: "look", Description: "Look.", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var messages []anthropic.EncodedMessage
	for _, message := range []anthropic.Message{
		{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "fix it"}}},
		{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "Reading."}, anthropic.ToolUseBlock{ID: "1", Name: "look", Input: json.RawMessage(`{}`)}}},
		{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.ToolResultBlock{ToolUseID: "1", Content: strings.Repeat("x", 9000)}}},
	} {
		encoded, err := anthropic.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, encoded)
	}
	config := agent.Config{System: "be brief", EnableTasks: true}
	parts, err := contextParts(config, toolSet, messages)
	if err != nil {
		t.Fatal(err)
	}
	if parts[1].count != 2 {
		t.Fatalf("tool definitions = %d, want 2 with the task tool", parts[1].count)
	}
	if parts[2].name != "tool results" || parts[2].count != 1 || parts[2].bytes != 9000 {
		t.Fatalf("largest history part = %+v", parts[2])
	}
	history := 0
	for _, part := range parts[2:] {
		history += part.bytes
	}
	// The block contents are a lower bound of the wire size that the
	// compaction estimate counts, and they are most of it.
	if estimate := agent.EstimateTokens(messages) * bytesPerToken; history > estimate || history < estimate*9/10 {
		t.Fatalf("history parts = %d bytes, compaction estimate = %d bytes", history, estimate)
	}
}
