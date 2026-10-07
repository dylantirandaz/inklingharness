// Package agent runs the model and tool loop.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/latency"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// Config holds settings for one user request. A nil Approve denies file changes
// and commands. Checkpoint receives only complete messages and paired tool results.
type Config struct {
	Model         string
	MaxTokens     int
	System        string
	Thinking      json.RawMessage
	Extra         map[string]json.RawMessage
	MaxTurns      int
	CompactTokens int
	EnableTasks   bool
	Approve       func(context.Context, anthropic.ToolUseBlock) (bool, error)
	Checkpoint    func(Outcome) error
	// Gate runs before every tool call, read-only ones too, and before
	// Approve. A refusal goes to the model as a failed result with its
	// reason; an error stops the run. A nil Gate refuses nothing. Gate must
	// be safe for concurrent calls, because read-only batches run in
	// parallel.
	Gate func(context.Context, anthropic.ToolUseBlock) (*Refusal, error)
	// AfterTool sees the result of every tool that ran and returns text for
	// the model, such as hook output; the text goes after the result. An
	// error stops the run, so a broken hook is never silent. A nil AfterTool
	// does nothing.
	AfterTool func(context.Context, anthropic.ToolUseBlock, tools.Result) (string, error)
	// Notices returns news that arrived outside the conversation, such as a
	// background job that ended. Each notice goes to the model once, with
	// the next tool results or prompt. A nil Notices has none.
	Notices func() []string
}

// Refusal is why a Gate stops one tool call.
type Refusal struct {
	Reason string
}

// Observer methods run on the caller's goroutine, including parallel tool results.
type Observer interface {
	Text(string)
	Thinking(string)
	ToolCallStart(string)
	ToolCall(string, json.RawMessage)
	ToolResult(string, tools.Result, time.Duration)
	TurnDone(int, anthropic.Usage, anthropic.StopReason, time.Duration)
	UnknownEvent(string)
	Status(string)
}

// SilentObserver discards display events, not failures or usage.
type SilentObserver struct{}

func (SilentObserver) Text(string)                                                        {}
func (SilentObserver) Thinking(string)                                                    {}
func (SilentObserver) ToolCallStart(string)                                               {}
func (SilentObserver) ToolCall(string, json.RawMessage)                                   {}
func (SilentObserver) ToolResult(string, tools.Result, time.Duration)                     {}
func (SilentObserver) TurnDone(int, anthropic.Usage, anthropic.StopReason, time.Duration) {}
func (SilentObserver) UnknownEvent(string)                                                {}
func (SilentObserver) Status(string)                                                      {}

// Outcome is also returned on failure. Messages never contain partial streams or
// unpaired tool calls. Usage includes completed compaction and child requests.
type Outcome struct {
	Messages      []anthropic.EncodedMessage
	Turns         int
	Usage         anthropic.Usage
	TurnLatencies []time.Duration
	FinalText     string
	// LastInputTokens is the input, cache-read, and cache-creation token count
	// of the latest completed model request. It is 0 when no request completed
	// or after compaction.
	LastInputTokens int
}

// Prompt is one user request: its text and the images attached to it.
type Prompt struct {
	Text   string
	Images []anthropic.ImageBlock
}

// maxCutReplies is how many replies cut at max_tokens one prompt may retry.
// A cut reply usually comes from reasoning that ran too long; a model that
// keeps running long must stop, so the retries are few.
const maxCutReplies = 2

const cutReplyNote = "Your last reply hit the output limit before it was complete, so it was discarded. Reason less, and take one small step at a time."

