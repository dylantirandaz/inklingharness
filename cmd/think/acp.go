package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// This file is the agent side of the Agent Client Protocol (ACP). Editors
// such as Zed use ACP to drive a coding agent. The messages are JSON-RPC 2.0,
// one JSON value per line, over stdin and stdout.
//
// Specification: ACP protocol version 1, schema release schema-v1.24.1
// (https://github.com/agentclientprotocol/agent-client-protocol,
// schema/v1/schema.json, and https://agentclientprotocol.com/protocol/v1).
//
// Supported agent methods: initialize, session/new, session/prompt, and the
// session/cancel notification. The agent sends session/update notifications
// and session/request_permission requests. The agent does not advertise
// optional methods (authenticate, session/load, session/resume,
// session/close, session/list, session/set_mode, session/set_config_option,
// logout), so a client that calls them gets "method not found".

// acpProtocolVersion is the only ACP major version that this agent knows.
// Version negotiation tells the client to use it.
const acpProtocolVersion = 1

const jsonRPCVersion = "2.0"

// JSON-RPC 2.0 error codes, and the ACP code for a missing resource.
const (
	acpParseError       = -32700
	acpInvalidRequest   = -32600
	acpMethodNotFound   = -32601
	acpInvalidParams    = -32602
	acpInternalError    = -32603
	acpResourceNotFound = -32002
)

// acpBackend creates sessions and runs prompt turns. serveACP calls Prompt
// for one session at a time, but it can call Prompt for different sessions
// at the same time.
type acpBackend interface {
	NewSession(ctx context.Context, cwd string) (string, error)
	Prompt(ctx context.Context, sessionID string, prompt agent.Prompt, observer agent.Observer, approve func(context.Context, anthropic.ToolUseBlock) (bool, error)) (*agent.Outcome, error)
}

// acpIncoming is one line from the client. A request has a method and an
// id, a notification has a method and no id, and a response to an agent
// request has an id and a result or an error.
type acpIncoming struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *acpError       `json:"error"`
}

type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpResult struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type acpFailure struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   acpError        `json:"error"`
}

type acpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type acpNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type acpInitializeResult struct {
	ProtocolVersion   uint16               `json:"protocolVersion"`
	AgentCapabilities acpAgentCapabilities `json:"agentCapabilities"`
	// AuthMethods is always empty: the agent uses the credentials of think.
	AuthMethods []struct{} `json:"authMethods"`
}

type acpAgentCapabilities struct {
	LoadSession        bool                  `json:"loadSession"`
	PromptCapabilities acpPromptCapabilities `json:"promptCapabilities"`
}

type acpPromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type acpNewSessionResult struct {
	SessionID string `json:"sessionId"`
}

type acpPromptResult struct {
	StopReason acpStopReason `json:"stopReason"`
}

// acpStopReason is why a prompt turn ended. The agent never sends
// max_turn_requests, because the backend reports its turn limit as an error
// that it does not identify.
type acpStopReason string

const (
	acpStopEndTurn   acpStopReason = "end_turn"
	acpStopMaxTokens acpStopReason = "max_tokens"
	acpStopRefusal   acpStopReason = "refusal"
	acpStopCancelled acpStopReason = "cancelled"
)

// acpContentBlock is one prompt content block. Type selects the fields that
// apply: text uses Text; image uses MimeType and Data; resource_link uses
// URI; resource uses Resource.
type acpContentBlock struct {
	Type     string               `json:"type"`
	Text     string               `json:"text"`
	MimeType string               `json:"mimeType"`
	Data     string               `json:"data"`
	URI      string               `json:"uri"`
	Resource *acpEmbeddedResource `json:"resource"`
}

// acpEmbeddedResource has Text for a text resource or Blob (base64) for a
// binary resource.
type acpEmbeddedResource struct {
	URI      string  `json:"uri"`
	MimeType string  `json:"mimeType"`
	Text     *string `json:"text"`
	Blob     *string `json:"blob"`
}

type acpSessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

type acpText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func newACPText(text string) acpText { return acpText{Type: "text", Text: text} }

