package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func TestCheckpointKeepsToolPairAfterPause(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			fmt.Fprint(w, messageStart(), textBlock(0, "The earlier step is complete."), messageEnd("pause_turn", 3))
		case 2:
			fmt.Fprint(w, messageStart(), toolUseBlock(0, "save", "context_checkpoint", `{"summary":"The earlier step is complete.","next_step":"Reply done. Do not change files."}`), messageEnd("tool_use", 8))
		case 3:
			fmt.Fprint(w, messageStart(), textBlock(0, "done"), messageEnd("end_turn", 1))
		default:
			t.Error("unexpected request after completion")
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	toolSet, err := tools.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	history := encodeHistory(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("Earlier exact source. ", 400)}}})
	original := history[0]
	root := t.TempDir()
	store := session.NewStore(root)
	config := Config{Model: "m", MaxTokens: 100, MaxTurns: 3, ContextStore: store}
	outcome, err := Run(context.Background(), anthropic.NewClient("key", server.URL, nil), config, toolSet, history, Prompt{Text: "Save the completed step, then reply done."}, SilentObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if !history[0].Equal(original) || outcome.FinalText != "done" || EstimateTokens(outcome.Messages) >= EstimateTokens(history) {
		t.Fatal("checkpoint changed the caller, lost completion, or failed to reduce history")
	}
	foundCall, foundResult := false, false
	for _, encoded := range outcome.Messages {
		for _, block := range decodeMessage(t, encoded).Content {
			switch block := block.(type) {
			case anthropic.ToolUseBlock:
				if block.ID == "save" {
					foundCall = true
				}
			case anthropic.ToolResultBlock:
				if block.ToolUseID == "save" {
					if !foundCall || block.IsError {
						t.Fatal("checkpoint left an unpaired or failed tool result after a pause")
					}
					foundResult = true
				}
			}
		}
	}
	if !foundCall || !foundResult {
		t.Fatal("checkpoint lost its completed tool pair")
	}
	archives, err := os.ReadDir(filepath.Join(root, "resources", "context"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("archive: %v, %v", archives, err)
	}
	page, err := store.ReadContext(context.Background(), archives[0].Name(), 0)
	if err != nil || !strings.Contains(page.Text, "Earlier exact source.") || !strings.Contains(page.Text, "The earlier step is complete.") {
		t.Fatalf("checkpoint lost the earlier source or pause: %v", err)
	}
}

func TestCheckpointHookFailureKeepsEarlierHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, messageStart(), toolUseBlock(0, "save", "context_checkpoint", `{"summary":"The earlier step is complete.","next_step":"Reply done."}`), messageEnd("tool_use", 8))
	}))
	defer server.Close()
	toolSet, err := tools.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	history := encodeHistory(t,
		anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: strings.Repeat("Keep this source. ", 300)}}},
		anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "Earlier step complete."}}},
	)
	hookError := errors.New("hook stopped the turn")
	config := Config{Model: "m", MaxTokens: 100, MaxTurns: 2, ContextStore: session.NewStore(t.TempDir()), AfterTool: func(context.Context, anthropic.ToolUseBlock, tools.Result) (string, error) {
		return "", hookError
	}}
	outcome, err := Run(context.Background(), anthropic.NewClient("key", server.URL, nil), config, toolSet, history, Prompt{Text: "Save context."}, SilentObserver{})
	if !errors.Is(err, hookError) {
		t.Fatalf("lost hook failure: %v", err)
	}
	if !outcome.Messages[0].Equal(history[0]) || !outcome.Messages[1].Equal(history[1]) {
		t.Fatal("failed checkpoint replaced earlier history")
	}
	calls := toolCalls(decodeMessage(t, outcome.Messages[len(outcome.Messages)-2]))
	results := decodeMessage(t, outcome.Messages[len(outcome.Messages)-1]).Content
	if len(calls) != 1 || len(results) != 1 {
		t.Fatal("failed checkpoint lost its tool pair")
	}
	result, ok := results[0].(anthropic.ToolResultBlock)
	if !ok || result.ToolUseID != calls[0].ID || !result.IsError {
		t.Fatal("failed checkpoint has no matching error result")
	}
}

func TestOlderInlineImagesBecomeDurableReferences(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	image := anthropic.ImageBlock{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(picture.Bytes())}
	encoded := encodeHistory(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "Keep the original image."}, image}})[0]
	var loaded anthropic.EncodedMessage
	if err := json.Unmarshal(encoded.Wire(), &loaded); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := session.NewStore(root)
	for _, original := range []anthropic.EncodedMessage{encoded, loaded} {
		history := []anthropic.EncodedMessage{original}
		result, err := externalizeImages(context.Background(), store, history)
		if err != nil {
			t.Fatal(err)
		}
		if !history[0].Equal(original) || !history[0].HasImages() || len(result) != 1 || result[0].HasImages() || bytes.Contains(result[0].Wire(), []byte(image.Data)) {
			t.Fatal("image migration changed the source or kept inline image bytes")
		}
		message := decodeMessage(t, result[0])
		if message.Role != anthropic.RoleUser || message.Content[0] != (anthropic.TextBlock{Text: "Keep the original image."}) {
			t.Fatal("image migration changed the request")
		}
	}
	stored, err := os.ReadDir(filepath.Join(root, "resources", "images"))
	if err != nil || len(stored) != 1 {
		t.Fatalf("images were not stored once: %v, %v", stored, err)
	}
	restored, err := store.LoadImage(context.Background(), stored[0].Name())
	if err != nil || restored != image {
		t.Fatalf("stored image changed: %v", err)
	}
	withoutStore, err := externalizeImages(context.Background(), nil, []anthropic.EncodedMessage{loaded})
	if err == nil || withoutStore != nil || !loaded.Equal(encoded) {
		t.Fatal("missing storage allowed a partial migration")
	}
}
