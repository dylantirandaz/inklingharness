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
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/session"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func readCall(id, name, input string) anthropic.ToolUseBlock {
	return anthropic.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)}
}

func TestRepeatedReadsNoteOnlyAnUnchangedRepeat(t *testing.T) {
	content := strings.Repeat("def add(a, b):\n    return a + b\n", 20)
	reads := newRepeatedReads()
	first := reads.result(readCall("toolu_1", "read_file", `{"path":"a.py"}`), tools.Result{Content: content})
	if first.Content != content {
		t.Fatal("the first read changed")
	}
	// Space in the input does not make a different call.
	second := reads.result(readCall("toolu_2", "read_file", `{ "path": "a.py" }`), tools.Result{Content: content})
	if !strings.Contains(second.Content, "toolu_1") || second.IsError || len(second.Content) >= len(content) {
		t.Fatalf("an unchanged repeat did not get a note naming the earlier call: %q", second.Content)
	}
	// A model that asks a third time gets the full result again.
	if third := reads.result(readCall("toolu_3", "read_file", `{"path":"a.py"}`), tools.Result{Content: content}); third.Content != content {
		t.Fatalf("the read after a note was not full: %q", third.Content)
	}
	if fourth := reads.result(readCall("toolu_4", "read_file", `{"path":"a.py"}`), tools.Result{Content: content}); !strings.Contains(fourth.Content, "toolu_3") {
		t.Fatalf("the note does not name the newest full result: %q", fourth.Content)
	}
	changed := content + "# changed\n"
	if fifth := reads.result(readCall("toolu_5", "read_file", `{"path":"a.py"}`), tools.Result{Content: changed}); fifth.Content != changed {
		t.Fatal("a changed file did not come back in full")
	}
	if sixth := reads.result(readCall("toolu_6", "read_file", `{"path":"a.py"}`), tools.Result{Content: changed}); !strings.Contains(sixth.Content, "toolu_5") {
		t.Fatalf("a repeat of the changed file does not name its newest read: %q", sixth.Content)
	}
}

func TestRepeatedReadsBoundaries(t *testing.T) {
	long := strings.Repeat("x", 1000)
	for _, test := range []struct {
		name          string
		first, second anthropic.ToolUseBlock
		result        tools.Result
		noted         bool
	}{
		{"read_files", readCall("a", "read_files", `{"files":[{"path":"a.py"},{"path":"b.py"}]}`), readCall("b", "read_files", `{"files":[{"path":"a.py"},{"path":"b.py"}]}`), tools.Result{Content: long}, true},
		{"other range", readCall("a", "read_file", `{"path":"a.py"}`), readCall("b", "read_file", `{"path":"a.py","offset":2}`), tools.Result{Content: long}, false},
		{"other tool", readCall("a", "bash", `{"command":"cat a.py"}`), readCall("b", "bash", `{"command":"cat a.py"}`), tools.Result{Content: long}, false},
		{"failed read", readCall("a", "read_file", `{"path":"a.py"}`), readCall("b", "read_file", `{"path":"a.py"}`), tools.Result{Content: long, IsError: true}, false},
		{"result shorter than the note", readCall("a", "read_file", `{"path":"a.py"}`), readCall("b", "read_file", `{"path":"a.py"}`), tools.Result{Content: "x = 1\n[end of file: 1 lines]"}, false},
		{"invalid input", readCall("a", "read_file", `{"path":`), readCall("b", "read_file", `{"path":`), tools.Result{Content: long}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := newRepeatedReads()
			reads.result(test.first, test.result)
			got := reads.result(test.second, test.result)
			if noted := got != test.result; noted != test.noted {
				t.Fatalf("noted = %t, want %t: %q", noted, test.noted, got.Content)
			}
		})
	}
	reads := newRepeatedReads()
	reads.result(readCall("a", "read_file", `{"path":"a.py"}`), tools.Result{Content: long})
	reads.forget()
	if got := reads.result(readCall("b", "read_file", `{"path":"a.py"}`), tools.Result{Content: long}); got.Content != long {
		t.Fatal("a result after forget was not full")
	}
	var none *repeatedReads
	none.forget()
	if got := none.result(readCall("b", "read_file", `{"path":"a.py"}`), tools.Result{Content: long}); got.Content != long {
		t.Fatal("the default profile changed a result")
	}
}

