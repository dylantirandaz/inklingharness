package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestBodyAppliesTheRunProfile(t *testing.T) {
	astra := wireProfile{Model: "openai/gpt-6-astra", Provider: "openai", Effort: "xhigh", MaxTokens: 128000}
	body := []byte(`{"model":"openai/gpt-6-astra","max_tokens":4096,"thinking":{"type":"enabled"},"output_config":{"effort":"low","format":"x"},"messages":[]}`)
	forwarded, err := requestBody(body, "/api/v1/messages", normalizeMessages, astra)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		MaxTokens    int             `json:"max_tokens"`
		Thinking     json.RawMessage `json:"thinking"`
		OutputConfig map[string]any  `json:"output_config"`
		Provider     struct {
			Order          []string `json:"order"`
			AllowFallbacks bool     `json:"allow_fallbacks"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(forwarded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields.MaxTokens != 128000 || fields.Thinking != nil || fields.OutputConfig["effort"] != "xhigh" || fields.OutputConfig["format"] != "x" {
		t.Fatalf("profile not applied: %s", forwarded)
	}
	if len(fields.Provider.Order) != 1 || fields.Provider.Order[0] != "openai" || fields.Provider.AllowFallbacks {
		t.Fatalf("route not applied: %s", forwarded)
	}

	responses, err := requestBody([]byte(`{"model":"openai/gpt-6-astra","reasoning":{"effort":"xhigh"}}`), "/api/v1/responses", verifyMessages, astra)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		MaxOutputTokens int `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(responses, &output); err != nil || output.MaxOutputTokens != 128000 {
		t.Fatalf("Responses cap not applied: %s", responses)
	}
}

func TestRequestBodyRejectsOtherProfiles(t *testing.T) {
	astra := wireProfile{Model: "openai/gpt-6-astra", Provider: "openai", Effort: "xhigh", MaxTokens: 128000}
	cases := map[string]struct {
		body   string
		path   string
		policy messagesPolicy
	}{
		"other model":            {`{"model":"thinkingmachines/inkling-small"}`, "/api/v1/messages", normalizeMessages},
		"verify default profile": {`{"model":"openai/gpt-6-astra","max_tokens":16384,"output_config":{"effort":"high"},"provider":{"order":["deepinfra/fp8"],"allow_fallbacks":false}}`, "/api/v1/messages", verifyMessages},
		"responses other effort": {`{"model":"openai/gpt-6-astra","reasoning":{"effort":"high"}}`, "/api/v1/responses", verifyMessages},
	}
	for name, test := range cases {
		if _, err := requestBody([]byte(test.body), test.path, test.policy, astra); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := requestBody([]byte(`{"model":"thinkingmachines/inkling-small","max_tokens":16384,"output_config":{"effort":"high"},"provider":{"order":["deepinfra/fp8"],"allow_fallbacks":false}}`), "/api/v1/messages", verifyMessages, defaultProfile()); err != nil {
		t.Errorf("default profile rejected: %v", err)
	}
}

func TestPrimeUpstreamStripsTheCheckedRoute(t *testing.T) {
	prime := wireProfile{Model: "deepseek/deepseek-v4.1-flash", Provider: "prime", Effort: "max", MaxTokens: 128000, Upstream: primeUpstream}
	if prime.upstreamHost() != "https://api.pinference.ai" || prime.keyVariable() != "PRIME_API_KEY" {
		t.Fatalf("wrong Prime upstream: %s %s", prime.upstreamHost(), prime.keyVariable())
	}
	route := `"provider":{"order":["prime"],"allow_fallbacks":false}`
	verified, err := requestBody([]byte(`{"model":"deepseek/deepseek-v4.1-flash","max_tokens":128000,"output_config":{"effort":"max"},`+route+`}`), "/api/v1/messages", verifyMessages, prime)
	if err != nil || strings.Contains(string(verified), "provider") {
		t.Fatalf("verified Messages body kept its route or failed: %s %v", verified, err)
	}
	normalized, err := requestBody([]byte(`{"model":"deepseek/deepseek-v4.1-flash","max_tokens":5,`+route+`}`), "/api/v1/messages", normalizeMessages, prime)
	if err != nil || strings.Contains(string(normalized), "provider") || !strings.Contains(string(normalized), `"max_tokens":128000`) {
		t.Fatalf("normalized Messages body is wrong: %s %v", normalized, err)
	}
	if _, err := requestBody([]byte(`{"model":"deepseek/deepseek-v4.1-flash","max_tokens":128000,"output_config":{"effort":"max"},"provider":{"order":["deepinfra/fp8"],"allow_fallbacks":false}}`), "/api/v1/messages", verifyMessages, prime); err == nil {
		t.Fatal("a request with another route was accepted")
	}
}