// Run adds a user prompt to history and runs until the model stops or fails.
// It does not change the history slice; the outcome holds the new history.
// Completed tool effects are not repeated on resume.
//
// The provider bills reasoning in history as input, and the model does not
// need the reasoning of earlier prompts, so the outcome history drops the
// reasoning of every assistant message before the last one.
func Run(ctx context.Context, client *anthropic.Client, config Config, toolSet *tools.Set, history []anthropic.EncodedMessage, prompt Prompt, observer Observer) (*Outcome, error) {
	earlier, err := withoutOldReasoning(history)
	if err != nil {
		return &Outcome{Messages: slices.Clone(history)}, fmt.Errorf("agent: %w", err)
	}
	outcome := &Outcome{Messages: earlier}
	if config.MaxTurns <= 0 || config.MaxTokens <= 0 || config.CompactTokens < 0 || strings.TrimSpace(prompt.Text) == "" {
		return outcome, errors.New("agent: positive turn/token limits, a nonnegative compaction limit, and a prompt are required")
	}
	content := []anthropic.ContentBlock{anthropic.TextBlock{Text: withNotices(prompt.Text, config)}}
	for _, image := range prompt.Images {
		content = append(content, image)
	}
	withPrompt, err := appendMessage(outcome.Messages, anthropic.Message{Role: anthropic.RoleUser, Content: content})
	if err != nil {
		return outcome, fmt.Errorf("agent: add prompt: %w", err)
	}
	outcome.Messages = withPrompt
	if err := checkpoint(ctx, config, outcome); err != nil {
		return outcome, err
	}
	definitions := RequestTools(config, toolSet)
	cutReplies := 0
	for turn := 1; turn <= config.MaxTurns; turn++ {
		if ShouldCompact(config, outcome.Messages, outcome.LastInputTokens) {
			compacted, err := Compact(ctx, client, config, toolSet, outcome.Messages, observer)
			outcome.Usage = outcome.Usage.Add(compacted.Usage)
			if err != nil {
				return outcome, err
			}
			outcome.Messages = compacted.Messages
			outcome.LastInputTokens = 0
			if err := checkpoint(ctx, config, outcome); err != nil {
				return outcome, err
			}
		}
		requestContext := latency.RequestContext(ctx)
		started := time.Now()
		response, err := client.Stream(requestContext, anthropic.Request{Model: config.Model, MaxTokens: config.MaxTokens, System: config.System, Messages: outcome.Messages, Tools: definitions, Thinking: config.Thinking, Extra: config.Extra}, func(event anthropic.StreamEvent) error { return forward(event, observer) })
		if err != nil {
			return outcome, fmt.Errorf("turn %d: %w", turn, err)
		}
		elapsed := time.Since(started)
		outcome.Turns++
		outcome.TurnLatencies = append(outcome.TurnLatencies, elapsed)
		outcome.Usage = outcome.Usage.Add(response.Usage)
		outcome.LastInputTokens = response.Usage.InputTokens + response.Usage.CacheReadInputTokens + response.Usage.CacheCreationInputTokens
		display := latency.Begin(requestContext, latency.Observer, "turn_done")
		observer.TurnDone(turn, response.Usage, response.StopReason, elapsed)
		display.End(nil)
		calls := toolCalls(response.Message)
		if response.StopReason == anthropic.StopMaxTokens && cutReplies < maxCutReplies {
			// A reply cut at the limit cannot run its tool calls, and its long
			// reasoning is lost anyway. The note goes after the last user
			// message, so the cached prefix stays valid.
			cutReplies++
			observer.Status("reply hit the output limit; asking for smaller steps")
			noted, err := appendMessage(outcome.Messages, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: cutReplyNote}}})
			if err != nil {
				return outcome, fmt.Errorf("turn %d: %w", turn, err)
			}
			outcome.Messages = noted
			continue
		}
		if len(calls) > 0 && response.StopReason != anthropic.StopToolUse {
			return outcome, fmt.Errorf("turn %d: tool calls with stop reason %q were not executed", turn, response.StopReason)
		}
		if response.StopReason == anthropic.StopToolUse && len(calls) == 0 {
			return outcome, fmt.Errorf("turn %d: tool_use without tool calls", turn)
		}
		completed, err := appendMessage(outcome.Messages, response.Message)
		if err != nil {
			return outcome, fmt.Errorf("turn %d: %w", turn, err)
		}
		var runErr error
		if response.StopReason == anthropic.StopToolUse {
			results, childUsage, err := runTools(requestContext, client, config, toolSet, calls, observer)
			outcome.Usage = outcome.Usage.Add(childUsage)
			runErr = err
			if notices := withNotices("", config); notices != "" {
				results = append(results, anthropic.TextBlock{Text: notices})
			}
			// History keeps a tool call only together with its results.
			completed, err = appendMessage(completed, anthropic.Message{Role: anthropic.RoleUser, Content: results})
			if err != nil {
				return outcome, fmt.Errorf("turn %d: %w", turn, errors.Join(runErr, err))
			}
		}
		outcome.Messages = completed
		if err := checkpoint(requestContext, config, outcome); err != nil {
			return outcome, errors.Join(runErr, err)
		}
		if runErr != nil {
			return outcome, fmt.Errorf("turn %d: %w", turn, runErr)
		}
		switch response.StopReason {
		case anthropic.StopToolUse, anthropic.StopPauseTurn:
		case anthropic.StopEndTurn, anthropic.StopStopSequence:
			outcome.FinalText = finalText(response.Message)
			return outcome, nil
		case anthropic.StopMaxTokens:
			return outcome, fmt.Errorf("turn %d: reply hit max_tokens=%d", turn, config.MaxTokens)
		case anthropic.StopRefusal:
			return outcome, fmt.Errorf("turn %d: model refused the request", turn)
		default:
			return outcome, fmt.Errorf("turn %d: unknown stop reason %q", turn, response.StopReason)
		}
	}
	return outcome, fmt.Errorf("task not complete after %d turns", config.MaxTurns)
}