// requestBody is the part of a request that the provider caches as a prefix.
type requestBody struct {
	System   json.RawMessage   `json:"system"`
	Tools    json.RawMessage   `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

// runReadTwice runs a conversation that reads a.txt twice, changes it, and
// reads it again. It returns each request.
func runReadTwice(t *testing.T, lean bool) []requestBody {
	t.Helper()
	root := t.TempDir()
	original := strings.Repeat("unchanged line\n", 40)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	var requests []requestBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, body)
		reply := map[int]string{
			1: toolUseBlock(1, "toolu_a", "read_file", `{"path":"a.txt"}`),
			2: toolUseBlock(1, "toolu_b", "read_file", `{"path":"a.txt"}`),
			3: toolUseBlock(1, "toolu_c", "write_file", `{"path":"a.txt","content":"changed\n"}`),
			4: toolUseBlock(1, "toolu_d", "read_file", `{"path":"a.txt"}`),
		}
		if block, found := reply[len(requests)]; found {
			_, _ = fmt.Fprint(w, messageStart(), thinkingBlock(0, fmt.Sprintf("sig-%d", len(requests))), block, messageEnd("tool_use", 3))
			return
		}
		_, _ = fmt.Fprint(w, messageStart(), textBlock(0, "done"), messageEnd("end_turn", 1))
	}))
	defer server.Close()
	output := t.TempDir()
	jobs := tools.NewJobs(output)
	t.Cleanup(func() {
		if err := jobs.Close(); err != nil {
			t.Error(err)
		}
	})
	newSet, system := tools.Standard, CodingInstructions(false)
	if lean {
		newSet, system = tools.Lean, LeanInstructions(false)
	}
	toolSet, err := newSet(root, output, jobs)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Model: "m", MaxTokens: 50, MaxTurns: 6, System: system, Lean: lean, ContextStore: session.NewStore(t.TempDir()),
		Approve: func(context.Context, anthropic.ToolUseBlock) (bool, error) { return true, nil },
	}
	if _, err := Run(context.Background(), anthropic.NewClient("key", server.URL, nil), config, toolSet, nil, Prompt{Text: "read a.txt"}, SilentObserver{}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 5 {
		t.Fatalf("requests = %d, want 5", len(requests))
	}
	return requests
}

func lastResult(t *testing.T, request requestBody) string {
	t.Helper()
	var message struct {
		Content []struct {
			Content string `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(request.Messages[len(request.Messages)-1], &message); err != nil || len(message.Content) != 1 {
		t.Fatalf("last message is not one tool result: %s, %v", request.Messages[len(request.Messages)-1], err)
	}
	return message.Content[0].Content
}

// The lean profile replaces an unchanged repeat with a note, and a read
// after a change comes back in full. Each request of both profiles repeats
// the system prompt, the tools, and the earlier messages byte for byte, so
// the provider can reuse its prompt cache.
func TestLeanNotesRepeatedReadAndKeepsCachedPrefix(t *testing.T) {
	for _, lean := range []bool{false, true} {
		t.Run(fmt.Sprintf("lean=%t", lean), func(t *testing.T) {
			requests := runReadTwice(t, lean)
			first, repeat, changed := lastResult(t, requests[1]), lastResult(t, requests[2]), lastResult(t, requests[4])
			if !strings.Contains(first, "unchanged line") || !strings.HasPrefix(changed, "changed\n") {
				t.Fatalf("full reads are wrong: %q, %q", first, changed)
			}
			if noted := strings.Contains(repeat, "toolu_a") && !strings.Contains(repeat, "unchanged line"); noted != lean {
				t.Fatalf("repeat noted = %t in lean=%t: %q", noted, lean, repeat)
			}
			for turn := 1; turn < len(requests); turn++ {
				previous, current := requests[turn-1], requests[turn]
				if !bytes.Equal(previous.System, current.System) || !bytes.Equal(previous.Tools, current.Tools) {
					t.Fatalf("request %d changed the system prompt or the tools", turn+1)
				}
				if len(current.Messages) != len(previous.Messages)+2 {
					t.Fatalf("request %d has %d messages after %d", turn+1, len(current.Messages), len(previous.Messages))
				}
				for index, message := range previous.Messages {
					if !bytes.Equal(current.Messages[index], message) {
						t.Fatalf("request %d changed message %d:\n%s\n%s", turn+1, index, message, current.Messages[index])
					}
				}
			}
		})
	}
}
