package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const (
	searchEndpoint         = "https://mcp.exa.ai/mcp?tools=web_search_exa"
	searchTimeout          = 30 * time.Second
	defaultSearchCount     = 5
	maxSearchCount         = 10
	maxSearchQueryChars    = 4096
	maxSearchResponseBytes = 1 << 20
	maxSearchTextChars     = 20_000
)

// SearchTool searches Exa's keyless service. Network access needs approval,
// as it does for web_fetch. No request is sent until the tool runs.
func SearchTool() tools.Tool {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return tools.Tool{
		Name:        "web_search",
		Description: "Search the web through Exa. Returns source titles, URLs, and excerpts. Use web_fetch to read a full page and cite source URLs. Count defaults to 5, at most 10. The keyless service is rate-limited.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":4096},"count":{"type":"integer","minimum":1,"maximum":10}},"required":["query"],"additionalProperties":false}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (tools.Result, error) {
			if err := ctx.Err(); err != nil {
				return tools.Result{}, err
			}
			request, err := parseSearchRequest(input)
			if err != nil {
				return failure("invalid web_search input: " + err.Error()), nil
			}
			return search(ctx, client, searchEndpoint, request)
		},
	}
}

type searchRequest struct {
	Query     string `json:"query"`
	Count     int    `json:"numResults"`
	Objective string `json:"objective"`
}

func parseSearchRequest(input json.RawMessage) (searchRequest, error) {
	var arguments struct {
		Query string `json:"query"`
		Count *int   `json:"count"`
	}
	if err := json.Unmarshal(input, &arguments); err != nil {
		return searchRequest{}, err
	}
	query := strings.TrimSpace(arguments.Query)
	if query == "" || utf8.RuneCountInString(query) > maxSearchQueryChars {
		return searchRequest{}, fmt.Errorf("query must contain 1 to %d characters", maxSearchQueryChars)
	}
	count := defaultSearchCount
	if arguments.Count != nil {
		count = *arguments.Count
	}
	if count < 1 || count > maxSearchCount {
		return searchRequest{}, fmt.Errorf("count must be from 1 to %d", maxSearchCount)
	}
	return searchRequest{Query: query, Count: count, Objective: query}, nil
}

func search(ctx context.Context, client *http.Client, endpoint string, request searchRequest) (tools.Result, error) {
	payload := struct {
		Version string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name      string        `json:"name"`
			Arguments searchRequest `json:"arguments"`
		} `json:"params"`
	}{Version: "2.0", ID: 1, Method: "tools/call"}
	payload.Params.Name = "web_search_exa"
	payload.Params.Arguments = request
	body, err := json.Marshal(payload)
	if err != nil {
		return tools.Result{}, fmt.Errorf("encode web search: %w", err)
	}
	searchContext, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(searchContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return tools.Result{}, fmt.Errorf("create web search: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json, text/event-stream")
	httpRequest.Header.Set("User-Agent", userAgent)
	response, err := client.Do(httpRequest)
	if err != nil {
		return searchTransferFailure(ctx, searchContext, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes))
		if err != nil {
			return searchTransferFailure(ctx, searchContext, err)
		}
		message := "web_search failed: Exa returned HTTP " + response.Status
		if response.StatusCode == http.StatusTooManyRequests {
			message += "; the free search rate limit was reached"
		}
		if detail := strings.TrimSpace(cleanText(string(body))); detail != "" {
			message += "\n" + detail
		}
		return failure(message), nil
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, maxSearchResponseBytes+1))
	if err != nil {
		return searchTransferFailure(ctx, searchContext, err)
	}
	if len(body) > maxSearchResponseBytes {
		return failure("web_search failed: Exa returned more than 1 MiB"), nil
	}
	result, err := decodeSearchResponse(body, response.Header.Get("Content-Type"))
	if err != nil {
		return failure("web_search failed: " + err.Error()), nil
	}
	return result, nil
}

func searchTransferFailure(ctx, searchContext context.Context, err error) (tools.Result, error) {
	if ctx.Err() != nil {
		return tools.Result{}, ctx.Err()
	}
	if errors.Is(searchContext.Err(), context.DeadlineExceeded) {
		return failure(fmt.Sprintf("web_search failed: no complete response within %s", searchTimeout)), nil
	}
	return failure("web_search failed: " + err.Error()), nil
}

func decodeSearchResponse(body []byte, contentType string) (tools.Result, error) {
	format, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return tools.Result{}, fmt.Errorf("invalid response type: %w", err)
	}
	switch format {
	case "application/json":
		result, matched, err := decodeSearchReply(body)
		if err != nil {
			return tools.Result{}, err
		}
		if matched {
			return result, nil
		}
	case "text/event-stream":
		var data []byte
		owned := false
		for line := range bytes.SplitAfterSeq(body, []byte("\n")) {
			if !bytes.HasSuffix(line, []byte("\n")) {
				break
			}
			line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			if len(line) == 0 {
				if data != nil {
					result, matched, err := decodeSearchReply(data)
					if err != nil {
						return tools.Result{}, err
					}
					if matched {
						return result, nil
					}
				}
				data, owned = nil, false
				continue
			}
			value, found := bytes.CutPrefix(line, []byte("data:"))
			if !found {
				continue
			}
			value = bytes.TrimPrefix(value, []byte(" "))
			if data == nil {
				data = value
			} else {
				// A single data line borrows the response body. Copy only when
				// the event needs joined lines, so the input stays unchanged.
				if !owned {
					data = bytes.Clone(data)
					owned = true
				}
				data = append(append(data, '\n'), value...)
			}
		}
	default:
		return tools.Result{}, fmt.Errorf("unsupported response type %q", format)
	}
	return tools.Result{}, errors.New("Exa did not return a complete search reply")
}

func decodeSearchReply(data []byte) (tools.Result, bool, error) {
	var reply struct {
		Version string `json:"jsonrpc"`
		ID      *int   `json:"id"`
		Method  string `json:"method"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return tools.Result{}, false, fmt.Errorf("invalid search reply: %w", err)
	}
	if reply.Version != "2.0" {
		return tools.Result{}, false, errors.New("invalid search reply version")
	}
	if reply.Error != nil {
		return tools.Result{}, false, fmt.Errorf("Exa error %d: %s", reply.Error.Code, reply.Error.Message)
	}
	if reply.ID == nil && reply.Method != "" && reply.Result == nil {
		return tools.Result{}, false, nil
	}
	if reply.ID == nil || *reply.ID != 1 {
		return tools.Result{}, false, errors.New("search reply ID does not match the request")
	}
	if reply.Result == nil {
		return tools.Result{}, false, errors.New("search reply has no result")
	}
	totalBytes := 0
	for _, block := range reply.Result.Content {
		if block.Type != "text" {
			return tools.Result{}, false, fmt.Errorf("unsupported search content %q", block.Type)
		}
		totalBytes += len(block.Text)
	}
	var content string
	switch len(reply.Result.Content) {
	case 0:
		return tools.Result{}, false, errors.New("search reply has no text")
	case 1:
		content = reply.Result.Content[0].Text
	default:
		var text strings.Builder
		text.Grow(totalBytes + 2*(len(reply.Result.Content)-1))
		for index, block := range reply.Result.Content {
			if index > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(block.Text)
		}
		content = text.String()
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return tools.Result{}, false, errors.New("search reply has no text")
	}
	part, total, taken := slice(content, 1, maxSearchTextChars)
	if taken < total {
		part += "\n\n[Search response shortened at 20,000 characters.]"
	}
	if reply.Result.IsError {
		return failure("web_search: " + part), true, nil
	}
	return tools.Result{Content: part}, true, nil
}
