package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeACPBackend acts on the first word of the prompt text:
//
//	hello      thinking, text, one grep call with its result, more text
//	edit N     N edit_file calls; each asks approval
//	wait       one bash call, then waits for cancellation
//	ask        one bash call that asks approval and stops on its error
//	max        a model turn that ends at max_tokens
//	refuse     a model turn that ends with a refusal
//	fail       an error without a model turn
type fakeACPBackend struct {
	mutex     sync.Mutex
	cwds      []string
	prompts   []acpPrompt
	decisions []bool
}

const editInput = `{"path":"a.go","old_string":"x","new_string":"y"}`

func (b *fakeACPBackend) NewSession(_ context.Context, cwd string) (string, error) {
	if cwd == "/fail" {
		return "", errors.New("disk full")
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.cwds = append(b.cwds, cwd)
	return fmt.Sprintf("s%d", len(b.cwds)), nil
}

func (b *fakeACPBackend) Prompt(ctx context.Context, _ string, prompt acpPrompt, observer agent.Observer, approve func(context.Context, anthropic.ToolUseBlock) (bool, error)) (*agent.Outcome, error) {
	b.mutex.Lock()
	b.prompts = append(b.prompts, prompt)
	b.mutex.Unlock()
	outcome := &agent.Outcome{}
	words := strings.Fields(prompt.Text)
	switch words[0] {
	case "hello":
		observer.Thinking("plan")
		observer.Text("Hi")
		observer.ToolCallStart("grep")
		observer.ToolCall("grep", json.RawMessage(`{"pattern":"TODO"}`))
		observer.TurnDone(1, anthropic.Usage{}, anthropic.StopToolUse, 0)
		observer.ToolResult("grep", tools.Result{Content: "a.go:1: TODO"}, time.Millisecond)
		observer.Text(" done")
		observer.TurnDone(2, anthropic.Usage{}, anthropic.StopEndTurn, 0)
		return outcome, nil
	case "edit":
		count, err := strconv.Atoi(words[1])
		if err != nil {
			return outcome, err
		}
		for index := range count {
			observer.ToolCall("edit_file", json.RawMessage(editInput))
			allowed, err := approve(ctx, anthropic.ToolUseBlock{ID: fmt.Sprintf("toolu_%d", index), Name: "edit_file", Input: json.RawMessage(editInput)})
			if err != nil {
				return outcome, err
			}
			b.mutex.Lock()
			b.decisions = append(b.decisions, allowed)
			b.mutex.Unlock()
			if allowed {
				observer.ToolResult("edit_file", tools.Result{Content: "edited"}, time.Millisecond)
			} else {
				observer.ToolResult("edit_file", tools.Result{Content: "denied", IsError: true}, 0)
			}
		}
		return outcome, nil
	case "wait":
		observer.ToolCall("bash", json.RawMessage(`{"command":"sleep 10"}`))
		<-ctx.Done()
		return outcome, ctx.Err()
	case "ask":
		input := json.RawMessage(`{"command":"sleep 10"}`)
		observer.ToolCall("bash", input)
		_, err := approve(ctx, anthropic.ToolUseBlock{ID: "toolu_ask", Name: "bash", Input: input})
		return outcome, err
	case "max":
		observer.TurnDone(1, anthropic.Usage{}, anthropic.StopMaxTokens, 0)
		return outcome, errors.New("turn 1: reply hit max_tokens=1")
	case "refuse":
		observer.TurnDone(1, anthropic.Usage{}, anthropic.StopRefusal, 0)
		return outcome, errors.New("turn 1: model refused the request")
	case "fail":
		return outcome, errors.New("backend broke")
	default:
		return outcome, fmt.Errorf("fake backend: unknown prompt %q", prompt.Text)
	}
}

func (b *fakeACPBackend) recordedDecisions() []bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return append([]bool(nil), b.decisions...)
}

// acpClient drives serveACP through pipes, as an editor does over stdio.
type acpClient struct {
	t      *testing.T
	input  *io.PipeWriter
	output chan string
	served chan error
}

