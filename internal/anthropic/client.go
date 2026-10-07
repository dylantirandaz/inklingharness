// Package anthropic is a small streaming client for the Messages API. It is
// hand-written so the harness controls the exact wire format: cache markers,
// thinking round-trips, and unknown block pass-through.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"

	"github.com/dylantirandaz/inklingharness/internal/latency"
)

const (
	apiVersion     = "2023-06-01"
	maxErrorBody   = 1 << 20
	streamSentinel = "[DONE]"
)

// Recorder receives the request body and the raw response stream of each
// call, for replay and debugging.
type Recorder interface {
	// Begin opens one record. requestBody is the request as ordered, shared,
	// read-only segments; their concatenation is the exact body sent. The
	// returned writer receives the raw response bytes as they arrive. Stream
	// closes it when the response ends.
	Begin(requestBody [][]byte) (io.WriteCloser, error)
}

// Client sends Messages API requests.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	recorder   Recorder
}

// NewClient builds a client. recorder may be nil. The HTTP client has no
// timeout because replies stream for minutes; the context controls the
// deadline.
func NewClient(apiKey, baseURL string, recorder Recorder) *Client {
	return &Client{httpClient: &http.Client{}, baseURL: baseURL, apiKey: apiKey, recorder: recorder}
}

// ToolDefinition describes one tool to the model.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is one Messages API call. Stream always sets "stream": true and
// turns on automatic prompt caching.
type Request struct {
	Model     string
	MaxTokens int
	// System is the system prompt. Empty means no system field.
	System string
	// Messages are sent by reusing their stored bytes; they are not encoded
	// again for each request.
	Messages []EncodedMessage
	Tools    []ToolDefinition
	// Thinking is the raw "thinking" request field. nil means omit the field,
	// so the model default applies.
	Thinking json.RawMessage
	// Extra holds top-level request fields that this client does not model,
	// for example "effort". Every client-managed key is reserved, even if omitted.
	Extra map[string]json.RawMessage
}

// Usage counts the tokens of one reply.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// Add returns the sum of two usages.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:              u.InputTokens + other.InputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens + other.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens + other.CacheReadInputTokens,
		OutputTokens:             u.OutputTokens + other.OutputTokens,
	}
}

// StopReason tells why the model stopped.
type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopStopSequence StopReason = "stop_sequence"
	StopToolUse      StopReason = "tool_use"
	StopPauseTurn    StopReason = "pause_turn"
	StopRefusal      StopReason = "refusal"
)

// Response is one complete reply.
type Response struct {
	Message    Message
	StopReason StopReason
	Usage      Usage
	Model      string
}

// APIError is a non-200 HTTP reply.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic: HTTP %d: %s", e.StatusCode, e.Body)
}

// StreamError is an error event that the server sent inside the stream.
type StreamError struct {
	Type    string
	Message string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("anthropic: stream error %s: %s", e.Type, e.Message)
}

// requestTail holds the fields after "messages", in wire order.
type requestTail struct {
	Stream       bool             `json:"stream"`
	CacheControl cacheControl     `json:"cache_control"`
	System       string           `json:"system,omitempty"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
	Thinking     *json.RawMessage `json:"thinking,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// requestBody is one JSON request as shared, read-only segments. Message
// segments point at stored history, so building a body copies no history.
type requestBody struct {
	segments [][]byte
	size     int64
}

var messageSeparator = []byte{','}

// reader returns a new reader over the same segments; each retry uses its own.
func (b requestBody) reader() io.Reader {
	buffers := net.Buffers(slices.Clone(b.segments))
	return &buffers
}

func (r Request) body() (requestBody, error) {
	for key := range r.Extra {
		switch key {
		case "model", "max_tokens", "messages", "stream", "cache_control", "system", "tools", "thinking":
			return requestBody{}, fmt.Errorf("anthropic: extra field %q collides with a client-managed field", key)
		}
	}
	head, err := json.Marshal(struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}{Model: r.Model, MaxTokens: r.MaxTokens})
	if err != nil {
		return requestBody{}, err
	}
	head = append(head[:len(head)-1], `,"messages":[`...)
	tailFields := requestTail{
		Stream: true,
		// Automatic caching moves the breakpoint as the conversation grows.
		CacheControl: cacheControl{Type: "ephemeral"},
		System:       r.System, Tools: r.Tools,
	}
	if r.Thinking != nil {
		tailFields.Thinking = &r.Thinking
	}
	encodedTail, err := json.Marshal(tailFields)
	if err != nil {
		return requestBody{}, err
	}
	tail := make([]byte, 0, len(encodedTail)+1)
	tail = append(tail, ']', ',')
	tail = append(tail, encodedTail[1:]...)
	if len(r.Extra) > 0 {
		extra, err := json.Marshal(r.Extra)
		if err != nil {
			return requestBody{}, fmt.Errorf("anthropic: encode extra fields: %w", err)
		}
		// Both encoders produced JSON objects. Join their fields without
		// decoding either object.
		tail[len(tail)-1] = ','
		tail = append(tail, extra[1:]...)
	}
	body := requestBody{segments: make([][]byte, 0, 2*len(r.Messages)+1)}
	body.segments = append(body.segments, head)
	body.size = int64(len(head) + len(tail))
	for index, message := range r.Messages {
		if message.wire == nil {
			return requestBody{}, fmt.Errorf("anthropic: message %d is empty", index)
		}
		if index > 0 {
			body.segments = append(body.segments, messageSeparator)
			body.size++
		}
		body.segments = append(body.segments, message.wire)
		body.size += int64(len(message.wire))
	}
	body.segments = append(body.segments, tail)
	return body, nil
}

