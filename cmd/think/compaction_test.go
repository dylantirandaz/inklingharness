package main

import (
	"errors"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/session"
)

func encode(t *testing.T, role anthropic.Role, text string) anthropic.EncodedMessage {
	t.Helper()
	message, err := anthropic.EncodeMessage(anthropic.Message{Role: role, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func sameMessages(left, right []anthropic.EncodedMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !left[i].Equal(right[i]) {
			return false
		}
	}
	return true
}

func TestSettleCompactionAppliesOnlyToUnchangedHistory(t *testing.T) {
	snapshot := []anthropic.EncodedMessage{
		encode(t, anthropic.RoleUser, "one"),
		encode(t, anthropic.RoleAssistant, "two"),
		encode(t, anthropic.RoleUser, "three"),
		encode(t, anthropic.RoleAssistant, "four"),
	}
	compacted := []anthropic.EncodedMessage{encode(t, anthropic.RoleUser, "summary"), snapshot[3]}
	spent := anthropic.Usage{InputTokens: 7, OutputTokens: 3}
	prior := anthropic.Usage{InputTokens: 100, OutputTokens: 10}
	success := compactionResult{outcome: &agent.Outcome{Messages: compacted, Usage: spent}}
	failure := compactionResult{outcome: &agent.Outcome{Messages: snapshot, Usage: spent}, err: errors.New("compact: request failed")}
	appended := append(append([]anthropic.EncodedMessage(nil), snapshot...), encode(t, anthropic.RoleUser, "five"))
	replacedLast := append(append([]anthropic.EncodedMessage(nil), snapshot[:3]...), encode(t, anthropic.RoleAssistant, "four, joined"))
	tests := []struct {
		name     string
		history  []anthropic.EncodedMessage
		result   compactionResult
		end      compactionEnd
		messages []anthropic.EncodedMessage
	}{
		{name: "unchanged", history: snapshot, result: success, end: compactionApplied, messages: compacted},
		{name: "message added", history: appended, result: success, end: compactionStale, messages: appended},
		{name: "last message replaced", history: replacedLast, result: success, end: compactionStale, messages: replacedLast},
		{name: "failed", history: snapshot, result: failure, end: compactionFailed, messages: snapshot},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := session.Session{ID: "id", Messages: test.history, Usage: prior}
			settled, end := settleCompaction(current, snapshot, test.result)
			if end != test.end {
				t.Fatalf("end = %d, want %d", end, test.end)
			}
			if !sameMessages(settled.Messages, test.messages) {
				t.Fatalf("messages = %d entries, want %d", len(settled.Messages), len(test.messages))
			}
			if want := prior.Add(spent); settled.Usage != want {
				t.Fatalf("usage = %+v, want %+v", settled.Usage, want)
			}
			if !sameMessages(current.Messages, test.history) || current.Usage != prior {
				t.Fatal("settleCompaction changed its input session")
			}
		})
	}
}
