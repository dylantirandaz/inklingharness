package anthropic

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type retryTransport func(*http.Request) (*http.Response, error)

func (transport retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type trackedRetryBody struct {
	io.Reader
	closed bool
}

func (body *trackedRetryBody) Close() error {
	body.closed = true
	return nil
}

func TestStreamRetriesEligibleStatuses(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504, 529} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			client := NewClient("test-key", "https://example.invalid", nil)
			attempts := 0
			failedBody := &trackedRetryBody{Reader: strings.NewReader("temporary failure")}
			successBody := &trackedRetryBody{Reader: strings.NewReader(streamFixture)}
			client.httpClient.Transport = retryTransport(func(request *http.Request) (*http.Response, error) {
				attempts++
				if attempts == 1 {
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"0"}}, Body: failedBody}, nil
				}
				if !failedBody.closed {
					t.Error("retry started before closing the rejected response")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: successBody}, nil
			})
			var retries []RetryEvent
			response, err := client.Stream(context.Background(), Request{Model: "m", MaxTokens: 1}, func(event StreamEvent) error {
				if retry, ok := event.(RetryEvent); ok {
					if !failedBody.closed {
						t.Error("observer called before closing the rejected response")
					}
					retries = append(retries, retry)
				}
				return nil
			})
			if err != nil || response == nil || attempts != 2 {
				t.Fatalf("response=%v err=%v attempts=%d", response, err, attempts)
			}
			if len(retries) != 1 || retries[0].Attempt != 2 || retries[0].Delay != 0 || retries[0].Reason == "" {
				t.Errorf("retry events = %+v", retries)
			}
			if !successBody.closed {
				t.Error("successful response body was not closed")
			}
		})
	}
}

func TestStreamStopsAfterFiveAttempts(t *testing.T) {
	client := NewClient("test-key", "https://example.invalid", nil)
	var bodies []*trackedRetryBody
	client.httpClient.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
		body := &trackedRetryBody{Reader: strings.NewReader("still overloaded")}
		bodies = append(bodies, body)
		return &http.Response{StatusCode: 529, Header: http.Header{"Retry-After": []string{"0"}}, Body: body}, nil
	})
	var attempts []int
	_, err := client.Stream(context.Background(), Request{}, func(event StreamEvent) error {
		if retry, ok := event.(RetryEvent); ok {
			attempts = append(attempts, retry.Attempt)
		}
		return nil
	})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 529 || apiError.Body != "still overloaded" {
		t.Fatalf("error = %v", err)
	}
	if len(bodies) != 5 || len(attempts) != 4 {
		t.Fatalf("requests=%d retry events=%v", len(bodies), attempts)
	}
	for index, attempt := range attempts {
		if attempt != index+2 {
			t.Errorf("retry attempt %d = %d", index, attempt)
		}
	}
	for _, body := range bodies {
		if !body.closed {
			t.Error("response body was not closed")
		}
	}
}

func TestStreamDoesNotRetryRejectedRequests(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 501} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			client := NewClient("test-key", "https://example.invalid", nil)
			attempts := 0
			body := &trackedRetryBody{Reader: strings.NewReader("rejected")}
			client.httpClient.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
				attempts++
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"0"}}, Body: body}, nil
			})
			_, err := client.Stream(context.Background(), Request{}, func(event StreamEvent) error {
				t.Errorf("unexpected event %T", event)
				return nil
			})
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != status || attempts != 1 || !body.closed {
				t.Fatalf("error=%v requests=%d closed=%v", err, attempts, body.closed)
			}
		})
	}
}

func TestStreamDoesNotRetryTransportErrors(t *testing.T) {
	client := NewClient("test-key", "https://example.invalid", nil)
	transportError := errors.New("connection lost after sending request")
	attempts := 0
	client.httpClient.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, transportError
	})
	_, err := client.Stream(context.Background(), Request{}, nil)
	if !errors.Is(err, transportError) || attempts != 1 {
		t.Fatalf("error=%v requests=%d", err, attempts)
	}
}

type failedStreamReader struct {
	err error
}

func (reader failedStreamReader) Read([]byte) (int, error) {
	return 0, reader.err
}

func TestStreamDoesNotRetryAfterSuccessfulHeaders(t *testing.T) {
	partial := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	for _, prefix := range []string{"", partial} {
		name := "headers only"
		if prefix != "" {
			name = "partial stream"
		}
		t.Run(name, func(t *testing.T) {
			client := NewClient("test-key", "https://example.invalid", nil)
			streamError := errors.New("stream connection lost")
			body := &trackedRetryBody{Reader: io.MultiReader(strings.NewReader(prefix), failedStreamReader{err: streamError})}
			attempts := 0
			client.httpClient.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
				attempts++
				return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
			})
			text := ""
			_, err := client.Stream(context.Background(), Request{}, func(event StreamEvent) error {
				switch event := event.(type) {
				case RetryEvent:
					t.Error("successful headers must prevent retries")
				case TextDelta:
					text += event.Text
				}
				return nil
			})
			if !errors.Is(err, streamError) || attempts != 1 || !body.closed {
				t.Fatalf("error=%v requests=%d closed=%v", err, attempts, body.closed)
			}
			if prefix != "" && text != "partial" {
				t.Errorf("streamed text = %q", text)
			}
		})
	}
}

