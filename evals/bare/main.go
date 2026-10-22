// Command bare is the no-harness baseline of the harness comparisons: the
// model, one bash tool, and a loop. It sends no system prompt and does no
// planning, context management, or recovery. The loop ends when the model
// answers without calling the tool.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// effortLevels are the reasoning efforts that Think also accepts.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

const (
	defaultCommandTimeout = 120 * time.Second
	maxCommandTimeout     = 600 * time.Second
	// pipeDrainDelay bounds the wait for stdout and stderr after the process
	// group is killed, in case a grandchild still holds the pipes.
	pipeDrainDelay = 2 * time.Second
	// A result keeps the start and the end of the output, as Think's bash tool
	// does for a result over 32 KiB, so one noisy command cannot fill the
	// context window.
	outputHeadBytes = 16 << 10
	outputTailBytes = 16 << 10
	// A request is tried this many times when the server is busy or the
	// connection fails before a response, as the Anthropic SDK does by default.
	maxRequestAttempts = 3
	maxRetryDelay      = 8 * time.Second
)

// bashTool matches the bash tool of Think without its background mode.
const bashTool = `{"name":"bash","description":"Run a bash command in the working directory. Returns stdout and stderr, and the exit code when not 0. Timeout: 120 s by default, 600 at most.","input_schema":{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600}},"required":["command"]}}`

type message struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// outputConfig is the Messages field that carries the reasoning effort.
type outputConfig struct {
	Effort string `json:"effort"`
}