func startACP(t *testing.T, backend acpBackend) *acpClient {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	client := &acpClient{t: t, input: inWriter, output: make(chan string, 1024), served: make(chan error, 1)}
	go func() {
		err := serveACP(context.Background(), inReader, outWriter, backend)
		outWriter.Close()
		client.served <- err
	}()
	go func() {
		scanner := bufio.NewScanner(outReader)
		for scanner.Scan() {
			client.output <- scanner.Text()
		}
		close(client.output)
	}()
	t.Cleanup(func() {
		inWriter.Close()
		select {
		case err := <-client.served:
			if err != nil {
				t.Errorf("serveACP: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serveACP did not return at the end of input")
		}
		for line := range client.output {
			t.Errorf("unexpected line at the end: %s", line)
		}
	})
	return client
}

func (c *acpClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.input, line+"\n"); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *acpClient) next() string {
	c.t.Helper()
	select {
	case line, open := <-c.output:
		if !open {
			c.t.Fatal("output ended")
		}
		return line
	case <-time.After(5 * time.Second):
		c.t.Fatal("no line from the agent")
		return ""
	}
}

func (c *acpClient) expect(want string) {
	c.t.Helper()
	if got := c.next(); got != want {
		c.t.Fatalf("line\n got: %s\nwant: %s", got, want)
	}
}

// expectAnyOrder reads len(want) lines, which can come in any order.
func (c *acpClient) expectAnyOrder(want ...string) {
	c.t.Helper()
	remaining := map[string]int{}
	for _, line := range want {
		remaining[line]++
	}
	for range want {
		got := c.next()
		if remaining[got] == 0 {
			c.t.Fatalf("unexpected line: %s\nwant one of: %q", got, want)
		}
		remaining[got]--
	}
}

func (c *acpClient) newSession(requestID int, sessionID string) {
	c.t.Helper()
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":"/work","mcpServers":[]}}`, requestID))
	c.expect(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"sessionId":%q}}`, requestID, sessionID))
}

func (c *acpClient) prompt(requestID int, sessionID, text string) {
	c.t.Helper()
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":%q}]}}`, requestID, sessionID, text))
}

func sessionUpdate(sessionID, update string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":%s}}`, sessionID, update)
}

func stopResponse(requestID int, reason string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":%q}}`, requestID, reason)
}

func toolStatus(sessionID, callID, status string) string {
	return sessionUpdate(sessionID, fmt.Sprintf(`{"sessionUpdate":"tool_call_update","toolCallId":%q,"status":%q}`, callID, status))
}

func toolDone(sessionID, callID, status, text string) string {
	return sessionUpdate(sessionID, fmt.Sprintf(`{"sessionUpdate":"tool_call_update","toolCallId":%q,"status":%q,"content":[{"type":"content","content":{"type":"text","text":%q}}]}`, callID, status, text))
}

func editCall(sessionID, callID string) string {
	return sessionUpdate(sessionID, fmt.Sprintf(`{"sessionUpdate":"tool_call","toolCallId":%q,"name":"edit_file","title":"Edit a.go","kind":"edit","status":"pending","rawInput":%s}`, callID, editInput))
}

func bashCall(sessionID, callID string) string {
	return sessionUpdate(sessionID, fmt.Sprintf(`{"sessionUpdate":"tool_call","toolCallId":%q,"name":"bash","title":"Run sleep 10","kind":"execute","status":"pending","rawInput":{"command":"sleep 10"}}`, callID))
}

const permissionOptions = `[{"optionId":"allow_once","name":"Allow once","kind":"allow_once"},{"optionId":"allow_always","name":"Always allow this tool","kind":"allow_always"},{"optionId":"reject_once","name":"Reject","kind":"reject_once"}]`

func editPermission(requestID int, sessionID, callID string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/request_permission","params":{"sessionId":%q,"toolCall":{"toolCallId":%q,"name":"edit_file","title":"Edit a.go","kind":"edit","status":"pending","rawInput":%s},"options":%s}}`, requestID, sessionID, callID, editInput, permissionOptions)
}

