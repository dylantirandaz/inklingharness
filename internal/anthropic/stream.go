package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/latency"
)

// StreamEvent is a live notification from a streaming reply. The set of
// implementations is closed to this package: TextDelta, ThinkingDelta,
// ToolUseStart, BlockStop, UnknownEvent, and RetryEvent.
type StreamEvent interface {
	streamEvent()
}

// RetryEvent announces a wait before another HTTP attempt. Attempt is the next
// attempt number (2 through 5), not the number of retries already performed.
type RetryEvent struct {
	Attempt int
	Delay   time.Duration
	Reason  string
}

// TextDelta is a piece of reply text.
type TextDelta struct {
	Text string
}

// ThinkingDelta is a piece of visible reasoning text.
type ThinkingDelta struct {
	Thinking string
}

// ToolUseStart tells that the model started a tool call. The input follows in
// the BlockStop event for the same index.
type ToolUseStart struct {
	Index int
	ID    string
	Name  string
}

// BlockStop carries one complete content block.
type BlockStop struct {
	Index int
	Block ContentBlock
}

// UnknownEvent is a stream event type that this client does not know. The
// client skips it and reports it, so an additive server change is visible
// but does not stop the reply.
type UnknownEvent struct {
	Type string
	Data string
}

func (TextDelta) streamEvent()     {}
func (ThinkingDelta) streamEvent() {}
func (ToolUseStart) streamEvent()  {}
func (BlockStop) streamEvent()     {}
func (UnknownEvent) streamEvent()  {}
func (RetryEvent) streamEvent()    {}

type wireEvent struct {
	Type         string            `json:"type"`
	Index        int               `json:"index"`
	Message      *wireMessageStart `json:"message"`
	ContentBlock json.RawMessage   `json:"content_block"`
	Delta        json.RawMessage   `json:"delta"`
	Usage        *Usage            `json:"usage"`
	Error        *wireError        `json:"error"`
}

type wireMessageStart struct {
	Model string `json:"model"`
	Usage Usage  `json:"usage"`
}

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type wireContentBlockStart struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Data string `json:"data"`
}

type wireContentBlockDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
}

type wireMessageDelta struct {
	StopReason string `json:"stop_reason"`
}

const (
	blockKindText             = "text"
	blockKindThinking         = "thinking"
	blockKindRedactedThinking = "redacted_thinking"
	blockKindToolUse          = "tool_use"
)

// blockBuilder accumulates the deltas of one content block.
type blockBuilder struct {
	kind      string
	id        string
	name      string
	content   strings.Builder
	signature strings.Builder
	data      string
	raw       json.RawMessage
}

// responseBuilder turns the event sequence of one reply into a Response.
type responseBuilder struct {
	open           map[int]*blockBuilder
	done           map[int]ContentBlock
	model          string
	usage          Usage
	stopReason     StopReason
	sawMessageStop bool
	timing         *latency.Span
}

func newResponseBuilder() *responseBuilder {
	return &responseBuilder{open: map[int]*blockBuilder{}, done: map[int]ContentBlock{}}
}

func (b *responseBuilder) apply(event serverSentEvent, observe func(StreamEvent) error) error {
	var wire wireEvent
	if err := json.Unmarshal(event.Data, &wire); err != nil {
		return fmt.Errorf("anthropic: decode %q event: %w", event.Name, err)
	}
	if b.timing != nil && (wire.Type == "message_start" || wire.Type == "message_delta") {
		// Only usage-bearing events need a second, presence-preserving decode.
		// The normal protocol decoder and public token accounting stay unchanged.
		var counters struct {
			Message *struct {
				Usage latency.Tokens `json:"usage"`
			} `json:"message"`
			Usage latency.Tokens `json:"usage"`
		}
		if json.Unmarshal(event.Data, &counters) == nil {
			if counters.Message != nil {
				b.timing.Usage(counters.Message.Usage)
			}
			b.timing.Usage(counters.Usage)
		}
	}
	switch wire.Type {
	case "message_start":
		if wire.Message == nil {
			return fmt.Errorf("anthropic: message_start without message")
		}
		b.model = wire.Message.Model
		b.usage = wire.Message.Usage
		return nil
	case "content_block_start":
		return b.startBlock(wire, observe)
	case "content_block_delta":
		return b.applyDelta(wire, observe)
	case "content_block_stop":
		return b.stopBlock(wire.Index, observe)
	case "message_delta":
		var delta wireMessageDelta
		if err := json.Unmarshal(wire.Delta, &delta); err != nil {
			return fmt.Errorf("anthropic: decode message_delta: %w", err)
		}
		if delta.StopReason != "" {
			b.stopReason = StopReason(delta.StopReason)
		}
		if wire.Usage != nil {
			b.usage = mergeUsage(b.usage, *wire.Usage)
		}
		return nil
	case "message_stop":
		b.sawMessageStop = true
		return nil
	case "ping":
		return nil
	case "error":
		if wire.Error == nil {
			return fmt.Errorf("anthropic: error event without error body")
		}
		return &StreamError{Type: wire.Error.Type, Message: wire.Error.Message}
	}
	return observe(UnknownEvent{Type: wire.Type, Data: string(event.Data)})
}

