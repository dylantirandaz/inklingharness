package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func frames(events ...string) string {
	var out strings.Builder
	for _, event := range events {
		out.WriteString("data: " + event + "\n\n")
	}
	return out.String()
}

func start(index int, block string) string {
	return `{"type":"content_block_start","index":` + string(rune('0'+index)) + `,"content_block":` + block + `}`
}

func delta(index int, body string) string {
	return `{"type":"content_block_delta","index":` + string(rune('0'+index)) + `,"delta":` + body + `}`
}

const stop = `{"type":"content_block_stop","index":0}`

// finish ends a reply with a stop reason, as the server does.
func finish(reason string) []string {
	return []string{
		`{"type":"message_delta","delta":{"stop_reason":"` + reason + `"},"usage":{"input_tokens":5,"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	}
}

func sameJSON(t *testing.T, got any, want string) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var left, right any
	if err := json.Unmarshal(encoded, &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &right); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("got %s, want %s", encoded, want)
	}
}

type testAgent struct {
	*agent
	events *bytes.Buffer
	delays []time.Duration
}

func newTestAgent(t *testing.T, handler http.HandlerFunc) *testAgent {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	test := &testAgent{events: new(bytes.Buffer)}
	test.agent = &agent{
		url:       server.URL,
		key:       "key",
		model:     "model-under-test",
		maxTokens: 777,
		maxTurns:  50,
		client:    server.Client(),
		events:    test.events,
		sleep:     func(delay time.Duration) { test.delays = append(test.delays, delay) },
	}
	return test
}

func streamReply(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, frames(events...))
}

func decodeRequest(t *testing.T, r *http.Request) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// The loop sends the task as the only message with one tool and no system
// prompt, runs the tool the model asks for, and returns the reply blocks,
// reasoning included, unchanged next to the result.
func TestLoopRoundTripsReasoningAndToolResults(t *testing.T) {
	requests := 0
	test := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		body := decodeRequest(t, r)
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("request to %s with authorization %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if _, present := body["system"]; present {
			t.Error("the request carries a system prompt")
		}
		sameJSON(t, body["model"], `"model-under-test"`)
		sameJSON(t, body["max_tokens"], `777`)
		sameJSON(t, body["stream"], `true`)
		var tools []struct{ Name string }
		if err := json.Unmarshal(body["tools"], &tools); err != nil || len(tools) != 1 || tools[0].Name != "bash" {
			t.Errorf("tools = %s, %v", body["tools"], err)
		}
		switch requests {
		case 1:
			sameJSON(t, body["messages"], `[{"role":"user","content":[{"type":"text","text":"do the task"}]}]`)
			streamReply(w, append([]string{
				`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
				start(0, `{"type":"thinking","thinking":""}`),
				delta(0, `{"type":"thinking_delta","thinking":"pl"}`),
				delta(0, `{"type":"thinking_delta","thinking":"an"}`),
				delta(0, `{"type":"signature_delta","signature":"sig-1"}`),
				stop,
				start(1, `{"type":"text","text":""}`),
				delta(1, `{"type":"text_delta","text":"Running."}`),
				stop,
				start(2, `{"type":"tool_use","id":"call_1","name":"bash","input":{}}`),
				delta(2, `{"type":"input_json_delta","partial_json":"{\"command\":"}`),
				delta(2, `{"type":"input_json_delta","partial_json":"\"printf hello\"}"}`),
				stop,
			}, finish("tool_use")...)...)
		case 2:
			sameJSON(t, body["messages"], `[
				{"role":"user","content":[{"type":"text","text":"do the task"}]},
				{"role":"assistant","content":[
					{"type":"thinking","thinking":"plan","signature":"sig-1"},
					{"type":"text","text":"Running."},
					{"type":"tool_use","id":"call_1","name":"bash","input":{"command":"printf hello"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"hello"}]}]`)
			streamReply(w, append([]string{start(0, `{"type":"text","text":"Done."}`), stop}, finish("end_turn")...)...)
		default:
			t.Errorf("unexpected request %d", requests)
		}
	})
	if err := test.run(context.Background(), "do the task"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(test.events.String(), `"event":"end","reason":"no tool call","stop_reason":"end_turn","turns":2`) {
		t.Fatalf("%d requests; events:\n%s", requests, test.events)
	}
}