func selected(requestID int, option string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"outcome":{"outcome":"selected","optionId":%q}}}`, requestID, option)
}

func TestACPPromptTurn(t *testing.T) {
	backend := &fakeACPBackend{}
	client := startACP(t, backend)
	client.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true},"clientInfo":{"name":"zed","version":"1.0.0"}}}`)
	client.expect(`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false,"promptCapabilities":{"image":true,"audio":false,"embeddedContext":true}},"authMethods":[]}}`)
	client.newSession(1, "s1")
	// The request id is a string here: the response must give it back unchanged.
	client.send(`{"jsonrpc":"2.0","id":"p1","method":"session/prompt","params":{"sessionId":"s1","prompt":[` +
		`{"type":"text","text":"hello "},` +
		`{"type":"resource_link","uri":"file:///work/a.go","name":"a.go"},` +
		`{"type":"text","text":" and "},` +
		`{"type":"resource","resource":{"uri":"file:///work/b.go","mimeType":"text/x-go","text":"package b"}},` +
		`{"type":"image","mimeType":"image/png","data":"iVBORw0KGgo="}]}}`)
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"plan"}}`))
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hi"}}`))
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"tool_call","toolCallId":"call_1","name":"grep","title":"Search TODO in .","kind":"search","status":"pending","rawInput":{"pattern":"TODO"}}`))
	client.expect(toolDone("s1", "call_1", "completed", "a.go:1: TODO"))
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" done"}}`))
	client.expect(`{"jsonrpc":"2.0","id":"p1","result":{"stopReason":"end_turn"}}`)

	// A second turn in the same session gets a new tool call id.
	client.prompt(2, "s1", "hello again")
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"plan"}}`))
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hi"}}`))
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"tool_call","toolCallId":"call_2","name":"grep","title":"Search TODO in .","kind":"search","status":"pending","rawInput":{"pattern":"TODO"}}`))
	client.expect(toolDone("s1", "call_2", "completed", "a.go:1: TODO"))
	client.expect(sessionUpdate("s1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" done"}}`))
	client.expect(stopResponse(2, "end_turn"))

	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	if !reflect.DeepEqual(backend.cwds, []string{"/work"}) {
		t.Errorf("session cwds = %q, want [/work]", backend.cwds)
	}
	want := acpPrompt{
		Text:   "hello @file:///work/a.go and @file:///work/b.go\n\n<context uri=\"file:///work/b.go\">\npackage b\n</context>",
		Images: []anthropic.ImageBlock{{MediaType: "image/png", Data: "iVBORw0KGgo="}},
	}
	if !reflect.DeepEqual(backend.prompts[0], want) {
		t.Errorf("prompt = %#v\nwant %#v", backend.prompts[0], want)
	}
}

func TestACPPermissionAllowAlwaysIsPerSessionAndTool(t *testing.T) {
	backend := &fakeACPBackend{}
	client := startACP(t, backend)
	client.newSession(1, "s1")
	client.prompt(2, "s1", "edit 2")
	client.expect(editCall("s1", "call_1"))
	client.expect(editPermission(1, "s1", "call_1"))
	client.send(selected(1, "allow_always"))
	client.expect(toolStatus("s1", "call_1", "in_progress"))
	client.expect(toolDone("s1", "call_1", "completed", "edited"))
	// The second call does not ask again.
	client.expect(editCall("s1", "call_2"))
	client.expect(toolStatus("s1", "call_2", "in_progress"))
	client.expect(toolDone("s1", "call_2", "completed", "edited"))
	client.expect(stopResponse(2, "end_turn"))

	// The choice stays for later turns of the session.
	client.prompt(3, "s1", "edit 1")
	client.expect(editCall("s1", "call_3"))
	client.expect(toolStatus("s1", "call_3", "in_progress"))
	client.expect(toolDone("s1", "call_3", "completed", "edited"))
	client.expect(stopResponse(3, "end_turn"))

	// Another session asks, and allow_once asks again for each call.
	client.newSession(4, "s2")
	client.prompt(5, "s2", "edit 2")
	client.expect(editCall("s2", "call_1"))
	client.expect(editPermission(2, "s2", "call_1"))
	client.send(selected(2, "allow_once"))
	client.expect(toolStatus("s2", "call_1", "in_progress"))
	client.expect(toolDone("s2", "call_1", "completed", "edited"))
	client.expect(editCall("s2", "call_2"))
	client.expect(editPermission(3, "s2", "call_2"))
	client.send(selected(3, "allow_once"))
	client.expect(toolStatus("s2", "call_2", "in_progress"))
	client.expect(toolDone("s2", "call_2", "completed", "edited"))
	client.expect(stopResponse(5, "end_turn"))

	if got := backend.recordedDecisions(); !reflect.DeepEqual(got, []bool{true, true, true, true, true}) {
		t.Errorf("decisions = %v", got)
	}
}

