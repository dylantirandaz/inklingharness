package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// bytesPerToken matches agent.EstimateTokens, so the parts add up to the
// footer estimate.
const bytesPerToken = 3

// contextPart is one line of /context: a kind of content and its size.
type contextPart struct {
	name  string
	bytes int
	count int
}

// writeContext shows what fills the next request: the system prompt, the
// tool definitions, and the history by kind of content. It is an estimate;
// the provider counts the exact tokens.
func writeContext(output io.Writer, config agent.Config, toolSet *tools.Set, messages []anthropic.EncodedMessage, compactTokens int) error {
	parts, err := contextParts(config, toolSet, messages)
	if err != nil {
		return err
	}
	total := 0
	for _, part := range parts {
		total += part.bytes
	}
	fmt.Fprintf(output, "Context estimate: ~%s tokens", shortCount(total/bytesPerToken))
	if compactTokens > 0 {
		fmt.Fprintf(output, " of %s before compaction (%d%%)", shortCount(compactTokens), total/bytesPerToken*100/compactTokens)
	}
	fmt.Fprintln(output)
	for _, part := range parts {
		if part.bytes == 0 {
			continue
		}
		label := part.name
		if part.count > 0 {
			label = fmt.Sprintf("%s (%d)", part.name, part.count)
		}
		share := 0
		if total > 0 {
			share = part.bytes * 100 / total
		}
		fmt.Fprintf(output, "  %-24s ~%6s tokens  %3d%%\n", label, shortCount(part.bytes/bytesPerToken), share)
	}
	return nil
}

func contextParts(config agent.Config, toolSet *tools.Set, messages []anthropic.EncodedMessage) ([]contextPart, error) {
	definitions := agent.RequestTools(config, toolSet)
	definitionBytes := 0
	for _, definition := range definitions {
		definitionBytes += len(definition.Name) + len(definition.Description) + len(definition.InputSchema)
	}
	parts := []contextPart{
		{name: "system prompt", bytes: len(config.System)},
		{name: "tool definitions", bytes: definitionBytes, count: len(definitions)},
	}
	kinds := map[string]*contextPart{}
	order := []string{"your prompts", "replies", "reasoning", "tool calls", "tool results", "images", "other"}
	for _, name := range order {
		kinds[name] = &contextPart{name: name}
	}
	for _, encoded := range messages {
		message, err := encoded.Decode()
		if err != nil {
			return nil, fmt.Errorf("context: %w", err)
		}
		for _, block := range message.Content {
			name, size := blockSize(message.Role, block)
			kinds[name].bytes += size
			kinds[name].count++
		}
	}
	for _, name := range order {
		parts = append(parts, *kinds[name])
	}
	// Largest first after the fixed prefix, so the cause of a full context
	// is at the top of the history.
	slices.SortStableFunc(parts[2:], func(left, right contextPart) int { return right.bytes - left.bytes })
	return parts, nil
}

func blockSize(role anthropic.Role, block anthropic.ContentBlock) (string, int) {
	switch block := block.(type) {
	case anthropic.TextBlock:
		if role == anthropic.RoleUser {
			return "your prompts", len(block.Text)
		}
		return "replies", len(block.Text)
	case anthropic.ThinkingBlock:
		return "reasoning", len(block.Thinking) + len(block.Signature)
	case anthropic.RedactedThinkingBlock:
		return "reasoning", len(block.Data)
	case anthropic.ToolUseBlock:
		return "tool calls", len(block.Name) + len(block.Input)
	case anthropic.ToolResultBlock:
		return "tool results", len(block.Content)
	case anthropic.ImageBlock:
		return "images", anthropic.ImageEstimateBytes
	case anthropic.OpaqueBlock:
		return "other", len(block.Raw)
	default:
		panic(fmt.Sprintf("context: unknown block %T", block))
	}
}

func shortCount(count int) string {
	switch {
	case count < 1000:
		return fmt.Sprint(count)
	case count < 100_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(count)/1000), ".0") + "k"
	default:
		return fmt.Sprintf("%dk", (count+500)/1000)
	}
}