func TestStreamCancelsRetryWait(t *testing.T) {
	client := NewClient("test-key", "https://example.invalid", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	body := &trackedRetryBody{Reader: strings.NewReader("busy")}
	client.httpClient.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"30"}}, Body: body}, nil
	})
	_, err := client.Stream(ctx, Request{}, func(event StreamEvent) error {
		if retry, ok := event.(RetryEvent); ok {
			if retry.Delay != 30*time.Second {
				t.Errorf("delay = %v", retry.Delay)
			}
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 || !body.closed {
		t.Fatalf("error=%v requests=%d closed=%v", err, attempts, body.closed)
	}
}

func TestRetryWaitHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := waitForRetry(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", err)
	}
}

func TestRetryObserverCanStopRequest(t *testing.T) {
	client := NewClient("test-key", "https://example.invalid", nil)
	attempts := 0
	body := &trackedRetryBody{Reader: strings.NewReader("busy")}
	client.httpClient.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: 503, Body: body}, nil
	})
	observerError := errors.New("observer stopped")
	_, err := client.Stream(context.Background(), Request{}, func(StreamEvent) error { return observerError })
	if !errors.Is(err, observerError) || attempts != 1 || !body.closed {
		t.Fatalf("error=%v requests=%d closed=%v", err, attempts, body.closed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		value string
		delay time.Duration
		valid bool
	}{
		{name: "seconds", value: "2", delay: 2 * time.Second, valid: true},
		{name: "zero", value: "0", valid: true},
		{name: "whitespace", value: " 3 ", delay: 3 * time.Second, valid: true},
		{name: "long seconds", value: "3600", delay: time.Hour, valid: true},
		{name: "overflow seconds", value: strings.Repeat("9", 100), delay: time.Duration(math.MaxInt64), valid: true},
		{name: "leading zeroes", value: strings.Repeat("0", 100) + "2", delay: 2 * time.Second, valid: true},
		{name: "date", value: now.Add(7 * time.Second).Format(http.TimeFormat), delay: 7 * time.Second, valid: true},
		{name: "past date", value: now.Add(-time.Minute).Format(http.TimeFormat), valid: true},
		{name: "long date", value: now.Add(time.Hour).Format(http.TimeFormat), delay: time.Hour, valid: true},
		{name: "distant date", value: "Fri, 31 Dec 9999 23:59:59 GMT", delay: time.Duration(math.MaxInt64), valid: true},
		{name: "missing"},
		{name: "negative", value: "-1"},
		{name: "fraction", value: "1.5"},
		{name: "invalid suffix", value: strings.Repeat("9", 100) + "bad"},
		{name: "invalid date", value: "not a date"},
	} {
		t.Run(test.name, func(t *testing.T) {
			delay, valid := parseRetryAfter(test.value, now)
			if delay != test.delay || valid != test.valid {
				t.Fatalf("delay=%v valid=%v; want %v, %v", delay, valid, test.delay, test.valid)
			}
		})
	}
}

func TestRetryBackoffIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 4; attempt++ {
		minimum := baseRetryDelay * time.Duration(1<<(attempt-1))
		for range 20 {
			delay := retryDelay(attempt, "invalid", time.Now())
			if delay < minimum || delay >= minimum+minimum/10 {
				t.Fatalf("attempt %d delay=%v outside backoff range", attempt, delay)
			}
		}
	}
	if delay := retryDelay(1000, "", time.Now()); delay != maxRetryDelay {
		t.Errorf("large attempt delay = %v", delay)
	}
	if delay := retryDelay(1, "2", time.Now()); delay != 2*time.Second {
		t.Errorf("Retry-After was not honored: %v", delay)
	}
}

func TestLongRetryAfterDoesNotRetryEarly(t *testing.T) {
	for _, header := range []string{"31", "99999999999999999999999999", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		client := NewClient("key", "https://example.invalid", nil)
		attempts := 0
		body := &trackedRetryBody{Reader: strings.NewReader("wait longer")}
		client.httpClient.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{header}}, Body: body}, nil
		})
		_, err := client.Stream(context.Background(), Request{}, nil)
		var failure *APIError
		if !errors.As(err, &failure) || failure.StatusCode != 429 || attempts != 1 || !body.closed {
			t.Fatalf("long Retry-After=%q: attempts=%d closed=%t error=%v", header, attempts, body.closed, err)
		}
	}
}