func TestACPPermissionDenied(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{"reject once", selected(1, "reject_once")},
		{"cancelled outcome", `{"jsonrpc":"2.0","id":1,"result":{"outcome":{"outcome":"cancelled"}}}`},
		{"error response", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"dialog closed"}}`},
		{"option not offered", selected(1, "reject_always")},
		{"result without outcome", `{"jsonrpc":"2.0","id":1,"result":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeACPBackend{}
			client := startACP(t, backend)
			client.newSession(1, "s1")
			client.prompt(2, "s1", "edit 2")
			client.expect(editCall("s1", "call_1"))
			client.expect(editPermission(1, "s1", "call_1"))
			client.send(test.reply)
			client.expect(toolDone("s1", "call_1", "failed", "denied"))
			// A denial is not remembered: the next call asks again.
			client.expect(editCall("s1", "call_2"))
			client.expect(editPermission(2, "s1", "call_2"))
			client.send(selected(2, "reject_once"))
			client.expect(toolDone("s1", "call_2", "failed", "denied"))
			client.expect(stopResponse(2, "end_turn"))
			if got := backend.recordedDecisions(); !reflect.DeepEqual(got, []bool{false, false}) {
				t.Errorf("decisions = %v, want [false false]", got)
			}
		})
	}
}

func TestACPCancel(t *testing.T) {
	tests := []struct {
		name string
		text string
		// waitsForPermission is true when the agent asks the client before
		// the cancellation.
		waitsForPermission bool
	}{
		{"running tool", "wait", false},
		{"pending permission", "ask", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := startACP(t, &fakeACPBackend{})
			client.newSession(1, "s1")
			client.prompt(2, "s1", test.text)
			client.expect(bashCall("s1", "call_1"))
			if test.waitsForPermission {
				client.expect(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"call_1","name":"bash","title":"Run sleep 10","kind":"execute","status":"pending","rawInput":{"command":"sleep 10"}},"options":%s}}`, permissionOptions))
			}
			client.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`)
			client.expect(toolStatus("s1", "call_1", "failed"))
			client.expect(stopResponse(2, "cancelled"))
			if test.waitsForPermission {
				// The client answers the request late, as the specification
				// requires. The agent ignores the answer.
				client.send(`{"jsonrpc":"2.0","id":1,"result":{"outcome":{"outcome":"cancelled"}}}`)
			}
			// The session takes a new prompt after the cancellation.
			client.prompt(3, "s1", "max")
			client.expect(stopResponse(3, "max_tokens"))
		})
	}
}

func TestACPStopReasons(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"max", stopResponse(2, "max_tokens")},
		{"refuse", stopResponse(2, "refusal")},
		{"fail", `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"backend broke"}}`},
	}
	for _, test := range tests {
		t.Run(test.text, func(t *testing.T) {
			client := startACP(t, &fakeACPBackend{})
			client.newSession(1, "s1")
			client.prompt(2, "s1", test.text)
			client.expect(test.want)
		})
	}
}