// Stream sends one request and streams the reply. Retryable HTTP statuses get
// up to five attempts, only before a successful response starts. It calls observe
// for each live event, in order, on the calling goroutine. It returns the complete
// reply when the stream ends.
func (c *Client) Stream(ctx context.Context, request Request, observe func(StreamEvent) error) (response *Response, err error) {
	ctx, timing := latency.StartRequest(ctx)
	defer func() { timing.End(err) }()
	encoding := latency.Begin(ctx, latency.Encoding, "messages")
	body, err := request.body()
	encoding.End(err)
	if err != nil {
		return nil, err
	}
	readMeter := latency.NewMeter(ctx, latency.StreamRead, "stream")
	decodeMeter := latency.NewMeter(ctx, latency.StreamDecode, "stream")
	observerMeter := latency.NewMeter(ctx, latency.Observer, "callback")
	defer readMeter.End()
	defer decodeMeter.End()
	defer observerMeter.End()
	if timing != nil && observe != nil {
		originalObserve := observe
		useful := false
		observe = func(event StreamEvent) error {
			if !useful {
				switch event := event.(type) {
				case TextDelta:
					useful = event.Text != ""
				case ThinkingDelta:
					useful = event.Thinking != ""
				case ToolUseStart:
					useful = true
				case BlockStop:
					switch block := event.Block.(type) {
					case TextBlock:
						useful = block.Text != ""
					case ThinkingBlock:
						useful = block.Thinking != ""
					case ToolUseBlock, RedactedThinkingBlock:
						useful = true
					}
				}
				if useful {
					timing.Mark(latency.FirstUseful)
				}
			}
			started := observerMeter.Start()
			eventErr := originalObserve(event)
			observerMeter.Add(started, eventErr)
			return eventErr
		}
	}
	httpResponse, err := c.sendWithRetries(ctx, body, observe)
	if err != nil {
		return nil, err
	}
	defer httpResponse.Body.Close()

	var stream io.Reader = httpResponse.Body
	if c.recorder != nil {
		record, err := c.recorder.Begin(body.segments)
		if err != nil {
			return nil, fmt.Errorf("anthropic: begin record: %w", err)
		}
		defer record.Close()
		stream = io.TeeReader(httpResponse.Body, record)
	}

	scanner := newEventScanner(stream)
	builder := newResponseBuilder()
	builder.timing = timing
	for {
		started := readMeter.Start()
		event, err := scanner.next()
		readErr := err
		if readErr == io.EOF {
			readErr = nil
		}
		readMeter.Add(started, readErr)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("anthropic: read stream: %w", err)
		}
		// Gateways such as OpenRouter end the stream with an OpenAI-style
		// sentinel after message_stop.
		if string(event.Data) == streamSentinel {
			break
		}
		started = decodeMeter.Start()
		observed := observerMeter.Total()
		err = builder.apply(event, observe)
		decodeMeter.AddExcluding(started, observerMeter.Total()-observed, err)
		if err != nil {
			return nil, err
		}
	}
	return builder.response()
}