// withoutOldReasoning returns a copy of history in which no assistant message
// before the last one has thinking or redacted thinking blocks. The last
// assistant message stays whole, because some APIs need its reasoning when
// its tool calls are not answered yet. A message that has only reasoning
// stays whole too, so no message becomes empty.
func withoutOldReasoning(history []anthropic.EncodedMessage) ([]anthropic.EncodedMessage, error) {
	result := slices.Clone(history)
	last := -1
	for index, message := range history {
		if message.Role() == anthropic.RoleAssistant {
			last = index
		}
	}
	for index := range last {
		message := history[index]
		if message.Role() != anthropic.RoleAssistant || !bytes.Contains(message.Wire(), []byte(`thinking"`)) {
			continue
		}
		decoded, err := message.Decode()
		if err != nil {
			return nil, err
		}
		kept := slices.DeleteFunc(slices.Clone(decoded.Content), func(block anthropic.ContentBlock) bool {
			switch block.(type) {
			case anthropic.ThinkingBlock, anthropic.RedactedThinkingBlock:
				return true
			default:
				return false
			}
		})
		if len(kept) == len(decoded.Content) || len(kept) == 0 {
			continue
		}
		encoded, err := anthropic.EncodeMessage(anthropic.Message{Role: decoded.Role, Content: kept})
		if err != nil {
			return nil, err
		}
		result[index] = encoded
	}
	return result, nil
}

func checkpoint(ctx context.Context, config Config, outcome *Outcome) (err error) {
	if config.Checkpoint == nil {
		return nil
	}
	span := latency.Begin(ctx, latency.Checkpoint, "save")
	defer func() { span.End(err) }()
	if err := config.Checkpoint(*outcome); err != nil {
		return fmt.Errorf("save conversation: %w", err)
	}
	return nil
}

// RequestTools returns the tool definitions of every request for this
// configuration. Compaction sends the same list so the cached prefix stays valid.
func RequestTools(config Config, toolSet *tools.Set) []anthropic.ToolDefinition {
	definitions := toolDefinitions(toolSet)
	if config.EnableTasks {
		definitions = append(definitions, taskDefinition())
	}
	return definitions
}

// appendMessage encodes message once and adds it to history. A message with
// the same role as the last one is joined to it, because the API requires
// alternate roles. The join builds a new backing array, so slices that share
// the old array, such as a saved checkpoint, keep their elements.
func appendMessage(messages []anthropic.EncodedMessage, message anthropic.Message) ([]anthropic.EncodedMessage, error) {
	last := len(messages) - 1
	if last < 0 || messages[last].Role() != message.Role {
		encoded, err := anthropic.EncodeMessage(message)
		if err != nil {
			return nil, err
		}
		return append(messages, encoded), nil
	}
	previous, err := messages[last].Decode()
	if err != nil {
		return nil, err
	}
	joined, err := anthropic.EncodeMessage(anthropic.Message{Role: message.Role, Content: slices.Concat(previous.Content, message.Content)})
	if err != nil {
		return nil, err
	}
	return append(messages[:last:last], joined), nil
}