type acpContentChunk struct {
	SessionUpdate string  `json:"sessionUpdate"`
	Content       acpText `json:"content"`
}

type acpToolKind string

const (
	acpKindRead    acpToolKind = "read"
	acpKindEdit    acpToolKind = "edit"
	acpKindSearch  acpToolKind = "search"
	acpKindExecute acpToolKind = "execute"
	acpKindFetch   acpToolKind = "fetch"
	acpKindOther   acpToolKind = "other"
)

type acpToolStatus string

const (
	acpToolPending    acpToolStatus = "pending"
	acpToolInProgress acpToolStatus = "in_progress"
	acpToolCompleted  acpToolStatus = "completed"
	acpToolFailed     acpToolStatus = "failed"
)

// acpToolCallReport is a ToolCall when SessionUpdate is "tool_call", and a
// ToolCallUpdate otherwise. An update sets only the fields that changed.
type acpToolCallReport struct {
	SessionUpdate string           `json:"sessionUpdate,omitempty"`
	ToolCallID    string           `json:"toolCallId"`
	Name          string           `json:"name,omitempty"`
	Title         string           `json:"title,omitempty"`
	Kind          acpToolKind      `json:"kind,omitempty"`
	Status        acpToolStatus    `json:"status,omitempty"`
	RawInput      json.RawMessage  `json:"rawInput,omitempty"`
	Content       []acpToolContent `json:"content,omitempty"`
}

type acpToolContent struct {
	Type    string  `json:"type"`
	Content acpText `json:"content"`
}

// acpPermission is both the id and the kind of one permission option.
type acpPermission string

const (
	acpAllowOnce   acpPermission = "allow_once"
	acpAllowAlways acpPermission = "allow_always"
	acpRejectOnce  acpPermission = "reject_once"
)

type acpPermissionOption struct {
	OptionID acpPermission `json:"optionId"`
	Name     string        `json:"name"`
	Kind     acpPermission `json:"kind"`
}

var acpPermissionOptions = []acpPermissionOption{
	{OptionID: acpAllowOnce, Name: "Allow once", Kind: acpAllowOnce},
	{OptionID: acpAllowAlways, Name: "Always allow this tool", Kind: acpAllowAlways},
	{OptionID: acpRejectOnce, Name: "Reject", Kind: acpRejectOnce},
}

type acpPermissionParams struct {
	SessionID string                `json:"sessionId"`
	ToolCall  acpToolCallReport     `json:"toolCall"`
	Options   []acpPermissionOption `json:"options"`
}

// acpReply is the client response to one agent request.
type acpReply struct {
	result json.RawMessage
	err    *acpError
}

// acpSession is one conversation. Only the serveACP goroutine uses turn.
// Turns of one session never overlap, so lastToolCall needs no lock.
type acpSession struct {
	id   string
	turn *acpTurn
	// lastToolCall numbers the tool calls, so each id is unique in the session.
	lastToolCall int

	allowMutex sync.Mutex
	// alwaysAllowed holds the tool names that the user allowed for the rest
	// of the session. It is nil until the first such choice.
	alwaysAllowed map[string]bool
}

func (s *acpSession) allows(tool string) bool {
	s.allowMutex.Lock()
	defer s.allowMutex.Unlock()
	return s.alwaysAllowed[tool]
}

func (s *acpSession) allowAlways(tool string) {
	s.allowMutex.Lock()
	defer s.allowMutex.Unlock()
	if s.alwaysAllowed == nil {
		s.alwaysAllowed = map[string]bool{}
	}
	s.alwaysAllowed[tool] = true
}

// acpTurn is a running session/prompt request.
type acpTurn struct {
	requestID json.RawMessage
	cancel    context.CancelFunc
}

// acpTurnEnd is the end of a turn: a stop reason when err is nil.
type acpTurnEnd struct {
	session    *acpSession
	stopReason acpStopReason
	err        error
}

