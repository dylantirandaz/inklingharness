package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// lineWriter writes one JSON value per line. Events, responses, and approval
// requests can come from different goroutines, so a mutex keeps each line
// whole. Each line goes out in one Write call, so an unbuffered writer such
// as stdout sends every event at once. After the first failure it writes
// nothing more, because a broken stream must not get partial lines.
type lineWriter struct {
	mutex   sync.Mutex
	output  io.Writer
	failure error
}

func (w *lineWriter) write(value any) error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if w.failure != nil {
		return w.failure
	}
	data, err := json.Marshal(value)
	if err != nil {
		w.failure = fmt.Errorf("encode JSON line: %w", err)
		return w.failure
	}
	if _, err := w.output.Write(append(data, '\n')); err != nil {
		w.failure = fmt.Errorf("write JSON line: %w", err)
	}
	return w.failure
}

func (w *lineWriter) err() error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.failure
}

type deltaEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

type toolCallEvent struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type toolResultEvent struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	Content       string `json:"content"`
	IsError       bool   `json:"is_error"`
	ElapsedMillis int64  `json:"elapsed_ms"`
}

type turnEvent struct {
	Type          string               `json:"type"`
	Turn          int                  `json:"turn"`
	StopReason    anthropic.StopReason `json:"stop_reason"`
	Usage         anthropic.Usage      `json:"usage"`
	ElapsedMillis int64                `json:"elapsed_ms"`
}

// messageEvent carries the status and error events.
type messageEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type unknownStreamEvent struct {
	Type  string `json:"type"`
	Event string `json:"event"`
}

// outcomeSummary is the end of a request: the done event in JSON mode and
// the prompt result in RPC mode.
type outcomeSummary struct {
	FinalText string          `json:"final_text"`
	Turns     int             `json:"turns"`
	Usage     anthropic.Usage `json:"usage"`
}

func summarize(outcome *agent.Outcome) outcomeSummary {
	return outcomeSummary{FinalText: outcome.FinalText, Turns: outcome.Turns, Usage: outcome.Usage}
}

type doneEvent struct {
	Type string `json:"type"`
	outcomeSummary
}

// jsonObserver writes agent events as JSON lines. The envelope puts each
// event in the frame of the mode: the bare event for `think run --json`, a
// notification for RPC mode.
type jsonObserver struct {
	lines    *lineWriter
	envelope func(event any) any
}

func newJSONObserver(w io.Writer) *jsonObserver {
	return &jsonObserver{lines: &lineWriter{output: w}, envelope: bareEvent}
}

func bareEvent(event any) any { return event }

// emit drops the write error here because Observer methods cannot return
// one. The lineWriter keeps it, and err returns it.
func (o *jsonObserver) emit(event any) { _ = o.lines.write(o.envelope(event)) }

func (o *jsonObserver) Text(delta string) {
	o.emit(deltaEvent{Type: "text", Delta: delta})
}

func (o *jsonObserver) Thinking(delta string) {
	o.emit(deltaEvent{Type: "thinking", Delta: delta})
}

// ToolCallStart has no event: the tool_call event follows with the input.
func (o *jsonObserver) ToolCallStart(string) {}

func (o *jsonObserver) ToolCall(name string, input json.RawMessage) {
	o.emit(toolCallEvent{Type: "tool_call", Name: name, Input: input})
}

func (o *jsonObserver) ToolResult(name string, result tools.Result, elapsed time.Duration) {
	o.emit(toolResultEvent{Type: "tool_result", Name: name, Content: result.Content, IsError: result.IsError, ElapsedMillis: elapsed.Milliseconds()})
}

func (o *jsonObserver) TurnDone(turn int, usage anthropic.Usage, stop anthropic.StopReason, elapsed time.Duration) {
	o.emit(turnEvent{Type: "turn", Turn: turn, StopReason: stop, Usage: usage, ElapsedMillis: elapsed.Milliseconds()})
}

func (o *jsonObserver) UnknownEvent(eventType string) {
	o.emit(unknownStreamEvent{Type: "unknown_event", Event: eventType})
}

func (o *jsonObserver) Status(message string) {
	o.emit(messageEvent{Type: "status", Message: message})
}

// writeDone writes the last event of a successful run.
func (o *jsonObserver) writeDone(outcome *agent.Outcome) {
	o.emit(doneEvent{Type: "done", outcomeSummary: summarize(outcome)})
}