func (b *responseBuilder) startBlock(wire wireEvent, observe func(StreamEvent) error) error {
	if _, exists := b.open[wire.Index]; exists {
		return fmt.Errorf("anthropic: content block %d started twice", wire.Index)
	}
	var start wireContentBlockStart
	if err := json.Unmarshal(wire.ContentBlock, &start); err != nil {
		return fmt.Errorf("anthropic: decode content_block_start: %w", err)
	}
	builder := &blockBuilder{kind: start.Type, id: start.ID, name: start.Name, data: start.Data, raw: wire.ContentBlock}
	b.open[wire.Index] = builder
	if start.Type == blockKindToolUse {
		return observe(ToolUseStart{Index: wire.Index, ID: start.ID, Name: start.Name})
	}
	return nil
}

func (b *responseBuilder) applyDelta(wire wireEvent, observe func(StreamEvent) error) error {
	builder, exists := b.open[wire.Index]
	if !exists {
		return fmt.Errorf("anthropic: delta for content block %d that is not open", wire.Index)
	}
	var delta wireContentBlockDelta
	if err := json.Unmarshal(wire.Delta, &delta); err != nil {
		return fmt.Errorf("anthropic: decode content_block_delta: %w", err)
	}
	switch delta.Type {
	case "text_delta":
		if builder.kind != blockKindText {
			return fmt.Errorf("anthropic: text_delta on %q block %d", builder.kind, wire.Index)
		}
		builder.content.WriteString(delta.Text)
		return observe(TextDelta{Text: delta.Text})
	case "thinking_delta":
		if builder.kind != blockKindThinking {
			return fmt.Errorf("anthropic: thinking_delta on %q block %d", builder.kind, wire.Index)
		}
		builder.content.WriteString(delta.Thinking)
		return observe(ThinkingDelta{Thinking: delta.Thinking})
	case "signature_delta":
		if builder.kind != blockKindThinking {
			return fmt.Errorf("anthropic: signature_delta on %q block %d", builder.kind, wire.Index)
		}
		builder.signature.WriteString(delta.Signature)
		return nil
	case "input_json_delta":
		if builder.kind != blockKindToolUse {
			return fmt.Errorf("anthropic: input_json_delta on %q block %d", builder.kind, wire.Index)
		}
		builder.content.WriteString(delta.PartialJSON)
		return nil
	}
	return fmt.Errorf("anthropic: unknown delta type %q on block %d", delta.Type, wire.Index)
}

func (b *responseBuilder) stopBlock(index int, observe func(StreamEvent) error) error {
	builder, exists := b.open[index]
	if !exists {
		return fmt.Errorf("anthropic: stop for content block %d that is not open", index)
	}
	delete(b.open, index)
	block, err := builder.finish()
	if err != nil {
		return fmt.Errorf("anthropic: content block %d: %w", index, err)
	}
	b.done[index] = block
	return observe(BlockStop{Index: index, Block: block})
}

func (builder *blockBuilder) finish() (ContentBlock, error) {
	switch builder.kind {
	case blockKindText:
		return TextBlock{Text: builder.content.String()}, nil
	case blockKindThinking:
		return ThinkingBlock{Thinking: builder.content.String(), Signature: builder.signature.String()}, nil
	case blockKindRedactedThinking:
		return RedactedThinkingBlock{Data: builder.data}, nil
	case blockKindToolUse:
		input := builder.content.String()
		if input == "" {
			input = "{}"
		}
		if !json.Valid([]byte(input)) {
			return nil, fmt.Errorf("tool_use %q input is not valid JSON: %q", builder.name, input)
		}
		return ToolUseBlock{ID: builder.id, Name: builder.name, Input: json.RawMessage(input)}, nil
	}
	return OpaqueBlock{Raw: builder.raw}, nil
}

func (b *responseBuilder) response() (*Response, error) {
	if !b.sawMessageStop {
		return nil, fmt.Errorf("anthropic: stream ended before message_stop")
	}
	if b.stopReason == "" {
		return nil, fmt.Errorf("anthropic: stream ended without a stop reason")
	}
	if len(b.open) > 0 {
		return nil, fmt.Errorf("anthropic: %d content blocks never stopped", len(b.open))
	}
	content := make([]ContentBlock, len(b.done))
	for index := range content {
		block, exists := b.done[index]
		if !exists {
			return nil, fmt.Errorf("anthropic: content block %d missing from stream", index)
		}
		content[index] = block
	}
	return &Response{
		Message:    Message{Role: RoleAssistant, Content: content},
		StopReason: b.stopReason,
		Usage:      b.usage,
		Model:      b.model,
	}, nil
}

// mergeUsage applies the cumulative counters of a message_delta. A zero
// counter means "not reported", so it keeps the earlier value.
func mergeUsage(base, update Usage) Usage {
	merged := base
	if update.InputTokens > 0 {
		merged.InputTokens = update.InputTokens
	}
	if update.CacheCreationInputTokens > 0 {
		merged.CacheCreationInputTokens = update.CacheCreationInputTokens
	}
	if update.CacheReadInputTokens > 0 {
		merged.CacheReadInputTokens = update.CacheReadInputTokens
	}
	if update.OutputTokens > 0 {
		merged.OutputTokens = update.OutputTokens
	}
	return merged
}
