package tinker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

func encoded(t *testing.T, message anthropic.Message) anthropic.EncodedMessage {
	t.Helper()
	value, err := anthropic.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// The real Messages client talks to a fake Tinker endpoint through the
// transport: the request becomes chat completions, and the reply streams back
// as text and a tool call.
func TestTransportServesTinkerModels(t *testing.T) {
	var chat map[string]any
	var authorization string
	tinkerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &chat); err != nil {
			t.Error(err)
		}
		io.WriteString(w, `{"id":"c1","choices":[{"finish_reason":"stop","message":{"content":"<think>plan</think>Reading it.\n<tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\": \"go.mod\"}}\n</tool_call>"}}],"usage":{"prompt_tokens":120,"completion_tokens":30}}`)
	}))
	defer tinkerServer.Close()
	otherServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("tinker request reached the Messages API")
	}))
	defer otherServer.Close()
	transport := &Transport{Base: http.DefaultTransport, BaseURL: tinkerServer.URL + "/v1", APIKey: func() string { return "secret" }}
	client := anthropic.NewClientWithTransport("router-key", otherServer.URL, nil, transport)
	messages := []anthropic.EncodedMessage{
		encoded(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "list files"}}}),
		encoded(t, anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "ok"}, anthropic.ToolUseBlock{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}}}),
		encoded(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.ToolResultBlock{ToolUseID: "t1", Content: "go.mod"}, anthropic.TextBlock{Text: "now read it"}}}),
	}
	response, err := client.Stream(context.Background(), anthropic.Request{
		Model: "tinker://run:train:0/sampler_weights/think-round-1", MaxTokens: 1000, System: "be brief", Messages: messages,
		Tools: []anthropic.ToolDefinition{{Name: "read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}, func(anthropic.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer secret" {
		t.Errorf("authorization = %q", authorization)
	}
	roles := []string{}
	for _, message := range chat["messages"].([]any) {
		roles = append(roles, message.(map[string]any)["role"].(string))
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,user" {
		t.Errorf("roles = %s", got)
	}
	call := chat["messages"].([]any)[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if call["name"] != "bash" || call["arguments"] != `{"command":"ls"}` {
		t.Errorf("tool call = %v", call)
	}
	if tools := chat["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v", tools)
	}
	if response.StopReason != anthropic.StopToolUse || response.Usage.InputTokens != 120 || response.Usage.OutputTokens != 30 {
		t.Errorf("stop=%s usage=%+v", response.StopReason, response.Usage)
	}
	content := response.Message.Content
	if len(content) != 2 {
		t.Fatalf("content = %#v", content)
	}
	if text := content[0].(anthropic.TextBlock).Text; text != "Reading it." {
		t.Errorf("text = %q", text)
	}
	use := content[1].(anthropic.ToolUseBlock)
	if use.Name != "read" || string(use.Input) != `{"path": "go.mod"}` || use.ID == "" {
		t.Errorf("tool use = %+v", use)
	}
}

func TestTransportPassesOtherModelsThrough(t *testing.T) {
	var model string
	messagesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		model = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		stream, _ := MessagesStream([]byte(`{"choices":[{"message":{"content":"hi"}}]}`), body.Model)
		w.Write(stream)
	}))
	defer messagesServer.Close()
	transport := &Transport{Base: http.DefaultTransport, BaseURL: "http://127.0.0.1:1", APIKey: func() string { return "" }}
	client := anthropic.NewClientWithTransport("key", messagesServer.URL, nil, transport)
	response, err := client.Stream(context.Background(), anthropic.Request{Model: "deepseek/deepseek-v4.1-flash", MaxTokens: 10,
		Messages: []anthropic.EncodedMessage{encoded(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "hi"}}})}}, func(anthropic.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if model != "deepseek/deepseek-v4.1-flash" || response.StopReason != anthropic.StopEndTurn {
		t.Errorf("model=%q stop=%s", model, response.StopReason)
	}
}

func TestMissingKeyIsAClearError(t *testing.T) {
	transport := &Transport{Base: http.DefaultTransport, BaseURL: "http://127.0.0.1:1", APIKey: func() string { return "" }}
	client := anthropic.NewClientWithTransport("key", "http://127.0.0.1:1", nil, transport)
	_, err := client.Stream(context.Background(), anthropic.Request{Model: "tinker://x", MaxTokens: 10,
		Messages: []anthropic.EncodedMessage{encoded(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: "hi"}}})}}, func(anthropic.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "TINKER_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

// A round's result file moves the state to the new checkpoint and records
// which session turns were trained, so the next round resumes from it.
func TestSettleAppliesFinishedRound(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, "result.json")
	state := State{BaseModel: "Qwen/Qwen3-8B", Trained: map[string]int{"old": 4}, Running: &Round{Number: 1, PID: os.Getpid(), Result: result, Sessions: map[string]int{"a": 6}}}
	if changed, err := Settle(&state); err != nil || changed {
		t.Fatalf("changed=%t err=%v while the process runs", changed, err)
	}
	os.WriteFile(result, []byte(`{"state_path":"tinker://r/weights/1","sampler_path":"tinker://r/sampler_weights/1","loss":0.5}`), 0o600)
	if changed, err := Settle(&state); err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	if state.Running != nil || state.Rounds != 1 || state.SamplerPath != "tinker://r/sampler_weights/1" || state.Trained["a"] != 6 || state.Trained["old"] != 4 {
		t.Fatalf("state = %+v", state)
	}
	if err := Save(dir, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil || loaded.StatePath != "tinker://r/weights/1" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}
