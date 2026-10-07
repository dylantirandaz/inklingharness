package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func bodyBytes(t *testing.T, request Request) []byte {
	t.Helper()
	body, err := request.body()
	if err != nil {
		t.Fatal(err)
	}
	joined := bytes.Join(body.segments, nil)
	if int64(len(joined)) != body.size {
		t.Fatalf("body size = %d, joined length = %d", body.size, len(joined))
	}
	return joined
}

func TestRequestBodyPreservesTypedAndExtraFields(t *testing.T) {
	typed := []Message{
		{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "hello"}}},
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "answer"}}},
	}
	request := Request{
		Model: "chosen-model", MaxTokens: 2048, System: "system prompt",
		Messages: mustEncode(t, typed...),
		Tools:    []ToolDefinition{{Name: "read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Thinking: json.RawMessage(`{"type":"enabled","budget_tokens":1024}`),
		Extra:    map[string]json.RawMessage{"effort": json.RawMessage(`"high"`), "provider": json.RawMessage(`{"order":["preferred"]}`)},
	}
	body := bodyBytes(t, request)
	var decoded struct {
		Model        string           `json:"model"`
		MaxTokens    int              `json:"max_tokens"`
		System       string           `json:"system"`
		Stream       bool             `json:"stream"`
		Messages     []Message        `json:"messages"`
		Tools        []ToolDefinition `json:"tools"`
		CacheControl struct {
			Type string `json:"type"`
		} `json:"cache_control"`
		Thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		Effort   string `json:"effort"`
		Provider struct {
			Order []string `json:"order"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Model != request.Model || decoded.MaxTokens != request.MaxTokens || decoded.System != request.System || !decoded.Stream {
		t.Errorf("request scalar fields were not preserved: %+v", decoded)
	}
	if !reflect.DeepEqual(decoded.Messages, typed) || !reflect.DeepEqual(decoded.Tools, request.Tools) {
		t.Errorf("messages or tools were not preserved: messages=%+v tools=%+v", decoded.Messages, decoded.Tools)
	}
	if decoded.CacheControl.Type != "ephemeral" || decoded.Thinking.Type != "enabled" || decoded.Thinking.BudgetTokens != 1024 {
		t.Errorf("cache or thinking configuration was lost: %+v", decoded)
	}
	if decoded.Effort != "high" || len(decoded.Provider.Order) != 1 || decoded.Provider.Order[0] != "preferred" {
		t.Errorf("extra fields were not preserved: %+v", decoded)
	}
}

func TestRequestBodyMatchesOneJSONEncoding(t *testing.T) {
	typed := everyBlockKind()
	thinking := json.RawMessage(`{"type":"disabled"}`)
	request := Request{Model: "m", MaxTokens: 9, System: "s", Messages: mustEncode(t, typed...), Tools: []ToolDefinition{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}, Thinking: thinking}
	want, err := json.Marshal(struct {
		Model        string           `json:"model"`
		MaxTokens    int              `json:"max_tokens"`
		Messages     []Message        `json:"messages"`
		Stream       bool             `json:"stream"`
		CacheControl cacheControl     `json:"cache_control"`
		System       string           `json:"system"`
		Tools        []ToolDefinition `json:"tools"`
		Thinking     json.RawMessage  `json:"thinking"`
	}{"m", 9, typed, true, cacheControl{Type: "ephemeral"}, "s", request.Tools, thinking})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodyBytes(t, request); !bytes.Equal(got, want) {
		t.Fatalf("segmented body differs from one encoding:\n got %s\nwant %s", got, want)
	}
	request.Messages = append(request.Messages, EncodedMessage{})
	if _, err := request.body(); err == nil {
		t.Fatal("zero encoded message was sent")
	}
}

func TestRequestBodyOmitsAbsentOptionalFields(t *testing.T) {
	for _, extra := range []map[string]json.RawMessage{nil, {"effort": json.RawMessage(`"low"`)}} {
		body := bodyBytes(t, Request{Model: "m", MaxTokens: 1, Extra: extra})
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"system", "tools", "thinking"} {
			if _, present := fields[key]; present {
				t.Errorf("absent optional field %q was included", key)
			}
		}
		for _, key := range []string{"model", "max_tokens", "messages", "stream", "cache_control"} {
			if _, present := fields[key]; !present {
				t.Errorf("required field %q was omitted", key)
			}
		}
	}
}

func TestRequestBodyReservesEveryManagedField(t *testing.T) {
	for _, key := range []string{"model", "max_tokens", "messages", "stream", "cache_control", "system", "tools", "thinking"} {
		t.Run(key, func(t *testing.T) {
			for _, request := range []Request{
				{},
				{Model: "m", MaxTokens: 1, System: "system", Messages: mustEncode(t, Message{Role: RoleUser}), Tools: []ToolDefinition{{Name: "read"}}, Thinking: json.RawMessage(`{"type":"disabled"}`)},
			} {
				request.Extra = map[string]json.RawMessage{key: json.RawMessage(`null`)}
				if _, err := request.body(); err == nil || !strings.Contains(err.Error(), "collides") {
					t.Errorf("managed field %q must reject extra collision: %v", key, err)
				}
			}
		})
	}
}

func TestStreamAttributesThisAppOnEveryAttempt(t *testing.T) {
	client := NewClient("test-key", "https://example.invalid", nil)
	attempts := 0
	client.httpClient.Transport = retryTransport(func(request *http.Request) (*http.Response, error) {
		attempts++
		for name, expected := range map[string]string{
			"HTTP-Referer":            "https://github.com/dylantirandaz/inklingharness",
			"X-OpenRouter-Title":      "inkling",
			"X-OpenRouter-Categories": "cli-agent",
		} {
			if actual := request.Header.Get(name); actual != expected {
				t.Errorf("attempt %d header %s = %q, want %q", attempts, name, actual, expected)
			}
		}
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&fields); err != nil {
			t.Errorf("attempt %d body cannot be decoded: %v", attempts, err)
		}
		var model string
		if err := json.Unmarshal(fields["model"], &model); err != nil || model != "chosen-model" {
			t.Errorf("attempt %d model=%q err=%v", attempts, model, err)
		}
		if attempts == 1 {
			return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"0"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(streamFixture))}, nil
	})
	if _, err := client.Stream(context.Background(), Request{Model: "chosen-model"}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d", attempts)
	}
}