// writeError writes the last event of a failed run.
func (o *jsonObserver) writeError(err error) {
	o.emit(messageEvent{Type: "error", Message: err.Error()})
}

// err returns the first write failure, or nil.
func (o *jsonObserver) err() error { return o.lines.err() }

// maxRPCLine is the longest request line. A prompt with large pasted text
// must fit, but one line must not use unlimited memory.
const maxRPCLine = 16 << 20

// rpcRunner runs one prompt. The approve function asks the client.
type rpcRunner func(ctx context.Context, prompt string, observer agent.Observer, approve func(context.Context, anthropic.ToolUseBlock) (bool, error)) (*agent.Outcome, error)

// rpcMessage is one line from the client: a request when Method is set, or
// the answer to an approval request when it is not.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

type rpcResult struct {
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result"`
}

type rpcErrorBody struct {
	Message string `json:"message"`
}

type rpcFailure struct {
	ID    json.RawMessage `json:"id"`
	Error rpcErrorBody    `json:"error"`
}

type rpcNotification struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcServerRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type approvalParams struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

// nullID answers a line that has no usable id.
var nullID = json.RawMessage("null")

func eventNotification(event any) any {
	return rpcNotification{Method: "event", Params: event}
}

type promptCompletion struct {
	outcome *agent.Outcome
	err     error
}

type activePrompt struct {
	id       json.RawMessage
	cancel   context.CancelFunc
	finished chan promptCompletion
}

// rpcServer state belongs to the serveRPC goroutine, except approvals,
// which the approve function also uses from the prompt goroutine.
type rpcServer struct {
	lines  *lineWriter
	run    rpcRunner
	active *activePrompt
	// inputEnded is closed at the end of stdin, so pending approvals stop.
	inputEnded chan struct{}

	approvalMutex sync.Mutex
	approvals     map[string]chan bool
	lastApproval  int
}

// serveRPC runs prompts for an editor over newline-delimited JSON. It
// returns nil after a shutdown request or the end of in, and only after the
// running prompt has ended and its response is written.
func serveRPC(ctx context.Context, in io.Reader, out io.Writer, run rpcRunner) error {
	server := &rpcServer{
		lines:      &lineWriter{output: out},
		run:        run,
		inputEnded: make(chan struct{}),
		approvals:  map[string]chan bool{},
	}
	stop := make(chan struct{})
	defer close(stop)
	requests, readEnded := readLines(in, stop)
	for {
		select {
		case line := <-requests:
			if server.handle(ctx, line) {
				return server.lines.err()
			}
		case err := <-readEnded:
			close(server.inputEnded)
			server.endPrompt()
			if err != nil {
				_ = server.lines.write(rpcFailure{ID: nullID, Error: rpcErrorBody{Message: "read request: " + err.Error()}})
				return fmt.Errorf("read request: %w", err)
			}
			return server.lines.err()
		case completion := <-server.promptFinished():
			server.respond(completion)
		case <-ctx.Done():
			server.endPrompt()
			return ctx.Err()
		}
		if err := server.lines.err(); err != nil {
			server.endPrompt()
			return err
		}
	}
}

// readLines reads in on its own goroutine, so a blocked read does not stop
// cancellation. The goroutine stops at the next line after stop is closed,
// or at the end of in.
func readLines(in io.Reader, stop <-chan struct{}) (<-chan []byte, <-chan error) {
	lines := make(chan []byte)
	ended := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 0, 64<<10), maxRPCLine)
		for scanner.Scan() {
			select {
			case lines <- bytes.Clone(scanner.Bytes()):
			case <-stop:
				return
			}
		}
		ended <- scanner.Err()
	}()
	return lines, ended
}

// promptFinished is nil when no prompt runs, so the select ignores it.
func (s *rpcServer) promptFinished() <-chan promptCompletion {
	if s.active == nil {
		return nil
	}
	return s.active.finished
}

// endPrompt cancels the running prompt, waits for it, and writes its response.
func (s *rpcServer) endPrompt() {
	if s.active == nil {
		return
	}
	s.active.cancel()
	s.respond(<-s.active.finished)
}

func (s *rpcServer) respond(completion promptCompletion) {
	id := s.active.id
	s.active.cancel()
	s.active = nil
	if completion.err != nil {
		s.fail(id, completion.err.Error())
		return
	}
	_ = s.lines.write(rpcResult{ID: id, Result: summarize(completion.outcome)})
}