// A run is audited from its events, so each tool event must hold what the
// model ran and what came back.
func TestToolEventRecordsTheCommandAndItsOutput(t *testing.T) {
	requests := 0
	test := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			streamReply(w, append([]string{
				start(0, `{"type":"tool_use","id":"call_1","name":"bash","input":{}}`),
				delta(0, `{"type":"input_json_delta","partial_json":"{\"command\":\"printf audit; exit 3\"}"}`),
				stop,
			}, finish("tool_use")...)...)
			return
		}
		streamReply(w, append([]string{start(0, `{"type":"text","text":"Done."}`), stop}, finish("end_turn")...)...)
	})
	if err := test.run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(test.events.String()), "\n") {
		var event struct {
			Event  string          `json:"event"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
			Error  bool            `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		if event.Event != "tool" {
			continue
		}
		found = true
		sameJSON(t, event.Input, `{"command":"printf audit; exit 3"}`)
		if event.Output != "audit\n[exit code 3]" || !event.Error {
			t.Errorf("output %q, error %t", event.Output, event.Error)
		}
	}
	if !found {
		t.Fatalf("no tool event in:\n%s", test.events)
	}
}

// A reply that stops at the output cap can hold a tool call with cut input.
// The next request must stay valid JSON, and the model hears about the error.
func TestCutToolInputGetsAnErrorResultAndAValidEcho(t *testing.T) {
	requests := 0
	test := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		body := decodeRequest(t, r)
		if requests == 1 {
			streamReply(w, append([]string{
				start(0, `{"type":"tool_use","id":"call_9","name":"bash","input":{}}`),
				delta(0, `{"type":"input_json_delta","partial_json":"{\"command\":\"ec"}`),
				stop,
			}, finish("max_tokens")...)...)
			return
		}
		var messages []struct {
			Role    string
			Content []map[string]any
		}
		if err := json.Unmarshal(body["messages"], &messages); err != nil || len(messages) != 3 {
			t.Fatalf("messages = %s, %v", body["messages"], err)
		}
		sameJSON(t, messages[1].Content[0]["input"], `{}`)
		result := messages[2].Content[0]
		if result["tool_use_id"] != "call_9" || result["is_error"] != true || !strings.Contains(result["content"].(string), "invalid input") {
			t.Errorf("tool result = %v", result)
		}
		streamReply(w, append([]string{start(0, `{"type":"text","text":"ok"}`), stop}, finish("end_turn")...)...)
	})
	if err := test.run(context.Background(), "task"); err != nil || requests != 2 {
		t.Fatalf("run = %v after %d requests", err, requests)
	}
}

func TestBashResults(t *testing.T) {
	tests := []struct {
		name, input string
		wantError   bool
		check       func(t *testing.T, content string)
	}{
		{"output", `{"command":"printf hello"}`, false, func(t *testing.T, content string) {
			if content != "hello" {
				t.Errorf("content = %q", content)
			}
		}},
		{"no output", `{"command":"true"}`, false, func(t *testing.T, content string) {
			if content != "(no output)" {
				t.Errorf("content = %q", content)
			}
		}},
		{"exit code with both streams", `{"command":"echo out; echo err >&2; exit 3"}`, true, func(t *testing.T, content string) {
			for _, want := range []string{"out", "err", "[exit code 3]"} {
				if !strings.Contains(content, want) {
					t.Errorf("content %q lacks %q", content, want)
				}
			}
		}},
		{"large output keeps start and end", `{"command":"printf a%.0s $(seq 1 20000); printf b%.0s $(seq 1 20000); printf c%.0s $(seq 1 20000)"}`, false, func(t *testing.T, content string) {
			if len(content) > outputHeadBytes+outputTailBytes+64 || !strings.HasPrefix(content, "aaaa") || !strings.HasSuffix(content, "cccc") || !strings.Contains(content, "bytes omitted]") {
				t.Errorf("content has %d bytes, starts %q, ends %q", len(content), content[:8], content[len(content)-8:])
			}
		}},
		{"empty command", `{"command":""}`, true, nil},
		{"timeout above the limit", `{"command":"true","timeout_seconds":601}`, true, nil},
		{"wrong argument type", `{"command":"true","timeout_seconds":"5"}`, true, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runBash(context.Background(), json.RawMessage(test.input))
			if result.isError != test.wantError {
				t.Fatalf("isError = %t for %q", result.isError, result.content)
			}
			if test.check != nil {
				test.check(t, result.content)
			}
		})
	}
}