// acpServer state belongs to the serveACP goroutine, except replies, which
// the turn goroutines also use.
type acpServer struct {
	lines    *lineWriter
	backend  acpBackend
	sessions map[string]*acpSession
	// turnEnded carries each ended turn to serveACP. The turn goroutine
	// writes all its updates before it sends, so the response comes last.
	turnEnded chan acpTurnEnd
	// inputEnded is closed at the end of the input, so waits for client
	// replies stop.
	inputEnded chan struct{}

	replyMutex  sync.Mutex
	replies     map[int64]chan acpReply
	lastRequest int64
}

// serveACP serves ACP over newline-delimited JSON. It returns nil at the end
// of in, after it cancels the running turns and writes their responses.
func serveACP(ctx context.Context, in io.Reader, out io.Writer, backend acpBackend) error {
	server := &acpServer{
		lines:      &lineWriter{output: out},
		backend:    backend,
		sessions:   map[string]*acpSession{},
		turnEnded:  make(chan acpTurnEnd),
		inputEnded: make(chan struct{}),
		replies:    map[int64]chan acpReply{},
	}
	stop := make(chan struct{})
	defer close(stop)
	messages, readEnded := readLines(in, stop)
	for {
		select {
		case line := <-messages:
			server.handle(ctx, line)
		case end := <-server.turnEnded:
			server.finishTurn(end)
		case err := <-readEnded:
			close(server.inputEnded)
			server.endTurns()
			if err != nil {
				server.fail(nullID, acpParseError, "read message: "+err.Error())
				return fmt.Errorf("read ACP message: %w", err)
			}
			return server.lines.err()
		case <-ctx.Done():
			server.endTurns()
			return ctx.Err()
		}
		if err := server.lines.err(); err != nil {
			server.endTurns()
			return err
		}
	}
}

// endTurns cancels all running turns and writes their responses.
func (s *acpServer) endTurns() {
	running := 0
	for _, session := range s.sessions {
		if session.turn != nil {
			session.turn.cancel()
			running++
		}
	}
	for range running {
		s.finishTurn(<-s.turnEnded)
	}
}

func (s *acpServer) finishTurn(end acpTurnEnd) {
	turn := end.session.turn
	end.session.turn = nil
	turn.cancel()
	if end.err != nil {
		s.fail(turn.requestID, acpInternalError, end.err.Error())
		return
	}
	s.reply(turn.requestID, acpPromptResult{StopReason: end.stopReason})
}

// reply and fail drop the write error because serveACP checks the
// lineWriter after each message.
func (s *acpServer) reply(id json.RawMessage, result any) {
	_ = s.lines.write(acpResult{JSONRPC: jsonRPCVersion, ID: id, Result: result})
}

func (s *acpServer) fail(id json.RawMessage, code int, message string) {
	_ = s.lines.write(acpFailure{JSONRPC: jsonRPCVersion, ID: id, Error: acpError{Code: code, Message: message}})
}

