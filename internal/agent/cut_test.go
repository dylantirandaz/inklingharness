package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

// A reply cut at max_tokens is dropped and retried with a note, at most
// maxCutReplies times for one prompt; then the run fails as before.
func TestRunRetriesRepliesCutAtTheLimit(t *testing.T) {
	for _, test := range []struct {
		name     string
		cuts     int
		wantErr  bool
		wantText string
	}{
		{"one cut, then an answer", 1, false, "done"},
		{"every reply cut", maxCutReplies + 1, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests [][]wireMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []wireMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				requests = append(requests, body.Messages)
				w.Header().Set("content-type", "text/event-stream")
				if len(requests) <= test.cuts {
					// A tool call that the limit cut: its input never ends.
					_, _ = fmt.Fprint(w, messageStart(), thinkingBlock(0, "sig"), toolUseBlock(1, "toolu_cut", "read_file", `{"path":"a.txt"}`), messageEnd("max_tokens", 50))
					return
				}
				_, _ = fmt.Fprint(w, messageStart(), textBlock(0, "done"), messageEnd("end_turn", 5))
			}))
			defer server.Close()
			toolSet, err := standardTools(t, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			client := anthropic.NewClient("key", server.URL, nil)
			outcome, err := Run(context.Background(), client, Config{Model: "m", MaxTokens: 50, MaxTurns: 10}, toolSet, nil, Prompt{Text: "fix it"}, recordingObserver{t: t})
			if (err != nil) != test.wantErr || outcome.FinalText != test.wantText {
				t.Fatalf("Run = %q, %v", outcome.FinalText, err)
			}
			if test.wantErr && len(requests) != maxCutReplies+1 {
				t.Fatalf("requests = %d, want %d", len(requests), maxCutReplies+1)
			}
			// The retry holds no part of the cut reply: one user message with
			// the prompt and then the note.
			retry := requests[1]
			if len(retry) != 1 || retry[0].Role != "user" || len(retry[0].Content) != 2 || !strings.Contains(string(retry[0].Content[1]), "hit the output limit") {
				t.Fatalf("retry messages = %s", fmt.Sprint(retry))
			}
		})
	}
}