// fail and reply drop the write error because serveRPC checks the
// lineWriter after each message.
func (s *rpcServer) fail(id json.RawMessage, message string) {
	_ = s.lines.write(rpcFailure{ID: id, Error: rpcErrorBody{Message: message}})
}

func (s *rpcServer) reply(id json.RawMessage) {
	_ = s.lines.write(rpcResult{ID: id, Result: struct{}{}})
}

// handle processes one client line and reports whether to shut down.
func (s *rpcServer) handle(ctx context.Context, line []byte) bool {
	// Blank lines carry no message. Clients often send one at the end.
	if len(bytes.TrimSpace(line)) == 0 {
		return false
	}
	var message rpcMessage
	if err := json.Unmarshal(line, &message); err != nil {
		s.fail(nullID, "parse message: "+err.Error())
		return false
	}
	if message.Method == "" {
		s.answerApproval(message)
		return false
	}
	if len(message.ID) == 0 || bytes.Equal(message.ID, nullID) {
		s.fail(nullID, fmt.Sprintf("request %q needs an id", message.Method))
		return false
	}
	switch message.Method {
	case "prompt":
		s.startPrompt(ctx, message)
	case "cancel":
		if s.active != nil {
			s.active.cancel()
		}
		s.reply(message.ID)
	case "shutdown":
		s.endPrompt()
		s.reply(message.ID)
		return true
	default:
		s.fail(message.ID, fmt.Sprintf("unknown method %q", message.Method))
	}
	return false
}

func (s *rpcServer) startPrompt(ctx context.Context, message rpcMessage) {
	if s.active != nil {
		s.fail(message.ID, "busy")
		return
	}
	var params struct {
		Text string `json:"text"`
	}
	if len(message.Params) == 0 {
		s.fail(message.ID, "prompt needs params.text")
		return
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		s.fail(message.ID, "parse prompt params: "+err.Error())
		return
	}
	if params.Text == "" {
		s.fail(message.ID, "prompt needs params.text")
		return
	}
	promptContext, cancel := context.WithCancel(ctx)
	prompt := &activePrompt{id: message.ID, cancel: cancel, finished: make(chan promptCompletion, 1)}
	s.active = prompt
	observer := &jsonObserver{lines: s.lines, envelope: eventNotification}
	go func() {
		outcome, err := s.run(promptContext, params.Text, observer, s.approve)
		prompt.finished <- promptCompletion{outcome: outcome, err: err}
	}()
}

// approve asks the client and waits for the answer. Cancellation and the end
// of stdin deny the call with an error, so the run stops.
func (s *rpcServer) approve(ctx context.Context, call anthropic.ToolUseBlock) (bool, error) {
	answer := make(chan bool, 1)
	s.approvalMutex.Lock()
	s.lastApproval++
	id := fmt.Sprintf("approve-%d", s.lastApproval)
	s.approvals[id] = answer
	s.approvalMutex.Unlock()
	defer func() {
		s.approvalMutex.Lock()
		delete(s.approvals, id)
		s.approvalMutex.Unlock()
	}()
	request := rpcServerRequest{ID: id, Method: "approve", Params: approvalParams{Tool: call.Name, Input: call.Input}}
	if err := s.lines.write(request); err != nil {
		return false, err
	}
	select {
	case allow := <-answer:
		return allow, nil
	case <-s.inputEnded:
		return false, io.ErrUnexpectedEOF
	case <-ctx.Done():
		// The end of stdin also cancels the prompt. The cause that came
		// first gives the clearer error.
		select {
		case <-s.inputEnded:
			return false, io.ErrUnexpectedEOF
		default:
			return false, ctx.Err()
		}
	}
}

func (s *rpcServer) answerApproval(message rpcMessage) {
	var id string
	if err := json.Unmarshal(message.ID, &id); err != nil {
		s.fail(nullID, "message needs a method or an approval id")
		return
	}
	var result struct {
		Allow *bool `json:"allow"`
	}
	if len(message.Result) == 0 {
		s.fail(message.ID, "approval answer needs result.allow")
		return
	}
	if err := json.Unmarshal(message.Result, &result); err != nil {
		s.fail(message.ID, "parse approval answer: "+err.Error())
		return
	}
	if result.Allow == nil {
		s.fail(message.ID, "approval answer needs result.allow")
		return
	}
	s.approvalMutex.Lock()
	pending, found := s.approvals[id]
	delete(s.approvals, id)
	s.approvalMutex.Unlock()
	if !found {
		s.fail(message.ID, "no approval is pending with this id")
		return
	}
	pending <- *result.Allow
}