func (s *acpServer) handle(ctx context.Context, line []byte) {
	// Blank lines carry no message.
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var message acpIncoming
	if err := json.Unmarshal(line, &message); err != nil {
		// Valid JSON of the wrong shape, such as a batch array, is an
		// invalid request. Only text that is not JSON is a parse error.
		var shape *json.UnmarshalTypeError
		if errors.As(err, &shape) {
			s.fail(nullID, acpInvalidRequest, "invalid request: "+err.Error())
			return
		}
		s.fail(nullID, acpParseError, "parse error: "+err.Error())
		return
	}
	id := message.ID
	if len(id) == 0 {
		id = nullID
	}
	if message.JSONRPC != jsonRPCVersion {
		s.fail(id, acpInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
		return
	}
	switch {
	case message.Method != "" && len(message.ID) == 0:
		s.notification(message)
	case message.Method != "":
		s.request(ctx, message)
	case len(message.ID) > 0 && (message.Result != nil || message.Error != nil):
		s.answer(message)
	default:
		s.fail(id, acpInvalidRequest, "invalid request: a message needs a method, a result, or an error")
	}
}

func (s *acpServer) request(ctx context.Context, message acpIncoming) {
	switch message.Method {
	case "initialize":
		s.initialize(message)
	case "session/new":
		s.newSession(ctx, message)
	case "session/prompt":
		s.prompt(ctx, message)
	default:
		s.fail(message.ID, acpMethodNotFound, fmt.Sprintf("method not found: %q", message.Method))
	}
}

func (s *acpServer) notification(message acpIncoming) {
	switch message.Method {
	case "session/cancel":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		// JSON-RPC forbids a response to a notification, so bad params are
		// ignored.
		if decodeACPParams(message.Params, &params) != nil {
			return
		}
		if session, found := s.sessions[params.SessionID]; found && session.turn != nil {
			session.turn.cancel()
		}
	default:
		// Clients can send other notifications, such as extension
		// notifications. JSON-RPC forbids a response, so they are ignored.
	}
}

// answer gives a client response to the agent request that waits for it.
// A response with an unknown id has no receiver, and JSON-RPC forbids a
// response to a response, so it is ignored.
func (s *acpServer) answer(message acpIncoming) {
	var id int64
	if json.Unmarshal(message.ID, &id) != nil {
		return
	}
	s.replyMutex.Lock()
	waiting, found := s.replies[id]
	delete(s.replies, id)
	s.replyMutex.Unlock()
	if found {
		waiting <- acpReply{result: message.Result, err: message.Error}
	}
}

func decodeACPParams(params json.RawMessage, into any) error {
	if len(params) == 0 {
		return errors.New("params are missing")
	}
	return json.Unmarshal(params, into)
}

func (s *acpServer) initialize(message acpIncoming) {
	var params struct {
		ProtocolVersion *uint16 `json:"protocolVersion"`
	}
	if err := decodeACPParams(message.Params, &params); err != nil {
		s.fail(message.ID, acpInvalidParams, "invalid params: "+err.Error())
		return
	}
	if params.ProtocolVersion == nil {
		s.fail(message.ID, acpInvalidParams, "invalid params: protocolVersion is required")
		return
	}
	// The agent answers with its only version. A client with another
	// version closes the connection.
	s.reply(message.ID, acpInitializeResult{
		ProtocolVersion: acpProtocolVersion,
		AgentCapabilities: acpAgentCapabilities{
			LoadSession:        false,
			PromptCapabilities: acpPromptCapabilities{Image: true, Audio: false, EmbeddedContext: true},
		},
		AuthMethods: []struct{}{},
	})
}

// newSession ignores params.mcpServers: think does not connect to MCP
// servers of the client. The agent does not advertise mcpCapabilities, so
// a client can send only stdio servers, and the agent does not start them.
func (s *acpServer) newSession(ctx context.Context, message acpIncoming) {
	var params struct {
		CWD *string `json:"cwd"`
	}
	if err := decodeACPParams(message.Params, &params); err != nil {
		s.fail(message.ID, acpInvalidParams, "invalid params: "+err.Error())
		return
	}
	if params.CWD == nil || !filepath.IsAbs(*params.CWD) {
		s.fail(message.ID, acpInvalidParams, "invalid params: cwd must be an absolute path")
		return
	}
	id, err := s.backend.NewSession(ctx, *params.CWD)
	if err != nil {
		s.fail(message.ID, acpInternalError, "create session: "+err.Error())
		return
	}
	if _, taken := s.sessions[id]; taken || id == "" {
		s.fail(message.ID, acpInternalError, fmt.Sprintf("create session: the backend gave an empty or used session id %q", id))
		return
	}
	s.sessions[id] = &acpSession{id: id}
	s.reply(message.ID, acpNewSessionResult{SessionID: id})
}

func (s *acpServer) prompt(ctx context.Context, message acpIncoming) {
	var params struct {
		SessionID string            `json:"sessionId"`
		Prompt    []acpContentBlock `json:"prompt"`
	}
	if err := decodeACPParams(message.Params, &params); err != nil {
		s.fail(message.ID, acpInvalidParams, "invalid params: "+err.Error())
		return
	}
	session, found := s.sessions[params.SessionID]
	if !found {
		s.fail(message.ID, acpResourceNotFound, fmt.Sprintf("session not found: %q", params.SessionID))
		return
	}
	if session.turn != nil {
		s.fail(message.ID, acpInvalidParams, fmt.Sprintf("invalid params: session %q already runs a prompt", params.SessionID))
		return
	}
	prompt, err := agentPrompt(params.Prompt)
	if err != nil {
		s.fail(message.ID, acpInvalidParams, "invalid params: "+err.Error())
		return
	}
	turnContext, cancel := context.WithCancel(ctx)
	session.turn = &acpTurn{requestID: message.ID, cancel: cancel}
	go s.runTurn(turnContext, session, prompt)
}

// agentPrompt converts ACP content blocks to one prompt. Text blocks join in
// order. A resource link or an embedded resource puts "@" and its URI in the
// text where it occurs, and the text of an embedded resource follows the
// prompt in a context section.
func agentPrompt(blocks []acpContentBlock) (agent.Prompt, error) {
	var text, sections strings.Builder
	var images []anthropic.ImageBlock
	for index, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "image":
			image, err := acpImage(block.MimeType, block.Data)
			if err != nil {
				return agent.Prompt{}, fmt.Errorf("prompt[%d]: %w", index, err)
			}
			images = append(images, image)
		case "resource_link":
			if block.URI == "" {
				return agent.Prompt{}, fmt.Errorf("prompt[%d]: resource_link needs a uri", index)
			}
			text.WriteString("@" + block.URI)
		case "resource":
			resource := block.Resource
			if resource == nil || resource.URI == "" {
				return agent.Prompt{}, fmt.Errorf("prompt[%d]: resource needs a resource with a uri", index)
			}
			switch {
			case resource.Text != nil:
				text.WriteString("@" + resource.URI)
				fmt.Fprintf(&sections, "\n\n<context uri=%q>\n%s\n</context>", resource.URI, *resource.Text)
			case resource.Blob != nil:
				image, err := acpImage(resource.MimeType, *resource.Blob)
				if err != nil {
					return agent.Prompt{}, fmt.Errorf("prompt[%d]: binary resource %s: %w", index, resource.URI, err)
				}
				text.WriteString("@" + resource.URI)
				images = append(images, image)
			default:
				return agent.Prompt{}, fmt.Errorf("prompt[%d]: resource needs text or blob", index)
			}
		case "audio":
			return agent.Prompt{}, fmt.Errorf("prompt[%d]: audio is not supported", index)
		default:
			return agent.Prompt{}, fmt.Errorf("prompt[%d]: unknown content type %q", index, block.Type)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return agent.Prompt{}, errors.New("prompt needs text or a resource")
	}
	text.WriteString(sections.String())
	return agent.Prompt{Text: text.String(), Images: images}, nil
}

