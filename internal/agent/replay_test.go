package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// TestReplayRepeatedReads measures the lean note for repeated reads on
// recorded trials, without a model call. Set THINK_REPLAY_EVENTS to a glob
// of think-events.jsonl files, for example
// out/terminal-bench/jobs/swe-bench-more-*/*/agent/think-events.jsonl. Only
// trials whose result.json names the agent "think" count. The test applies
// repeatedReads to each recorded tool result in order and logs how many
// results change and how many characters they lose. A character saved in
// turn t is also saved in each later turn, so it also logs the share weighted
// by the remaining turns of the trial.
func TestReplayRepeatedReads(t *testing.T) {
	pattern := os.Getenv("THINK_REPLAY_EVENTS")
	if pattern == "" {
		t.Skip("set THINK_REPLAY_EVENTS to a glob of think-events.jsonl files")
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	var trials, results, changed int
	var total, saved, weightedTotal, weightedSaved int64
	for _, path := range paths {
		var trial struct {
			AgentInfo struct {
				Name string `json:"name"`
			} `json:"agent_info"`
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(path)), "result.json"))
		if err != nil || json.Unmarshal(raw, &trial) != nil || trial.AgentInfo.Name != "think" {
			continue
		}
		trials++
		type recorded struct {
			call   toolCall
			result tools.Result
			turn   int
		}
		var events []recorded
		var pending []toolCall
		turns := 0
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(nil, 64<<20)
		for scanner.Scan() {
			var event struct {
				Type    string          `json:"type"`
				Name    string          `json:"name"`
				Input   json.RawMessage `json:"input"`
				Content string          `json:"content"`
				IsError bool            `json:"is_error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			switch event.Type {
			case "tool_call":
				pending = append(pending, toolCall{name: event.Name, input: event.Input})
			case "turn":
				turns++
			case "tool_result":
				if len(pending) == 0 {
					t.Fatalf("%s: a result without a call", path)
				}
				events = append(events, recorded{call: pending[0], result: tools.Result{Content: event.Content, IsError: event.IsError}, turn: turns})
				pending = pending[1:]
			}
		}
		if err := errors.Join(scanner.Err(), file.Close()); err != nil {
			t.Fatal(err)
		}
		reads := newRepeatedReads()
		for index, event := range events {
			call := readCall(fmt.Sprintf("toolu_replay_%d", index), event.call.name, string(event.call.input))
			after := reads.result(call, event.result)
			remaining := int64(turns - event.turn + 1)
			results++
			total += int64(len(event.result.Content))
			weightedTotal += int64(len(event.result.Content)) * remaining
			if after != event.result {
				changed++
				saved += int64(len(event.result.Content) - len(after.Content))
				weightedSaved += int64(len(event.result.Content)-len(after.Content)) * remaining
			}
		}
	}
	if total == 0 {
		t.Fatalf("no tool results of think trials match %q", pattern)
	}
	t.Logf("trials %d, tool results %d, result characters %d", trials, results, total)
	t.Logf("| transformation | results changed | characters saved | share of tool-result characters | share weighted by later turns |")
	t.Logf("| repeated read note | %d | %d | %.2f%% | %.2f%% |", changed, saved, 100*float64(saved)/float64(total), 100*float64(weightedSaved)/float64(weightedTotal))
}

type toolCall struct {
	name  string
	input json.RawMessage
}
