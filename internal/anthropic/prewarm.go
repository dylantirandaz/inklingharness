package anthropic

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Prewarm opens the connection that later requests reuse. It sends one HEAD
// request without credentials; any HTTP status means that DNS, TCP, and TLS
// are complete. It never sends conversation data.
func (c *Client) Prewarm(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL+"/v1/messages", nil)
	if err != nil {
		return fmt.Errorf("anthropic: prewarm: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("anthropic: prewarm: %w", err)
	}
	// Draining lets HTTP/1.1 return the connection to the idle pool.
	_, copyErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
	closeErr := response.Body.Close()
	if copyErr != nil {
		return fmt.Errorf("anthropic: prewarm: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("anthropic: prewarm: %w", closeErr)
	}
	return nil
}