// acpImage accepts the image types of the Messages API. The API checks the
// base64 data, so a bad encoding fails the prompt turn.
func acpImage(mimeType, data string) (anthropic.ImageBlock, error) {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return anthropic.ImageBlock{}, fmt.Errorf("unsupported image type %q", mimeType)
	}
	if data == "" {
		return anthropic.ImageBlock{}, errors.New("image data is empty")
	}
	return anthropic.ImageBlock{MediaType: mimeType, Data: data}, nil
}

func (s *acpServer) runTurn(ctx context.Context, session *acpSession, prompt agent.Prompt) {
	observer := &acpObserver{server: s, session: session}
	_, err := s.backend.Prompt(ctx, session.id, prompt, observer, observer.approve)
	observer.failOpenCalls()
	end := acpTurnEnd{session: session}
	end.stopReason, end.err = acpStopReasonOf(ctx.Err() != nil, observer.lastStopReason(), err)
	s.turnEnded <- end
}

// acpStopReasonOf maps the end of a run to a stop reason. A cancelled turn
// always ends with "cancelled", as the specification requires. The backend
// reports max_tokens and refusal as errors after a model turn with that
// stop reason, so the last stop reason identifies them.
func acpStopReasonOf(cancelled bool, last anthropic.StopReason, err error) (acpStopReason, error) {
	if cancelled {
		return acpStopCancelled, nil
	}
	if err == nil {
		return acpStopEndTurn, nil
	}
	switch last {
	case anthropic.StopMaxTokens:
		return acpStopMaxTokens, nil
	case anthropic.StopRefusal:
		return acpStopRefusal, nil
	case anthropic.StopEndTurn, anthropic.StopStopSequence, anthropic.StopToolUse, anthropic.StopPauseTurn:
		return "", err
	default:
		// No model turn ended, or the API sent a stop reason that this
		// version does not know.
		return "", err
	}
}