// appendEncoded adds a stored message without encoding it again, unless it
// must be joined to a last message of the same role.
func appendEncoded(messages []anthropic.EncodedMessage, message anthropic.EncodedMessage) ([]anthropic.EncodedMessage, error) {
	if len(messages) == 0 || messages[len(messages)-1].Role() != message.Role() {
		return append(messages, message), nil
	}
	typed, err := message.Decode()
	if err != nil {
		return nil, err
	}
	return appendMessage(messages, typed)
}

func forward(event anthropic.StreamEvent, observer Observer) error {
	switch typed := event.(type) {
	case anthropic.TextDelta:
		observer.Text(typed.Text)
	case anthropic.ThinkingDelta:
		observer.Thinking(typed.Thinking)
	case anthropic.ToolUseStart:
		observer.ToolCallStart(typed.Name)
	case anthropic.BlockStop:
		if call, ok := typed.Block.(anthropic.ToolUseBlock); ok {
			observer.ToolCall(call.Name, call.Input)
		}
	case anthropic.UnknownEvent:
		observer.UnknownEvent(typed.Type)
	case anthropic.RetryEvent:
		observer.Status(fmt.Sprintf("retry %d in %s: %s", typed.Attempt, typed.Delay.Round(time.Millisecond), typed.Reason))
	default:
		return fmt.Errorf("agent: unknown stream event %T", event)
	}
	return nil
}

