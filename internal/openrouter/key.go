// Package openrouter holds the OpenRouter-specific calls that sit outside the
// Messages protocol.
package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	// BaseURL is the OpenRouter API root. The Messages client appends
	// /v1/messages to it.
	BaseURL = "https://openrouter.ai/api"
	// DefaultModel is the paid Inkling Small endpoint.
	DefaultModel = "thinkingmachines/inkling-small"
	maxBody      = 1 << 20
)

// KeyInfo is the part of GET /v1/key that the login flow reports.
type KeyInfo struct {
	Label                  string
	IsFreeTier             bool
	FreeModelDailyRequests *FreeModelDailyRequests
}

// FreeModelDailyRequests is the per-UTC-day quota for :free models.
type FreeModelDailyRequests struct {
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
	Used      int `json:"used"`
}

// KeyRejected tells that the server did not accept the key.
type KeyRejected struct {
	StatusCode int
	Body       string
}

func (e *KeyRejected) Error() string {
	return fmt.Sprintf("openrouter: HTTP %d: %s", e.StatusCode, e.Body)
}

// CheckKey asks OpenRouter who the key belongs to. It proves that the key
// works before the login flow stores it.
func CheckKey(ctx context.Context, baseURL, apiKey string) (KeyInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/key", nil)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("openrouter: build request: %w", err)
	}
	request.Header.Set("authorization", "Bearer "+apiKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("openrouter: send request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return KeyInfo{}, fmt.Errorf("openrouter: read reply: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return KeyInfo{}, &KeyRejected{StatusCode: response.StatusCode, Body: string(body)}
	}
	var parsed struct {
		Data struct {
			Label                  string                  `json:"label"`
			IsFreeTier             bool                    `json:"is_free_tier"`
			FreeModelDailyRequests *FreeModelDailyRequests `json:"free_model_daily_requests"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return KeyInfo{}, fmt.Errorf("openrouter: decode key reply: %w", err)
	}
	return KeyInfo{
		Label:                  parsed.Data.Label,
		IsFreeTier:             parsed.Data.IsFreeTier,
		FreeModelDailyRequests: parsed.Data.FreeModelDailyRequests,
	}, nil
}