type request struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	Stream       bool              `json:"stream"`
	Tools        []json.RawMessage `json:"tools"`
	Messages     []message         `json:"messages"`
	OutputConfig *outputConfig     `json:"output_config,omitempty"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// toolCall is one tool_use block of a reply. input is nil when the model sent
// no valid JSON object, which happens when a reply stops at the output cap.
type toolCall struct {
	id, name string
	input    json.RawMessage
}

// reply is one assistant turn: the blocks to send back in the next request and
// the tool calls among them.
type reply struct {
	content    []json.RawMessage
	calls      []toolCall
	stopReason string
	usage      json.RawMessage
}

type blockState struct {
	kind      string
	start     json.RawMessage
	text      strings.Builder
	signature strings.Builder
	input     strings.Builder
}

type streamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// readReply decodes one Messages stream. Every content block comes back as the
// server sent it, so reasoning and its signatures return unchanged.
func readReply(body io.Reader) (reply, error) {
	var (
		out     reply
		blocks  []*blockState
		data    []string
		stopped bool
	)
	dispatch := func(payload string) error {
		var event streamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("decode stream event: %w", err)
		}
		switch event.Type {
		case "content_block_start":
			if event.Index < 0 {
				return fmt.Errorf("content block index %d", event.Index)
			}
			var start struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			}
			if err := json.Unmarshal(event.ContentBlock, &start); err != nil {
				return fmt.Errorf("decode content block: %w", err)
			}
			for len(blocks) <= event.Index {
				blocks = append(blocks, nil)
			}
			block := &blockState{kind: start.Type, start: event.ContentBlock}
			block.text.WriteString(start.Text + start.Thinking)
			block.signature.WriteString(start.Signature)
			blocks[event.Index] = block
		case "content_block_delta":
			if event.Index < 0 || event.Index >= len(blocks) || blocks[event.Index] == nil {
				return fmt.Errorf("delta for unknown content block %d", event.Index)
			}
			block := blocks[event.Index]
			switch event.Delta.Type {
			case "text_delta":
				block.text.WriteString(event.Delta.Text)
			case "thinking_delta":
				block.text.WriteString(event.Delta.Thinking)
			case "signature_delta":
				block.signature.WriteString(event.Delta.Signature)
			case "input_json_delta":
				block.input.WriteString(event.Delta.PartialJSON)
			}
		case "message_delta":
			if event.Delta.StopReason != "" {
				out.stopReason = event.Delta.StopReason
			}
			if len(event.Usage) > 0 {
				out.usage = event.Usage
			}
		case "message_stop":
			stopped = true
		case "error":
			return fmt.Errorf("stream error %s: %s", event.Error.Type, event.Error.Message)
		}
		return nil
	}

	reader := bufio.NewReaderSize(body, 64<<10)
	// stray holds the start of any line that is not part of a stream, such as
	// a JSON error body that came with status 200, for the error message.
	stray := ""
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if len(data) > 0 {
				payload := strings.Join(data, "\n")
				data = data[:0]
				if payload != "[DONE]" {
					if err := dispatch(payload); err != nil {
						return reply{}, err
					}
				}
			}
		case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, ":"):
			// Event names and comments, such as OpenRouter's keep-alive, carry
			// nothing this loop needs.
		default:
			if len(stray) < 512 {
				stray += line
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return reply{}, err
			}
			break
		}
	}
	if len(data) > 0 {
		if payload := strings.Join(data, "\n"); payload != "[DONE]" {
			if err := dispatch(payload); err != nil {
				return reply{}, err
			}
		}
	}
	if !stopped {
		if stray != "" {
			return reply{}, fmt.Errorf("the stream ended before message_stop: %s", stray)
		}
		return reply{}, errors.New("the stream ended before message_stop")
	}

	for _, block := range blocks {
		if block == nil {
			continue
		}
		switch block.kind {
		case "text":
			if text := block.text.String(); text != "" {
				out.content = append(out.content, mustMarshal(textBlock{"text", text}))
			}
		case "thinking":
			if text, signature := block.text.String(), block.signature.String(); text != "" || signature != "" {
				out.content = append(out.content, mustMarshal(thinkingBlock{"thinking", text, signature}))
			}
		case "tool_use":
			call, err := finishToolUse(block)
			if err != nil {
				return reply{}, err
			}
			echoed := call.input
			if echoed == nil {
				echoed = json.RawMessage("{}")
			}
			out.content = append(out.content, mustMarshal(toolUseBlock{"tool_use", call.id, call.name, echoed}))
			out.calls = append(out.calls, call)
		default:
			// Redacted reasoning and block kinds this loop does not know go back
			// as they came.
			out.content = append(out.content, block.start)
		}
	}
	return out, nil
}

func finishToolUse(block *blockState) (toolCall, error) {
	var start struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(block.start, &start); err != nil {
		return toolCall{}, fmt.Errorf("decode tool_use block: %w", err)
	}
	call := toolCall{id: start.ID, name: start.Name}
	input := strings.TrimSpace(block.input.String())
	if input == "" {
		input = strings.TrimSpace(string(start.Input))
	}
	if input == "" || input == "null" {
		input = "{}"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &object) == nil && object != nil {
		call.input = json.RawMessage(input)
	}
	return call, nil
}

func mustMarshal(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

type toolResult struct {
	content string
	isError bool
}

// outputCollector keeps the start and the end of a command's output in bounded
// memory and counts what lies between.
type outputCollector struct {
	head    []byte
	tail    []byte
	omitted int64
}

func (o *outputCollector) Write(data []byte) (int, error) {
	written := len(data)
	if room := outputHeadBytes - len(o.head); room > 0 {
		take := min(room, len(data))
		o.head = append(o.head, data[:take]...)
		data = data[take:]
	}
	if len(data) > 0 {
		o.tail = append(o.tail, data...)
		if len(o.tail) > 2*outputTailBytes {
			drop := len(o.tail) - outputTailBytes
			o.omitted += int64(drop)
			o.tail = o.tail[:copy(o.tail, o.tail[drop:])]
		}
	}
	return written, nil
}

func (o *outputCollector) text() string {
	if len(o.tail) > outputTailBytes {
		drop := len(o.tail) - outputTailBytes
		o.omitted += int64(drop)
		o.tail = o.tail[drop:]
	}
	if o.omitted == 0 {
		return string(o.head) + string(o.tail)
	}
	return string(o.head) + "\n[" + strconv.FormatInt(o.omitted, 10) + " bytes omitted]\n" + string(o.tail)
}

func invalidInput(err error) toolResult {
	return toolResult{content: "invalid input: " + err.Error(), isError: true}
}

// runBash runs one command in its own process group, so a timeout kills the
// background children too.
func runBash(ctx context.Context, input json.RawMessage) toolResult {
	var arguments struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &arguments); err != nil {
		return invalidInput(err)
	}
	if arguments.Command == "" {
		return invalidInput(errors.New("command is required"))
	}
	timeout := defaultCommandTimeout
	if arguments.TimeoutSeconds != 0 {
		if arguments.TimeoutSeconds < 1 || time.Duration(arguments.TimeoutSeconds)*time.Second > maxCommandTimeout {
			return invalidInput(fmt.Errorf("timeout_seconds must be between 1 and %d", int(maxCommandTimeout.Seconds())))
		}
		timeout = time.Duration(arguments.TimeoutSeconds) * time.Second
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runContext, "/bin/bash", "-c", arguments.Command)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = pipeDrainDelay
	var output outputCollector
	command.Stdout, command.Stderr = &output, &output

	runErr := command.Run()
	content := output.text()
	if errors.Is(runContext.Err(), context.DeadlineExceeded) {
		return toolResult{content: content + fmt.Sprintf("\n[timed out after %s]", timeout), isError: true}
	}
	exitCode := 0
	if runErr != nil {
		var exitError *exec.ExitError
		if !errors.As(runErr, &exitError) {
			return toolResult{content: "bash: " + runErr.Error(), isError: true}
		}
		exitCode = exitError.ExitCode()
	}
	if content == "" {
		content = "(no output)"
	}
	if exitCode != 0 {
		return toolResult{content: content + fmt.Sprintf("\n[exit code %d]", exitCode), isError: true}
	}
	return toolResult{content: content}
}

type agent struct {
	url       string
	key       string
	model     string
	effort    string
	maxTokens int
	maxTurns  int
	client    *http.Client
	events    io.Writer
	sleep     func(time.Duration)
}

func (a *agent) log(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(a.events, "%s\n", encoded)
}

type statusError struct {
	status int
	body   string
}

func (e statusError) Error() string {
	return fmt.Sprintf("the server answered %d: %s", e.status, e.body)
}

func retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests || status >= 500
}

// open sends one request and returns its streaming response. It retries when
// the connection fails or the server is busy, never after a stream starts.
func (a *agent) open(ctx context.Context, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRequestAttempts; attempt++ {
		if attempt > 1 {
			a.sleep(min(time.Duration(1<<(attempt-2))*time.Second, maxRetryDelay))
		}
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "text/event-stream")
		httpRequest.Header.Set("Authorization", "Bearer "+a.key)
		httpRequest.Header.Set("Anthropic-Version", "2023-06-01")
		response, err := a.client.Do(httpRequest)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if response.StatusCode == http.StatusOK {
			return response, nil
		}
		excerpt, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		response.Body.Close()
		lastErr = statusError{status: response.StatusCode, body: strings.TrimSpace(string(excerpt))}
		if !retryable(response.StatusCode) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (a *agent) next(ctx context.Context, messages []message) (reply, error) {
	payload := request{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Stream:    true,
		Tools:     []json.RawMessage{json.RawMessage(bashTool)},
		Messages:  messages,
	}
	if a.effort != "" {
		payload.OutputConfig = &outputConfig{Effort: a.effort}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return reply{}, err
	}
	response, err := a.open(ctx, body)
	if err != nil {
		return reply{}, err
	}
	defer response.Body.Close()
	return readReply(response.Body)
}

func (a *agent) call(ctx context.Context, call toolCall) toolResult {
	switch {
	case call.name != "bash":
		return toolResult{content: fmt.Sprintf("unknown tool %q", call.name), isError: true}
	case call.input == nil:
		return invalidInput(errors.New("the tool input is not a JSON object"))
	default:
		return runBash(ctx, call.input)
	}
}

// run is the whole loop. It returns nil when the model stops calling the tool.
func (a *agent) run(ctx context.Context, instruction string) error {
	messages := []message{{Role: "user", Content: []json.RawMessage{mustMarshal(textBlock{"text", instruction})}}}
	for turn := 1; turn <= a.maxTurns; turn++ {
		reply, err := a.next(ctx, messages)
		if err != nil {
			a.log(map[string]any{"event": "error", "turn": turn, "error": err.Error()})
			return fmt.Errorf("turn %d: %w", turn, err)
		}
		a.log(map[string]any{"event": "turn", "turn": turn, "stop_reason": reply.stopReason, "tool_calls": len(reply.calls), "usage": reply.usage})
		if len(reply.calls) == 0 {
			a.log(map[string]any{"event": "end", "reason": "no tool call", "turns": turn, "stop_reason": reply.stopReason})
			return nil
		}
		messages = append(messages, message{Role: "assistant", Content: reply.content})
		results := make([]json.RawMessage, 0, len(reply.calls))
		for _, call := range reply.calls {
			started := time.Now()
			result := a.call(ctx, call)
			if err := ctx.Err(); err != nil {
				return err
			}
			// The audit of a run needs to see what the model ran and what came back.
			a.log(map[string]any{"event": "tool", "turn": turn, "seconds": time.Since(started).Seconds(), "bytes": len(result.content), "error": result.isError, "input": call.input, "output": result.content})
			results = append(results, mustMarshal(toolResultBlock{"tool_result", call.id, result.content, result.isError}))
		}
		messages = append(messages, message{Role: "user", Content: results})
	}
	a.log(map[string]any{"event": "end", "reason": "turn limit", "turns": a.maxTurns})
	return nil
}

func run(arguments []string, events io.Writer) error {
	flags := flag.NewFlagSet("bare", flag.ContinueOnError)
	baseURL := flags.String("base-url", "", "the Messages API base, without /v1/messages")
	model := flags.String("model", "", "the model to request")
	maxTokens := flags.Int("max-tokens", 16384, "the output token cap of each request")
	effort := flags.String("effort", "", "reasoning effort, sent as output_config.effort: "+strings.Join(effortLevels, ", ")+"; empty sends none")
	maxTurns := flags.Int("max-turns", 10000, "the most requests of one run")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *baseURL == "" || *model == "" || *maxTokens < 1 || *maxTurns < 1 || flags.NArg() != 1 {
		return errors.New("usage: bare -base-url URL -model MODEL [-effort LEVEL] [-max-tokens N] [-max-turns N] -- INSTRUCTION")
	}
	if *effort != "" && !slices.Contains(effortLevels, *effort) {
		return fmt.Errorf("effort must be one of %s, not %q", strings.Join(effortLevels, ", "), *effort)
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return errors.New("OPENROUTER_API_KEY is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return errors.New("the default HTTP transport has an unexpected type")
	}
	transport = transport.Clone()
	transport.DisableCompression = true
	a := &agent{
		url:       strings.TrimSuffix(*baseURL, "/"),
		key:       key,
		model:     *model,
		effort:    *effort,
		maxTokens: *maxTokens,
		maxTurns:  *maxTurns,
		client:    &http.Client{Transport: transport},
		events:    events,
		sleep:     time.Sleep,
	}
	return a.run(ctx, flags.Arg(0))
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bare:", err)
		os.Exit(1)
	}
}
