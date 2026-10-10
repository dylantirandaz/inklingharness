package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSearchEventFrames(t *testing.T) {
	body := []byte(": keepalive\r\n\r\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\r\n\r\n" +
		"event: message\r\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\r\n" +
		"data: \"result\":{\"content\":[{\"type\":\"text\",\"text\":\"Title: Weak pointers\\nURL: https://go.dev/doc/go1.24\\n日本語\"}]}}\r\n\r\n")
	before := bytes.Clone(body)
	result, err := decodeSearchResponse(body, "text/event-stream; charset=utf-8")
	if err != nil || result.IsError || !strings.Contains(result.Content, "https://go.dev/doc/go1.24\n日本語") {
		t.Fatalf("source text was lost across event frames: %+v, %v", result, err)
	}
	if !bytes.Equal(body, before) {
		t.Fatal("decoding changed the response buffer")
	}
	partial := bytes.TrimSuffix(body, []byte("\r\n\r\n"))
	if _, err := decodeSearchResponse(partial, "text/event-stream"); err == nil {
		t.Fatal("an incomplete event was accepted as a search result")
	}
}

func TestSearchRejectsInvalidReplies(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"unrelated response"}]}}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"  "}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"image","data":"image"}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":`,
	} {
		if _, err := decodeSearchResponse([]byte(body), "application/json"); err == nil {
			t.Fatalf("invalid reply accepted: %s", body)
		}
	}
}

func TestSearchServiceErrorsRemainFailures(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		diagnostic string
	}{
		{"rate limit", http.StatusTooManyRequests, "query quota exhausted", "429"},
		{"RPC error", http.StatusOK, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"query quota exhausted"}}`, "query quota exhausted"},
		{"tool error", http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"query quota exhausted"}]}}`, "query quota exhausted"},
		{"large response", http.StatusOK, strings.Repeat(" ", maxSearchResponseBytes+1), "1 MiB"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				fmt.Fprint(writer, test.body)
			}))
			defer server.Close()
			request, err := parseSearchRequest(json.RawMessage(`{"query":"Go weak pointers"}`))
			if err != nil {
				t.Fatal(err)
			}
			result, err := search(context.Background(), server.Client(), server.URL, request)
			if err != nil || !result.IsError || !strings.Contains(result.Content, test.diagnostic) {
				t.Fatalf("provider failure was lost: %+v, %v", result, err)
			}
		})
	}
}

func TestSearchTruncationKeepsCompleteCharacters(t *testing.T) {
	prefix := "Title: Documentation\nURL: https://example.org/docs\n"
	text := prefix + strings.Repeat("界", maxSearchTextChars)
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + string(encoded) + `}]}}`)
	result, err := decodeSearchResponse(body, "application/json")
	if err != nil || result.IsError || !utf8.ValidString(result.Content) {
		t.Fatalf("invalid shortened result: %+v, %v", result, err)
	}
	if !strings.HasPrefix(result.Content, prefix) || strings.Count(result.Content, "界") != maxSearchTextChars-utf8.RuneCountInString(prefix) {
		t.Fatal("shortening changed the source or the character boundary")
	}
}

func TestSearchCancellationDuringResponse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "event: message\ndata: ")
		if err := http.NewResponseController(writer).Flush(); err != nil {
			t.Error(err)
			return
		}
		close(started)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	request, err := parseSearchRequest(json.RawMessage(`{"query":"Go weak pointers"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := search(ctx, server.Client(), server.URL, request)
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("search did not receive response headers")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation became a tool result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search did not stop after cancellation")
	}
}

func TestSearchInvalidInput(t *testing.T) {
	for _, input := range []string{
		`{`,
		`{}`,
		`{"query":" \n\t"}`,
		`{"query":"Go","count":0}`,
		`{"query":"Go","count":11}`,
		`{"query":"Go","count":1.5}`,
		`{"query":"` + strings.Repeat("x", maxSearchQueryChars+1) + `"}`,
	} {
		result, err := SearchTool().Run(context.Background(), json.RawMessage(input))
		if err != nil || !result.IsError {
			t.Fatalf("invalid input was not refused: %s, %+v, %v", input, result, err)
		}
	}
}
