package tinker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// DefaultBaseURL is Tinker's OpenAI-compatible inference endpoint.
const DefaultBaseURL = "https://tinker.thinkingmachines.dev/services/tinker-prod/oai/api/v1"

// Transport sends Messages API requests for tinker:// models to Tinker's
// chat completions endpoint and answers with a Messages event stream, so the
// rest of Think needs no change. Other requests go to Base unchanged.
type Transport struct {
	Base    http.RoundTripper
	BaseURL string
	APIKey  func() string
}

// NewTransport routes tinker:// models with TINKER_API_KEY and TINKER_BASE_URL.
func NewTransport(base http.RoundTripper) *Transport {
	url := os.Getenv("TINKER_BASE_URL")
	if url == "" {
		url = DefaultBaseURL
	}
	return &Transport{Base: base, BaseURL: strings.TrimRight(url, "/"), APIKey: func() string { return os.Getenv("TINKER_API_KEY") }}
}

// The client always writes the model first, so a short prefix decides the route.
var modelPrefix = regexp.MustCompile(`^\s*\{\s*"model"\s*:\s*"tinker://`)

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.Body == nil || !strings.HasSuffix(request.URL.Path, "/messages") {
		return t.Base.RoundTrip(request)
	}
	reader := bufio.NewReaderSize(request.Body, 256)
	head, _ := reader.Peek(64)
	if !modelPrefix.Match(head) {
		clone := request.Clone(request.Context())
		clone.Body = struct {
			io.Reader
			io.Closer
		}{reader, request.Body}
		return t.Base.RoundTrip(clone)
	}
	defer request.Body.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return t.complete(request, body)
}

type messagesRequest struct {
	Model     string                       `json:"model"`
	MaxTokens int                          `json:"max_tokens"`
	System    json.RawMessage              `json:"system"`
	Messages  []map[string]json.RawMessage `json:"messages"`
	Tools     []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type chatToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// blocks reads a content field that is either a string or a list of blocks.
func blocks(raw json.RawMessage) ([]block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{Type: "text", Text: text}}, nil
	}
	var list []block
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func plainText(raw json.RawMessage) string {
	list, err := blocks(raw)
	if err != nil {
		return string(raw)
	}
	parts := make([]string, 0, len(list))
	for _, item := range list {
		switch item.Type {
		case "text":
			parts = append(parts, item.Text)
		case "image":
			parts = append(parts, "[image omitted]")
		}
	}
	return strings.Join(parts, "\n")
}

// ChatRequest converts a Messages API request body to a chat completions body.
func ChatRequest(body []byte) ([]byte, string, error) {
	var request messagesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, "", fmt.Errorf("tinker: decode request: %w", err)
	}
	var messages []chatMessage
	if system := plainText(request.System); system != "" {
		messages = append(messages, chatMessage{Role: "system", Content: system})
	}
	for _, message := range request.Messages {
		var role string
		if err := json.Unmarshal(message["role"], &role); err != nil {
			return nil, "", fmt.Errorf("tinker: message role: %w", err)
		}
		list, err := blocks(message["content"])
		if err != nil {
			return nil, "", fmt.Errorf("tinker: message content: %w", err)
		}
		switch role {
		case "assistant":
			reply := chatMessage{Role: "assistant"}
			var text []string
			for _, item := range list {
				switch item.Type {
				case "text":
					text = append(text, item.Text)
				case "tool_use":
					call := chatToolCall{ID: item.ID, Type: "function"}
					call.Function.Name = item.Name
					call.Function.Arguments = string(item.Input)
					if len(item.Input) == 0 {
						call.Function.Arguments = "{}"
					}
					reply.ToolCalls = append(reply.ToolCalls, call)
				}
			}
			reply.Content = strings.Join(text, "\n")
			messages = append(messages, reply)
		default:
			var text []string
			for _, item := range list {
				switch item.Type {
				case "tool_result":
					messages = append(messages, chatMessage{Role: "tool", ToolCallID: item.ToolUseID, Content: plainText(item.Content)})
				case "text":
					text = append(text, item.Text)
				case "image":
					text = append(text, "[image omitted]")
				}
			}
			if len(text) > 0 {
				messages = append(messages, chatMessage{Role: "user", Content: strings.Join(text, "\n")})
			}
		}
	}
	type function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	type tool struct {
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	chat := struct {
		Model     string        `json:"model"`
		Messages  []chatMessage `json:"messages"`
		Tools     []tool        `json:"tools,omitempty"`
		MaxTokens int           `json:"max_tokens,omitempty"`
	}{Model: request.Model, Messages: messages, MaxTokens: request.MaxTokens}
	for _, item := range request.Tools {
		chat.Tools = append(chat.Tools, tool{Type: "function", Function: function{Name: item.Name, Description: item.Description, Parameters: item.InputSchema}})
	}
	encoded, err := json.Marshal(chat)
	return encoded, request.Model, err
}

type chatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Models that Tinker serves with a plain chat template write tool calls as
// text in the Hermes format; read those when the server parsed none.
var (
	textToolCall = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	thinkingText = regexp.MustCompile(`(?s)^\s*(?:<think>)?.*?</think>\s*`)
)

// MessagesStream converts a chat completions reply into a Messages event stream.
func MessagesStream(body []byte, model string) ([]byte, error) {
	var reply chatResponse
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("tinker: decode reply: %w", err)
	}
	if len(reply.Choices) == 0 {
		return nil, fmt.Errorf("tinker: reply has no choices")
	}
	choice := reply.Choices[0]
	text := thinkingText.ReplaceAllString(choice.Message.Content, "")
	calls := choice.Message.ToolCalls
	if len(calls) == 0 {
		for _, match := range textToolCall.FindAllStringSubmatch(text, -1) {
			var parsed struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal([]byte(match[1]), &parsed) != nil || parsed.Name == "" {
				continue
			}
			call := chatToolCall{Type: "function"}
			call.Function.Name = parsed.Name
			call.Function.Arguments = string(parsed.Arguments)
			calls = append(calls, call)
		}
		if len(calls) > 0 {
			text = textToolCall.ReplaceAllString(text, "")
		}
	}
	text = strings.TrimSpace(text)
	var stream bytes.Buffer
	event := func(name string, value any) {
		encoded, _ := json.Marshal(value)
		fmt.Fprintf(&stream, "event: %s\ndata: %s\n\n", name, encoded)
	}
	id := reply.ID
	if id == "" {
		id = "msg_tinker"
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "usage": map[string]int{"input_tokens": reply.Usage.PromptTokens, "output_tokens": 0},
	}})
	index := 0
	if text != "" {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]string{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "text_delta", "text": text}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	for number, call := range calls {
		callID := call.ID
		if callID == "" {
			callID = fmt.Sprintf("toolu_tinker_%s_%d", id, number)
		}
		arguments := strings.TrimSpace(call.Function.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			// Keep a malformed call visible to the model as a failed tool input.
			encoded, _ := json.Marshal(map[string]string{"invalid_arguments": arguments})
			arguments = string(encoded)
		}
		event("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": callID, "name": call.Function.Name, "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "input_json_delta", "partial_json": arguments}})
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	stop := "end_turn"
	switch {
	case len(calls) > 0:
		stop = "tool_use"
	case choice.FinishReason == "length":
		stop = "max_tokens"
	}
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]int{"output_tokens": reply.Usage.CompletionTokens}})
	event("message_stop", map[string]string{"type": "message_stop"})
	return stream.Bytes(), nil
}

func (t *Transport) complete(original *http.Request, body []byte) (*http.Response, error) {
	key := t.APIKey()
	if key == "" {
		return errorResponse(original, http.StatusUnauthorized, "TINKER_API_KEY is not set, so Think cannot call a tinker:// model"), nil
	}
	chat, model, err := ChatRequest(body)
	if err != nil {
		return errorResponse(original, http.StatusBadRequest, err.Error()), nil
	}
	request, err := http.NewRequestWithContext(original.Context(), http.MethodPost, t.BaseURL+"/chat/completions", bytes.NewReader(chat))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := t.Base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return errorResponse(original, response.StatusCode, "Tinker: "+strings.TrimSpace(string(reply))), nil
	}
	stream, err := MessagesStream(reply, model)
	if err != nil {
		return errorResponse(original, http.StatusBadGateway, err.Error()), nil
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {"text/event-stream"}},
		Body:          io.NopCloser(bytes.NewReader(stream)),
		ContentLength: int64(len(stream)),
		Request:       original,
	}, nil
}

func errorResponse(request *http.Request, status int, message string) *http.Response {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "tinker_error", "message": message}})
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request,
	}
}
