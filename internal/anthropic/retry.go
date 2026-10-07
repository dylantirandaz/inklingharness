package anthropic

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/latency"
)

const (
	maxAttempts    = 5
	baseRetryDelay = 500 * time.Millisecond
	maxRetryDelay  = 30 * time.Second
)

// sendWithRetries returns only successful response headers. Stream consumption
// happens outside this loop, so a broken stream can never replay a request.
func (c *Client) sendWithRetries(ctx context.Context, body requestBody, observe func(StreamEvent) error) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, timing := latency.StartAttempt(ctx, attempt)
		request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.baseURL+"/v1/messages", body.reader())
		if err != nil {
			timing.End(nil, err)
			return nil, fmt.Errorf("anthropic: build request: %w", err)
		}
		request.ContentLength = body.size
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(body.reader()), nil }
		request.Header.Set("authorization", "Bearer "+c.apiKey)
		request.Header.Set("anthropic-version", apiVersion)
		request.Header.Set("content-type", "application/json")
		request.Header.Set("accept", "text/event-stream")
		request.Header.Set("HTTP-Referer", "https://github.com/dylantirandaz/inklingharness")
		request.Header.Set("X-OpenRouter-Title", "inkling")
		request.Header.Set("X-OpenRouter-Categories", "cli-agent")

		response, err := c.httpClient.Do(request)
		timing.End(response, err)
		if err != nil {
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			// A transport error does not establish whether the server processed
			// the request. Retrying could duplicate inference or side effects.
			return nil, fmt.Errorf("anthropic: send request: %w", err)
		}
		if response.StatusCode == http.StatusOK {
			return response, nil
		}
		shouldRetry := retryableStatus(response.StatusCode) && attempt < maxAttempts
		var delay time.Duration
		if shouldRetry {
			delay = retryDelay(attempt, response.Header.Get("Retry-After"), time.Now())
			shouldRetry = delay <= maxRetryDelay
		}
		if !shouldRetry {
			errorBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
			response.Body.Close()
			if readErr != nil {
				return nil, fmt.Errorf("anthropic: HTTP %d, and reading the error body failed: %w", response.StatusCode, readErr)
			}
			return nil, &APIError{StatusCode: response.StatusCode, Body: string(errorBody)}
		}
		// Do not wait for an arbitrarily long error body before retrying.
		response.Body.Close()
		if observe != nil {
			if err := observe(RetryEvent{Attempt: attempt + 1, Delay: delay, Reason: fmt.Sprintf("HTTP %d", response.StatusCode)}); err != nil {
				return nil, err
			}
		}
		wait := latency.Begin(ctx, latency.Retry, "wait")
		err = waitForRetry(ctx, delay)
		wait.End(err)
		if err != nil {
			return nil, err
		}
	}
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	default:
		return false
	}
}

func retryDelay(attempt int, retryAfter string, now time.Time) time.Duration {
	if delay, valid := parseRetryAfter(retryAfter, now); valid {
		return delay
	}
	delay := baseRetryDelay
	for n := 1; n < attempt && delay < maxRetryDelay; n++ {
		delay *= 2
	}
	if delay >= maxRetryDelay {
		return maxRetryDelay
	}
	// Positive jitter keeps the exponential minimum while spreading clients.
	delay += time.Duration(rand.Int64N(int64(delay / 10)))
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

// Saturating decimal parsing prevents overflow. A wait above maxRetryDelay
// returns the original HTTP failure; it must never cause an early retry.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	seconds := time.Duration(0)
	decimal := true
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			decimal = false
			break
		}
		if seconds <= (time.Duration(math.MaxInt64)/time.Second-time.Duration(digit-'0'))/10 {
			seconds = seconds*10 + time.Duration(digit-'0')
		} else {
			seconds = time.Duration(math.MaxInt64) / time.Second
		}
	}
	if decimal {
		if seconds >= time.Duration(math.MaxInt64)/time.Second {
			return time.Duration(math.MaxInt64), true
		}
		return seconds * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := date.Sub(now)
	if delay < 0 {
		return 0, true
	}
	return delay, true
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
