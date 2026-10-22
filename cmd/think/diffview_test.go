package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/terminal"
)

func TestDiffDocumentsSplitPerFile(t *testing.T) {
	patch := "diff --git a/change.txt b/change.txt\nindex 1..2 100644\n--- a/change.txt\n+++ b/change.txt\n@@ -1 +1 @@\n-before\n+after\n" +
		"diff --git a/new dir/new.txt b/new dir/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new dir/new.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n" +
		"diff --git a/image.bin b/image.bin\nnew file mode 100644\nBinary files /dev/null and b/image.bin differ\n"
	documents := diffDocuments(patch)
	if len(documents) != 3 {
		t.Fatalf("documents = %d, want 3", len(documents))
	}
	want := []string{"change.txt · +1 −1", "new dir/new.txt · new · +2 −0", "image.bin · binary"}
	joined := ""
	for index, document := range documents {
		if document.Title != want[index] || document.Format != terminal.DiffDocument {
			t.Fatalf("document %d = %q (%d), want %q", index, document.Title, document.Format, want[index])
		}
		joined += document.Text
	}
	if joined != patch {
		t.Fatalf("split changed the patch:\n%s", joined)
	}
	if got := diffDocuments("no header\n"); len(got) != 1 || got[0].Title != "Changes" {
		t.Fatalf("headerless patch = %+v", got)
	}
}

func encodedMessage(t *testing.T, role anthropic.Role, blocks ...anthropic.ContentBlock) anthropic.EncodedMessage {
	t.Helper()
	encoded, err := anthropic.EncodeMessage(anthropic.Message{Role: role, Content: blocks})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestLatestToolDocumentsCoverWholeTurn(t *testing.T) {
	messages := []anthropic.EncodedMessage{
		encodedMessage(t, anthropic.RoleUser, anthropic.TextBlock{Text: "earlier"}),
		encodedMessage(t, anthropic.RoleAssistant, anthropic.ToolUseBlock{ID: "old", Name: "bash", Input: json.RawMessage(`{"command":"old"}`)}),
		encodedMessage(t, anthropic.RoleUser, anthropic.ToolResultBlock{ToolUseID: "old", Content: "old output"}),
		encodedMessage(t, anthropic.RoleAssistant, anthropic.TextBlock{Text: "done"}),
		encodedMessage(t, anthropic.RoleUser, anthropic.TextBlock{Text: "create and show"}),
		encodedMessage(t, anthropic.RoleAssistant, anthropic.ToolUseBlock{ID: "one", Name: "bash", Input: json.RawMessage(`{"command":"echo world > b.txt"}`)}),
		encodedMessage(t, anthropic.RoleUser, anthropic.ToolResultBlock{ToolUseID: "one", Content: ""}),
		encodedMessage(t, anthropic.RoleAssistant, anthropic.ToolUseBlock{ID: "two", Name: "bash", Input: json.RawMessage(`{"command":"cat a.txt"}`)}, anthropic.ToolUseBlock{ID: "three", Name: "read_file", Input: json.RawMessage(`{"path":"b.txt"}`)}),
		encodedMessage(t, anthropic.RoleUser, anthropic.ToolResultBlock{ToolUseID: "two", Content: "hello"}, anthropic.ToolResultBlock{ToolUseID: "three", Content: "world", IsError: true}),
		encodedMessage(t, anthropic.RoleAssistant, anthropic.TextBlock{Text: "Created b.txt"}),
	}
	theme := presentation.NewTheme(presentation.TrueColor, presentation.DarkBackground, presentation.AccentPlum)
	documents, err := latestToolDocuments(messages, theme)
	if err != nil {
		t.Fatal(err)
	}
	titles := make([]string, len(documents))
	for index, document := range documents {
		titles[index] = document.Title
	}
	want := []string{"Run echo world > b.txt", "Run cat a.txt", "Read b.txt"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Fatalf("documents = %q, want %q", titles, want)
	}
	// Each tab opens with the drop of its step; the failed read is red.
	if documents[0].Mark != theme.StepMark(presentation.StepRun) || documents[2].Mark != theme.StepMark(presentation.StepFailed) {
		t.Fatalf("drops = %q, %q", documents[0].Mark, documents[2].Mark)
	}
	if !strings.HasSuffix(documents[2].Text, "\nResult\nworld") {
		t.Fatalf("third document = %q", documents[2].Text)
	}
	if documents, err := latestToolDocuments(messages[:5], theme); err != nil || len(documents) != 0 {
		t.Fatalf("a turn without tools gave %d documents, %v", len(documents), err)
	}
}
