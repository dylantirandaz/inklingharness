package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func decodeJSON(t *testing.T, text string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return value
}

func TestJSONObserverEvents(t *testing.T) {
	usage := anthropic.Usage{InputTokens: 10, CacheCreationInputTokens: 3, CacheReadInputTokens: 5, OutputTokens: 42}
	tests := []struct {
		name string
		emit func(*jsonObserver)
		want string
	}{
		{"text", func(o *jsonObserver) { o.Text("hel\"lo\n") }, `{"type":"text","delta":"hel\"lo\n"}`},
		{"thinking", func(o *jsonObserver) { o.Thinking("hmm") }, `{"type":"thinking","delta":"hmm"}`},
		{"tool call", func(o *jsonObserver) { o.ToolCall("bash", json.RawMessage(`{"command":"ls -la"}`)) },
			`{"type":"tool_call","name":"bash","input":{"command":"ls -la"}}`},
		{"tool result", func(o *jsonObserver) {
			o.ToolResult("read", tools.Result{Content: "no such file", IsError: true}, 1500*time.Microsecond)
		}, `{"type":"tool_result","name":"read","content":"no such file","is_error":true,"elapsed_ms":1}`},
		{"turn", func(o *jsonObserver) { o.TurnDone(2, usage, anthropic.StopEndTurn, 2*time.Second) },
			`{"type":"turn","turn":2,"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":42,"cache_read_input_tokens":5,"cache_creation_input_tokens":3},"elapsed_ms":2000}`},
		{"status", func(o *jsonObserver) { o.Status("compacting") }, `{"type":"status","message":"compacting"}`},
		{"unknown event", func(o *jsonObserver) { o.UnknownEvent("ping_v2") }, `{"type":"unknown_event","event":"ping_v2"}`},
		{"done", func(o *jsonObserver) { o.writeDone(&agent.Outcome{FinalText: "all good", Turns: 3, Usage: usage}) },
			`{"type":"done","final_text":"all good","turns":3,"usage":{"input_tokens":10,"output_tokens":42,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}`},
		{"error", func(o *jsonObserver) { o.writeError(errors.New("rate limited")) }, `{"type":"error","message":"rate limited"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			observer := newJSONObserver(&output)
			test.emit(observer)
			if err := observer.err(); err != nil {
				t.Fatalf("err() = %v", err)
			}
			text := output.String()
			if strings.Count(text, "\n") != 1 || !strings.HasSuffix(text, "\n") {
				t.Fatalf("output %q is not exactly one line", text)
			}
			if got, want := decodeJSON(t, text), decodeJSON(t, test.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("event = %v, want %v", got, want)
			}
		})
	}
}

func TestJSONObserverToolCallStartWritesNothing(t *testing.T) {
	var output bytes.Buffer
	newJSONObserver(&output).ToolCallStart("bash")
	if output.Len() != 0 {
		t.Fatalf("output = %q, want nothing", output.String())
	}
}

func TestJSONObserverConcurrentToolResults(t *testing.T) {
	var output bytes.Buffer
	observer := newJSONObserver(&output)
	const writers, perWriter = 8, 50
	var group sync.WaitGroup
	for writer := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range perWriter {
				observer.ToolResult("read", tools.Result{Content: strings.Repeat(strconv.Itoa(writer), 100+index)}, time.Millisecond)
			}
		}()
	}
	group.Wait()
	scanner := bufio.NewScanner(&output)
	scanner.Buffer(nil, 1<<20)
	count := 0
	for scanner.Scan() {
		var event toolResultEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("line %d is not one JSON event: %v", count, err)
		}
		if event.Type != "tool_result" || event.Name != "read" {
			t.Fatalf("line %d = %+v", count, event)
		}
		count++
	}
	if count != writers*perWriter {
		t.Fatalf("got %d lines, want %d", count, writers*perWriter)
	}
}

type failingWriter struct{ calls int }

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, io.ErrClosedPipe
}

func TestJSONObserverKeepsFirstWriteError(t *testing.T) {
	writer := &failingWriter{}
	observer := newJSONObserver(writer)
	observer.Text("a")
	observer.Status("b")
	observer.writeError(errors.New("c"))
	if err := observer.err(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("err() = %v, want %v", err, io.ErrClosedPipe)
	}
	if writer.calls != 1 {
		t.Fatalf("writer called %d times, want 1: a broken stream gets no more lines", writer.calls)
	}
}

// scriptedRunner acts like one agent turn: it streams text, asks to run one
// command, and reports the answer as the tool result.
func scriptedRunner(ctx context.Context, prompt string, observer agent.Observer, approve func(context.Context, anthropic.ToolUseBlock) (bool, error)) (*agent.Outcome, error) {
	observer.Text("echo: " + prompt)
	allowed, err := approve(ctx, anthropic.ToolUseBlock{ID: "call-1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)})
	if err != nil {
		return &agent.Outcome{Turns: 1}, err
	}
	observer.ToolResult("bash", tools.Result{Content: strconv.FormatBool(allowed), IsError: !allowed}, 3*time.Millisecond)
	usage := anthropic.Usage{InputTokens: 10, OutputTokens: 4}
	observer.TurnDone(1, usage, anthropic.StopEndTurn, 7*time.Millisecond)
	return &agent.Outcome{FinalText: "done: " + prompt, Turns: 1, Usage: usage}, nil
}

type rpcClient struct {
	t        *testing.T
	input    *io.PipeWriter
	messages chan string
	served   chan error
}

func startRPC(t *testing.T, ctx context.Context, run rpcRunner) *rpcClient {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	client := &rpcClient{t: t, input: inputWriter, messages: make(chan string, 64), served: make(chan error, 1)}
	go func() {
		err := serveRPC(ctx, inputReader, outputWriter, run)
		outputWriter.Close()
		client.served <- err
	}()
	go func() {
		defer close(client.messages)
		scanner := bufio.NewScanner(outputReader)
		for scanner.Scan() {
			client.messages <- scanner.Text()
		}
	}()
	t.Cleanup(func() {
		inputWriter.Close()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case _, open := <-client.messages:
				if !open {
					return
				}
			case <-timeout:
				t.Error("server output did not end after stdin closed")
				return
			}
		}
	})
	return client
}

func (c *rpcClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.input, line+"\n"); err != nil {
		c.t.Fatalf("send %q: %v", line, err)
	}
}

func (c *rpcClient) next() string {
	c.t.Helper()
	select {
	case line, open := <-c.messages:
		if !open {
			c.t.Fatal("server output ended")
		}
		return line
	case <-time.After(5 * time.Second):
		c.t.Fatal("no server message after 5s")
	}
	return ""
}

// expect compares the next server line with want as decoded JSON.
func (c *rpcClient) expect(want string) {
	c.t.Helper()
	line := c.next()
	if got, wanted := decodeJSON(c.t, line), decodeJSON(c.t, want); !reflect.DeepEqual(got, wanted) {
		c.t.Fatalf("server sent %s\nwant %s", line, want)
	}
}

// expectError checks an error response with the given raw id and a message
// that contains part.
func (c *rpcClient) expectError(id, part string) {
	c.t.Helper()
	line := c.next()
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		c.t.Fatalf("decode %q: %v", line, err)
	}
	if string(response.ID) != id || response.Error == nil || !strings.Contains(response.Error.Message, part) {
		c.t.Fatalf("server sent %s, want error with id %s and message containing %q", line, id, part)
	}
}

func (c *rpcClient) waitServed() error {
	c.t.Helper()
	select {
	case err := <-c.served:
		return err
	case <-time.After(5 * time.Second):
		c.t.Fatal("serveRPC did not return after 5s")
	}
	return nil
}

const approveRequest = `{"id":"approve-%d","method":"approve","params":{"tool":"bash","input":{"command":"ls"}}}`

func approveLine(number int) string {
	return strings.Replace(approveRequest, "%d", strconv.Itoa(number), 1)
}

func TestRPCPromptApprovalRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		allow   bool
		isError bool
	}{
		{"allow", true, false},
		{"deny", false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := startRPC(t, context.Background(), scriptedRunner)
			client.send(`{"id":1,"method":"prompt","params":{"text":"hi"}}`)
			client.expect(`{"method":"event","params":{"type":"text","delta":"echo: hi"}}`)
			client.expect(approveLine(1))
			client.send(`{"id":"approve-1","result":{"allow":` + strconv.FormatBool(test.allow) + `}}`)
			client.expect(`{"method":"event","params":{"type":"tool_result","name":"bash","content":"` + strconv.FormatBool(test.allow) +
				`","is_error":` + strconv.FormatBool(test.isError) + `,"elapsed_ms":3}}`)
			client.expect(`{"method":"event","params":{"type":"turn","turn":1,"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"elapsed_ms":7}}`)
			client.expect(`{"id":1,"result":{"final_text":"done: hi","turns":1,"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)

			// A second prompt runs after the first one ends, with a new approval id.
			client.send(`{"id":2,"method":"prompt","params":{"text":"again"}}`)
			client.expect(`{"method":"event","params":{"type":"text","delta":"echo: again"}}`)
			client.expect(approveLine(2))
			client.send(`{"id":"approve-1","result":{"allow":true}}`)
			client.expectError(`"approve-1"`, "no approval is pending")
			client.send(`{"id":3,"method":"shutdown"}`)
			client.expectError("2", "context canceled")
			client.expect(`{"id":3,"result":{}}`)
			if err := client.waitServed(); err != nil {
				t.Fatalf("serveRPC = %v, want nil", err)
			}
		})
	}
}

func TestRPCBusyWhilePromptRuns(t *testing.T) {
	client := startRPC(t, context.Background(), scriptedRunner)
	client.send(`{"id":1,"method":"prompt","params":{"text":"first"}}`)
	client.expect(`{"method":"event","params":{"type":"text","delta":"echo: first"}}`)
	client.expect(approveLine(1))
	client.send(`{"id":2,"method":"prompt","params":{"text":"second"}}`)
	client.expect(`{"id":2,"error":{"message":"busy"}}`)
	client.send(`{"id":"approve-1","result":{"allow":true}}`)
	client.next() // tool_result
	client.next() // turn
	client.expect(`{"id":1,"result":{"final_text":"done: first","turns":1,"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)
}

func TestRPCCancelStopsPendingApproval(t *testing.T) {
	client := startRPC(t, context.Background(), scriptedRunner)
	client.send(`{"id":1,"method":"prompt","params":{"text":"hi"}}`)
	client.next() // text
	client.expect(approveLine(1))
	client.send(`{"id":2,"method":"cancel"}`)
	client.expect(`{"id":2,"result":{}}`)
	client.expect(`{"id":1,"error":{"message":"context canceled"}}`)
	// Cancel without a running prompt changes nothing and still succeeds.
	client.send(`{"id":3,"method":"cancel"}`)
	client.expect(`{"id":3,"result":{}}`)
	// The server takes a new prompt after a cancel.
	client.send(`{"id":4,"method":"prompt","params":{"text":"next"}}`)
	client.expect(`{"method":"event","params":{"type":"text","delta":"echo: next"}}`)
}

func TestRPCShutdownWhileIdle(t *testing.T) {
	client := startRPC(t, context.Background(), scriptedRunner)
	client.send(`{"id":"s","method":"shutdown"}`)
	client.expect(`{"id":"s","result":{}}`)
	if err := client.waitServed(); err != nil {
		t.Fatalf("serveRPC = %v, want nil", err)
	}
}

func TestRPCRejectsBadLinesAndContinues(t *testing.T) {
	client := startRPC(t, context.Background(), scriptedRunner)
	tests := []struct {
		line   string
		id     string
		reason string
	}{
		{`not json`, "null", "parse message"},
		{`{"id":1,"method":"frobnicate"}`, "1", `unknown method "frobnicate"`},
		{`{"method":"prompt","params":{"text":"hi"}}`, "null", "needs an id"},
		{`{"id":2,"method":"prompt"}`, "2", "needs params.text"},
		{`{"id":3,"method":"prompt","params":{"text":""}}`, "3", "needs params.text"},
		{`{"id":4,"method":"prompt","params":[1]}`, "4", "parse prompt params"},
		{`{"id":5}`, "null", "needs a method or an approval id"},
		{`{"id":"approve-9","result":{}}`, `"approve-9"`, "needs result.allow"},
		{`{"id":"approve-9","result":{"allow":true}}`, `"approve-9"`, "no approval is pending"},
	}
	for _, test := range tests {
		client.send(test.line)
		client.expectError(test.id, test.reason)
	}
	// Blank lines get no answer; the next request proves the server still works.
	client.send("")
	client.send(`{"id":6,"method":"prompt","params":{"text":"ok"}}`)
	client.expect(`{"method":"event","params":{"type":"text","delta":"echo: ok"}}`)
}

func TestRPCEndOfInputDeniesPendingApproval(t *testing.T) {
	client := startRPC(t, context.Background(), scriptedRunner)
	client.send(`{"id":1,"method":"prompt","params":{"text":"hi"}}`)
	client.next() // text
	client.expect(approveLine(1))
	client.input.Close()
	client.expect(`{"id":1,"error":{"message":"unexpected EOF"}}`)
	if err := client.waitServed(); err != nil {
		t.Fatalf("serveRPC = %v, want nil", err)
	}
}

func TestRPCContextCancelEndsServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := startRPC(t, ctx, scriptedRunner)
	client.send(`{"id":1,"method":"prompt","params":{"text":"hi"}}`)
	client.next() // text
	client.expect(approveLine(1))
	cancel()
	client.expect(`{"id":1,"error":{"message":"context canceled"}}`)
	if err := client.waitServed(); !errors.Is(err, context.Canceled) {
		t.Fatalf("serveRPC = %v, want %v", err, context.Canceled)
	}
}

func TestRPCLineLimit(t *testing.T) {
	client := startRPC(t, context.Background(), scriptedRunner)
	// The writer blocks when the server stops reading, so it runs apart.
	go func() {
		_, _ = io.WriteString(client.input, `{"id":1,"method":"prompt","params":{"text":"`+strings.Repeat("x", maxRPCLine)+`"}}`+"\n")
	}()
	client.expectError("null", "token too long")
	if err := client.waitServed(); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("serveRPC = %v, want %v", err, bufio.ErrTooLong)
	}
}