// call sends one agent request to the client and waits for the response.
func (s *acpServer) call(ctx context.Context, method string, params any) (acpReply, error) {
	waiting := make(chan acpReply, 1)
	s.replyMutex.Lock()
	s.lastRequest++
	id := s.lastRequest
	s.replies[id] = waiting
	s.replyMutex.Unlock()
	defer func() {
		s.replyMutex.Lock()
		delete(s.replies, id)
		s.replyMutex.Unlock()
	}()
	if err := s.lines.write(acpRequest{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params}); err != nil {
		return acpReply{}, err
	}
	select {
	case reply := <-waiting:
		return reply, nil
	case <-s.inputEnded:
		return acpReply{}, io.ErrUnexpectedEOF
	case <-ctx.Done():
		return acpReply{}, ctx.Err()
	}
}

// acpOpenCall is a reported tool call without a result.
type acpOpenCall struct {
	id    string
	name  string
	input json.RawMessage
	// claimed is set when approve takes the call, so that a second call with
	// the same input gets the next open call.
	claimed bool
}

// acpObserver sends the events of one turn as session/update notifications.
// The Observer contract gives tool results in call order, so the oldest open
// call gets the next result.
type acpObserver struct {
	server  *acpServer
	session *acpSession

	mutex    sync.Mutex
	open     []acpOpenCall
	lastStop anthropic.StopReason
}

// update drops the write error because Observer methods cannot return one.
// The lineWriter keeps it, and serveACP returns it.
func (o *acpObserver) update(update any) {
	_ = o.server.lines.write(acpNotification{
		JSONRPC: jsonRPCVersion,
		Method:  "session/update",
		Params:  acpSessionNotification{SessionID: o.session.id, Update: update},
	})
}

func (o *acpObserver) Text(delta string) {
	if delta != "" {
		o.update(acpContentChunk{SessionUpdate: "agent_message_chunk", Content: newACPText(delta)})
	}
}

func (o *acpObserver) Thinking(delta string) {
	if delta != "" {
		o.update(acpContentChunk{SessionUpdate: "agent_thought_chunk", Content: newACPText(delta)})
	}
}

// ToolCallStart has no update: the tool_call update follows with the input.
func (o *acpObserver) ToolCallStart(string) {}

func (o *acpObserver) ToolCall(name string, input json.RawMessage) {
	o.mutex.Lock()
	o.session.lastToolCall++
	id := fmt.Sprintf("call_%d", o.session.lastToolCall)
	o.open = append(o.open, acpOpenCall{id: id, name: name, input: input})
	o.mutex.Unlock()
	report := describeACPToolCall(id, name, input)
	report.SessionUpdate = "tool_call"
	o.update(report)
}

func (o *acpObserver) ToolResult(name string, result tools.Result, _ time.Duration) {
	o.mutex.Lock()
	if len(o.open) == 0 || o.open[0].name != name {
		o.mutex.Unlock()
		panic(fmt.Sprintf("acp: result of tool %q does not follow its tool call", name))
	}
	id := o.open[0].id
	o.open = o.open[1:]
	o.mutex.Unlock()
	status := acpToolCompleted
	if result.IsError {
		status = acpToolFailed
	}
	o.update(acpToolCallReport{
		SessionUpdate: "tool_call_update",
		ToolCallID:    id,
		Status:        status,
		Content:       []acpToolContent{{Type: "content", Content: newACPText(result.Content)}},
	})
}