func toolDefinitions(toolSet *tools.Set) []anthropic.ToolDefinition {
	definitions := make([]anthropic.ToolDefinition, 0, len(toolSet.All()))
	for _, tool := range toolSet.All() {
		definitions = append(definitions, anthropic.ToolDefinition{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return definitions
}

func toolCalls(message anthropic.Message) []anthropic.ToolUseBlock {
	var calls []anthropic.ToolUseBlock
	for _, block := range message.Content {
		if call, ok := block.(anthropic.ToolUseBlock); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

func finalText(message anthropic.Message) string {
	var text strings.Builder
	for _, block := range message.Content {
		if block, ok := block.(anthropic.TextBlock); ok {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

type execution struct {
	result  tools.Result
	elapsed time.Duration
	usage   anthropic.Usage
	err     error
}

// Read batches use at most four workers. Display events stay in call order.
func runTools(ctx context.Context, client *anthropic.Client, config Config, toolSet *tools.Set, calls []anthropic.ToolUseBlock, observer Observer) ([]anthropic.ContentBlock, anthropic.Usage, error) {
	executions := make([]execution, len(calls))
	runOne := func(index int) { executions[index] = runTool(ctx, client, config, toolSet, calls[index]) }
	if allReadOnly(toolSet, calls, config.EnableTasks) {
		var group sync.WaitGroup
		work := make(chan int)
		for range min(4, len(calls)) {
			group.Add(1)
			go func() {
				defer group.Done()
				for index := range work {
					runOne(index)
				}
			}()
		}
		for index := range calls {
			work <- index
		}
		close(work)
		group.Wait()
	} else {
		failed := false
		for index := range calls {
			if failed {
				executions[index] = execution{result: tools.Result{Content: "not run: an earlier tool failed", IsError: true}}
				continue
			}
			runOne(index)
			failed = executions[index].err != nil || executions[index].result.IsError
		}
	}
	results := make([]anthropic.ContentBlock, len(calls))
	var usage anthropic.Usage
	var failures []error
	display := latency.NewMeter(ctx, latency.Observer, "tool_results")
	defer display.End()
	for index, executed := range executions {
		started := display.Start()
		observer.ToolResult(calls[index].Name, executed.result, executed.elapsed)
		display.Add(started, nil)
		results[index] = anthropic.ToolResultBlock{ToolUseID: calls[index].ID, Content: executed.result.Content, IsError: executed.result.IsError}
		usage = usage.Add(executed.usage)
		if executed.err != nil {
			failures = append(failures, executed.err)
		}
	}
	return results, usage, errors.Join(failures...)
}

func allReadOnly(toolSet *tools.Set, calls []anthropic.ToolUseBlock, enableTasks bool) bool {
	for _, call := range calls {
		if enableTasks && call.Name == "task" {
			continue
		}
		tool, found := toolSet.Lookup(call.Name)
		if !found || !tool.ReadOnly {
			return false
		}
	}
	return true
}

func runTool(ctx context.Context, client *anthropic.Client, config Config, toolSet *tools.Set, call anthropic.ToolUseBlock) execution {
	started := time.Now()
	span := latency.Begin(ctx, latency.Tool, call.Name)
	finish := func(result tools.Result, usage anthropic.Usage, err error) execution {
		if span != nil {
			timingErr := err
			if timingErr == nil && result.IsError {
				timingErr = errors.New("tool result failed")
			}
			span.End(timingErr)
		}
		if err != nil {
			result = tools.Result{Content: "tool stopped: " + err.Error() + ". Check file state before another attempt.", IsError: true}
		}
		return execution{result: result, usage: usage, err: err, elapsed: time.Since(started)}
	}
	if err := ctx.Err(); err != nil {
		return finish(tools.Result{}, anthropic.Usage{}, err)
	}
	if config.Gate != nil {
		refusal, err := config.Gate(ctx, call)
		if err != nil {
			return finish(tools.Result{}, anthropic.Usage{}, err)
		}
		if refusal != nil {
			return finish(tools.Result{Content: "Refused: " + refusal.Reason, IsError: true}, anthropic.Usage{}, nil)
		}
	}
	if config.EnableTasks && call.Name == "task" {
		result, usage, err := runTask(ctx, client, config, toolSet, call.Input)
		if err == nil {
			result, err = afterTool(ctx, config, call, result)
		}
		return finish(result, usage, err)
	}
	tool, found := toolSet.Lookup(call.Name)
	if !found {
		return finish(tools.Result{Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}, anthropic.Usage{}, nil)
	}
	// These change only the agent's own state, so they need no approval:
	// bash_job the jobs that an approved bash call started, remember the
	// project memory file, and ask_user asks the user.
	if !tool.ReadOnly && !slices.Contains([]string{"bash_job", "remember", "ask_user"}, call.Name) {
		allowed := false
		var err error
		approval := latency.Begin(ctx, latency.Approval, call.Name)
		if config.Approve != nil {
			allowed, err = config.Approve(ctx, call)
		}
		if approval != nil {
			approvalErr := err
			if approvalErr == nil && !allowed {
				approvalErr = errors.New("approval denied")
			}
			approval.End(approvalErr)
		}
		if err != nil {
			return finish(tools.Result{}, anthropic.Usage{}, err)
		}
		if !allowed {
			return finish(tools.Result{Content: "The user did not approve this operation. Do not try another tool to bypass this decision.", IsError: true}, anthropic.Usage{}, nil)
		}
	}
	if err := ctx.Err(); err != nil {
		return finish(tools.Result{}, anthropic.Usage{}, err)
	}
	result, err := tool.Run(ctx, call.Input)
	if err == nil {
		result, err = afterTool(ctx, config, call, result)
	}
	return finish(result, anthropic.Usage{}, err)
}

// afterTool adds the text of config.AfterTool after the result.
func afterTool(ctx context.Context, config Config, call anthropic.ToolUseBlock, result tools.Result) (tools.Result, error) {
	if config.AfterTool == nil {
		return result, nil
	}
	extra, err := config.AfterTool(ctx, call, result)
	if err != nil {
		return result, fmt.Errorf("after-tool hook for %s: %w", call.Name, err)
	}
	if extra != "" {
		result.Content += "\n\n" + extra
	}
	return result, nil
}

// withNotices adds the pending notices after text. With no notices it
// returns text unchanged.
func withNotices(text string, config Config) string {
	if config.Notices == nil {
		return text
	}
	notices := config.Notices()
	if len(notices) == 0 {
		return text
	}
	block := "Notices since the last message:\n- " + strings.Join(notices, "\n- ")
	if text == "" {
		return block
	}
	return text + "\n\n" + block
}