func TestACPErrors(t *testing.T) {
	client := startACP(t, &fakeACPBackend{})
	client.newSession(1, "s1")
	prompt := func(id int, blocks string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":"s1","prompt":%s}}`, id, blocks)
	}
	tests := []struct {
		name   string
		line   string
		wantID string
		// wantCode 0 means that the agent must not answer.
		wantCode int
	}{
		{"truncated JSON", `{"jsonrpc":"2.0","id":2,`, "null", acpParseError},
		{"not JSON", `hello`, "null", acpParseError},
		{"batch array", `[{"jsonrpc":"2.0","id":3,"method":"initialize"}]`, "null", acpInvalidRequest},
		{"wrong jsonrpc version", `{"jsonrpc":"1.0","id":4,"method":"initialize","params":{"protocolVersion":1}}`, "4", acpInvalidRequest},
		{"no method and no result", `{"jsonrpc":"2.0","id":5}`, "5", acpInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":6,"method":"session/load","params":{"sessionId":"s1","cwd":"/work","mcpServers":[]}}`, "6", acpMethodNotFound},
		{"initialize without version", `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`, "7", acpInvalidParams},
		{"initialize without params", `{"jsonrpc":"2.0","id":8,"method":"initialize"}`, "8", acpInvalidParams},
		{"initialize with negative version", `{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":-1}}`, "9", acpInvalidParams},
		{"relative cwd", `{"jsonrpc":"2.0","id":10,"method":"session/new","params":{"cwd":"work","mcpServers":[]}}`, "10", acpInvalidParams},
		{"backend cannot create session", `{"jsonrpc":"2.0","id":11,"method":"session/new","params":{"cwd":"/fail","mcpServers":[]}}`, "11", acpInternalError},
		{"unknown session", `{"jsonrpc":"2.0","id":12,"method":"session/prompt","params":{"sessionId":"s9","prompt":[{"type":"text","text":"hello"}]}}`, "12", acpResourceNotFound},
		{"prompt params of wrong type", `{"jsonrpc":"2.0","id":13,"method":"session/prompt","params":{"sessionId":5,"prompt":[]}}`, "13", acpInvalidParams},
		{"audio block", prompt(14, `[{"type":"text","text":"listen"},{"type":"audio","mimeType":"audio/wav","data":"AAAA"}]`), "14", acpInvalidParams},
		{"unsupported image type", prompt(15, `[{"type":"text","text":"look"},{"type":"image","mimeType":"image/svg+xml","data":"AAAA"}]`), "15", acpInvalidParams},
		{"binary resource that is not an image", prompt(16, `[{"type":"text","text":"read"},{"type":"resource","resource":{"uri":"file:///a.pdf","mimeType":"application/pdf","blob":"AAAA"}}]`), "16", acpInvalidParams},
		{"no text", prompt(17, `[{"type":"text","text":"  "}]`), "17", acpInvalidParams},
		{"unknown block type", prompt(18, `[{"type":"text","text":"hi"},{"type":"video"}]`), "18", acpInvalidParams},
		{"unknown notification", `{"jsonrpc":"2.0","method":"_zed/hello","params":{}}`, "", 0},
		{"cancel without a running turn", `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`, "", 0},
		{"response to no request", `{"jsonrpc":"2.0","id":99,"result":{}}`, "", 0},
		{"blank line", ``, "", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client.t = t
			client.send(test.line)
			if test.wantCode == 0 {
				// The next answer must be the answer to this probe.
				client.send(`{"jsonrpc":"2.0","id":"probe","method":"probe"}`)
				test.wantID, test.wantCode = `"probe"`, acpMethodNotFound
			}
			var response struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Error   *acpError       `json:"error"`
			}
			line := client.next()
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				t.Fatalf("decode %s: %v", line, err)
			}
			if response.JSONRPC != "2.0" || string(response.ID) != test.wantID || response.Error == nil || response.Error.Code != test.wantCode || response.Error.Message == "" {
				t.Errorf("response = %s, want id %s and error code %d", line, test.wantID, test.wantCode)
			}
		})
	}
	client.t = t
}

func TestACPTwoSessions(t *testing.T) {
	client := startACP(t, &fakeACPBackend{})
	client.newSession(1, "s1")
	client.newSession(2, "s2")
	client.prompt(3, "s1", "wait")
	client.prompt(4, "s2", "wait")
	client.expectAnyOrder(bashCall("s1", "call_1"), bashCall("s2", "call_1"))

	// One prompt per session at a time.
	client.prompt(5, "s1", "hello")
	line := client.next()
	if !strings.HasPrefix(line, fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"error":{"code":%d,`, acpInvalidParams)) {
		t.Fatalf("second prompt in a busy session: %s", line)
	}

	// Cancellation of s1 does not stop s2.
	client.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`)
	client.expect(toolStatus("s1", "call_1", "failed"))
	client.expect(stopResponse(3, "cancelled"))
	client.prompt(6, "s1", "max")
	client.expect(stopResponse(6, "max_tokens"))

	client.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s2"}}`)
	client.expect(toolStatus("s2", "call_1", "failed"))
	client.expect(stopResponse(4, "cancelled"))
}

func TestACPEndOfInputEndsRunningTurns(t *testing.T) {
	client := startACP(t, &fakeACPBackend{})
	client.newSession(1, "s1")
	client.prompt(2, "s1", "ask")
	client.expect(bashCall("s1", "call_1"))
	if line := client.next(); !strings.Contains(line, `"method":"session/request_permission"`) {
		t.Fatalf("want a permission request, got %s", line)
	}
	client.input.Close()
	client.expect(toolStatus("s1", "call_1", "failed"))
	client.expect(stopResponse(2, "cancelled"))
}