// A command that outlives its timeout dies with its children, and the model
// hears that it timed out.
func TestBashTimeoutKillsTheProcessGroup(t *testing.T) {
	started := time.Now()
	result := runBash(context.Background(), json.RawMessage(`{"command":"sleep 30 & sleep 30","timeout_seconds":1}`))
	if !result.isError || !strings.Contains(result.content, "[timed out after 1s]") {
		t.Fatalf("result = %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the command ran for %s", elapsed)
	}
}

// A busy server gets another try after a delay; a rejected request does not.
func TestRetriesBusyServersButNotRejectedRequests(t *testing.T) {
	requests := 0
	test := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, "overloaded", 529)
			return
		}
		streamReply(w, append([]string{start(0, `{"type":"text","text":"hi"}`), stop}, finish("end_turn")...)...)
	})
	if err := test.run(context.Background(), "task"); err != nil || requests != 2 || !reflect.DeepEqual(test.delays, []time.Duration{time.Second}) {
		t.Fatalf("run = %v after %d requests with delays %v", err, requests, test.delays)
	}

	rejected := 0
	test = newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		rejected++
		http.Error(w, "context length exceeded", http.StatusBadRequest)
	})
	err := test.run(context.Background(), "task")
	if err == nil || rejected != 1 || !strings.Contains(err.Error(), "context length exceeded") {
		t.Fatalf("err = %v after %d requests", err, rejected)
	}
}

func TestStreamWithoutMessageStopFails(t *testing.T) {
	test := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		streamReply(w, start(0, `{"type":"text","text":"cut"}`), delta(0, `{"type":"text_delta","text":" off"}`))
	})
	if err := test.run(context.Background(), "task"); err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("err = %v", err)
	}
}

// A 200 response whose body is an error object must say what it held.
func TestErrorBodyWithStatusOKIsReported(t *testing.T) {
	test := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, ": keep-alive\n\n"+`{"error":{"message":"provider failed"}}`)
	})
	if err := test.run(context.Background(), "task"); err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("err = %v", err)
	}
}

// The reasoning effort travels in output_config, and only when it is set, so a
// run without the flag sends exactly the request of the earlier runs.
func TestEffortIsSentAsOutputConfigOnlyWhenSet(t *testing.T) {
	for _, effort := range []string{"", "high"} {
		var sent map[string]json.RawMessage
		test := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
			sent = decodeRequest(t, r)
			streamReply(w, append([]string{start(0, `{"type":"text","text":"ok"}`), stop}, finish("end_turn")...)...)
		})
		test.effort = effort
		if err := test.run(context.Background(), "task"); err != nil {
			t.Fatal(err)
		}
		output, present := sent["output_config"]
		switch {
		case effort == "" && present:
			t.Errorf("a run without effort sent output_config %s", output)
		case effort != "":
			sameJSON(t, output, `{"effort":"high"}`)
		}
	}
}

func TestRunRefusesAnUnknownEffort(t *testing.T) {
	err := run([]string{"-base-url", "http://127.0.0.1:1", "-model", "m", "-effort", "extreme", "--", "task"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "effort must be one of") {
		t.Fatalf("err = %v", err)
	}
}