func (o *acpObserver) TurnDone(_ int, _ anthropic.Usage, stop anthropic.StopReason, _ time.Duration) {
	o.mutex.Lock()
	o.lastStop = stop
	o.mutex.Unlock()
}

// UnknownEvent and Status have no ACP update. Status tells about retries and
// compaction, which are not part of the conversation.
func (o *acpObserver) UnknownEvent(string) {}

func (o *acpObserver) Status(string) {}

func (o *acpObserver) lastStopReason() anthropic.StopReason {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	return o.lastStop
}

// failOpenCalls marks the calls without a result as failed. This occurs
// when a turn stops before its tools run.
func (o *acpObserver) failOpenCalls() {
	o.mutex.Lock()
	open := o.open
	o.open = nil
	o.mutex.Unlock()
	for _, call := range open {
		o.update(acpToolCallReport{SessionUpdate: "tool_call_update", ToolCallID: call.id, Status: acpToolFailed})
	}
}

// approve asks the client unless the user allowed the tool for the session.
// A rejection, a cancelled outcome, an error response, or an unknown option
// denies the call. Cancellation of the turn and the end of the input stop
// the wait with an error, so the run stops.
func (o *acpObserver) approve(ctx context.Context, call anthropic.ToolUseBlock) (bool, error) {
	id := o.claim(call)
	if !o.session.allows(call.Name) {
		report := describeACPToolCall(id, call.Name, call.Input)
		reply, err := o.server.call(ctx, "session/request_permission", acpPermissionParams{SessionID: o.session.id, ToolCall: report, Options: acpPermissionOptions})
		if err != nil {
			return false, err
		}
		if !o.allowedBy(call.Name, reply) {
			return false, nil
		}
	}
	o.update(acpToolCallReport{SessionUpdate: "tool_call_update", ToolCallID: id, Status: acpToolInProgress})
	return true, nil
}

// claim returns the id of the oldest undecided open call with the name and
// input of call. The backend asks approval only for calls that it reported.
func (o *acpObserver) claim(call anthropic.ToolUseBlock) string {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	for index := range o.open {
		open := &o.open[index]
		if !open.claimed && open.name == call.Name && bytes.Equal(open.input, call.Input) {
			open.claimed = true
			return open.id
		}
	}
	panic(fmt.Sprintf("acp: approval for tool %q that the observer did not get", call.Name))
}

func (o *acpObserver) allowedBy(tool string, reply acpReply) bool {
	if reply.err != nil {
		return false
	}
	var result struct {
		Outcome struct {
			Outcome  string        `json:"outcome"`
			OptionID acpPermission `json:"optionId"`
		} `json:"outcome"`
	}
	if json.Unmarshal(reply.result, &result) != nil || result.Outcome.Outcome != "selected" {
		return false
	}
	switch result.Outcome.OptionID {
	case acpAllowOnce:
		return true
	case acpAllowAlways:
		o.session.allowAlways(tool)
		return true
	case acpRejectOnce:
		return false
	default:
		// The client sent an option that the agent did not offer.
		return false
	}
}

// describeACPToolCall gives the fields of a new tool call. RawInput is left
// out when the input is not valid JSON, because it would break the line.
func describeACPToolCall(id, name string, input json.RawMessage) acpToolCallReport {
	title, err := presentation.ToolTitle(name, input)
	if err != nil {
		title = presentation.Safe(name)
	}
	report := acpToolCallReport{ToolCallID: id, Name: name, Title: title, Kind: acpToolKindOf(name), Status: acpToolPending}
	if json.Valid(input) {
		report.RawInput = input
	}
	return report
}

func acpToolKindOf(name string) acpToolKind {
	switch name {
	case "read_file", "list_dir":
		return acpKindRead
	case "glob", "grep":
		return acpKindSearch
	case "edit_file", "write_file":
		return acpKindEdit
	case "bash", "bash_job":
		return acpKindExecute
	case "web_fetch":
		return acpKindFetch
	default:
		// Tool names are an open set: custom tools and MCP tools also occur.
		return acpKindOther
	}
}
