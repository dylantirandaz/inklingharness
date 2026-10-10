// Package web provides web search and URL fetching. Both return text with
// source URLs and require approval before sending data to the network.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const (
	fetchTimeout = 30 * time.Second
	maxRedirects = 5
	// maxBodyBytes caps the response that is read. A larger body is cut, so a
	// large or endless response cannot use all memory.
	maxBodyBytes = 5 << 20
	// maxErrorBytes caps the body text in the result for a failed status.
	maxErrorBytes   = 2 << 10
	defaultMaxChars = 50_000
	maxCharsLimit   = 200_000
	// maxSliceBytes keeps one result below the 256 KiB limit of read tools,
	// also when most characters need more than one byte.
	maxSliceBytes = 240 << 10
	// maxTitleChars keeps a long title from filling every page of a result.
	maxTitleChars = 200
	userAgent     = "inkling/1 (+https://github.com/dylantirandaz/inklingharness)"
	acceptTypes   = "text/markdown, text/html;q=0.9, text/plain;q=0.8, */*;q=0.1"
)

// FetchTool returns the web_fetch tool. It is not read-only: a request goes
// to the network and can change state on a server, so it needs approval
// like bash.
func FetchTool() tools.Tool {
	client := &http.Client{CheckRedirect: checkRedirect}
	return tools.Tool{
		Name:        "web_fetch",
		Description: "Fetch an http or https URL and return its text; HTML becomes plain text with links. A long page comes in parts; the result gives the next offset.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"offset":{"type":"integer","minimum":1},"max_chars":{"type":"integer","minimum":1,"maximum":200000}},"required":["url"]}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (tools.Result, error) {
			request, err := parseRequest(input)
			if err != nil {
				return failure("invalid input: " + err.Error()), nil
			}
			return fetch(ctx, client, request)
		},
	}
}

type fetchRequest struct {
	address  *url.URL
	offset   int
	maxChars int
}

func parseRequest(input json.RawMessage) (fetchRequest, error) {
	arguments := struct {
		URL      string `json:"url"`
		Offset   int    `json:"offset"`
		MaxChars int    `json:"max_chars"`
	}{Offset: 1, MaxChars: defaultMaxChars}
	if err := json.Unmarshal(input, &arguments); err != nil {
		return fetchRequest{}, err
	}
	if arguments.URL == "" {
		return fetchRequest{}, errors.New("url is required")
	}
	address, err := url.Parse(arguments.URL)
	if err != nil {
		return fetchRequest{}, err
	}
	if !webScheme(address.Scheme) {
		return fetchRequest{}, fmt.Errorf("url %q must start with http:// or https://", arguments.URL)
	}
	if address.Host == "" {
		return fetchRequest{}, fmt.Errorf("url %q has no host", arguments.URL)
	}
	if arguments.Offset < 1 {
		return fetchRequest{}, errors.New("offset must be 1 or more")
	}
	if arguments.MaxChars < 1 || arguments.MaxChars > maxCharsLimit {
		return fetchRequest{}, fmt.Errorf("max_chars must be from 1 to %d", maxCharsLimit)
	}
	return fetchRequest{address: address, offset: arguments.Offset, maxChars: arguments.MaxChars}, nil
}

// webScheme reports whether a scheme is http or https. url.Parse makes the
// scheme lowercase.
func webScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

func checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if !webScheme(request.URL.Scheme) {
		return fmt.Errorf("redirect to %q refused: web_fetch reads only http and https URLs", request.URL.Redacted())
	}
	return nil
}

func failure(message string) tools.Result {
	return tools.Result{Content: message, IsError: true}
}

func fetch(ctx context.Context, client *http.Client, request fetchRequest) (tools.Result, error) {
	fetchContext, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(fetchContext, http.MethodGet, request.address.String(), nil)
	if err != nil {
		return failure("web_fetch failed: " + err.Error()), nil
	}
	httpRequest.Header.Set("User-Agent", userAgent)
	httpRequest.Header.Set("Accept", acceptTypes)
	response, err := client.Do(httpRequest)
	if err != nil {
		return transferFailure(ctx, fetchContext, err)
	}
	defer response.Body.Close()
	final := response.Request.URL

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return statusFailure(ctx, fetchContext, response, final)
	}

	// Check a declared type before the body is read, so that a refused type
	// costs no transfer.
	declared := response.Header.Get("Content-Type")
	var format textFormat
	if declared != "" {
		format, err = parseContentType(declared)
		if err != nil {
			return failure(fmt.Sprintf("web_fetch cannot use %s: %v", final.Redacted(), err)), nil
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return transferFailure(ctx, fetchContext, err)
	}
	cut := len(body) > maxBodyBytes
	if cut {
		body = body[:maxBodyBytes]
	}
	if declared == "" {
		// The server sent no type, so guess it from the first bytes.
		format, err = parseContentType(http.DetectContentType(body))
		if err != nil {
			return failure(fmt.Sprintf("web_fetch cannot use %s: %v", final.Redacted(), err)), nil
		}
	}

	text := decode(body, format.charset)
	var header strings.Builder
	switch format.kind {
	case htmlText:
		title, converted := htmlToText(text, final)
		text = converted
		title, _, _ = slice(cleanText(title), 1, maxTitleChars)
		if title == "" {
			title = "(none)"
		}
		fmt.Fprintf(&header, "Title: %s\n", title)
	case plainText:
		// Plain text, Markdown, JSON and XML stay as they are.
	default:
		panic(fmt.Sprintf("web: unknown text kind %d", format.kind))
	}
	fmt.Fprintf(&header, "URL: %s\n", final.Redacted())
	if cut {
		fmt.Fprintf(&header, "Note: the response is larger than %d MiB; only the first %d MiB were read.\n", maxBodyBytes>>20, maxBodyBytes>>20)
	}
	return pageResult(header.String(), cleanText(text), request.offset, request.maxChars), nil
}

// transferFailure turns a failed request or body read into a result. A
// cancelled caller context is a fault for the caller, not for the model.
func transferFailure(ctx, fetchContext context.Context, err error) (tools.Result, error) {
	if ctx.Err() != nil {
		return tools.Result{}, ctx.Err()
	}
	if errors.Is(fetchContext.Err(), context.DeadlineExceeded) {
		return failure(fmt.Sprintf("web_fetch failed: no complete response within %s", fetchTimeout)), nil
	}
	return failure("web_fetch failed: " + err.Error()), nil
}

func statusFailure(ctx, fetchContext context.Context, response *http.Response, final *url.URL) (tools.Result, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes))
	if err != nil {
		return transferFailure(ctx, fetchContext, err)
	}
	set := utf8Charset
	if format, err := parseContentType(response.Header.Get("Content-Type")); err == nil {
		set = format.charset
	}
	message := fmt.Sprintf("HTTP status %s from %s", response.Status, final.Redacted())
	if text := strings.TrimSpace(cleanText(decode(body, set))); text != "" {
		message += "\n\n" + text
	}
	return failure(message), nil
}

// pageResult gives the characters [offset, offset+maxChars) of text after
// the header. It states the total length and the next offset, so the model
// can read the rest.
func pageResult(header, text string, offset, maxChars int) tools.Result {
	part, total, taken := slice(text, offset, maxChars)
	if total == 0 {
		return tools.Result{Content: header + "\nThe page has no text."}
	}
	if offset > total {
		return failure(fmt.Sprintf("offset %d is past the end: the text has %d characters", offset, total))
	}
	last := offset + taken - 1
	var result strings.Builder
	result.Grow(len(header) + len(part) + 160)
	result.WriteString(header)
	fmt.Fprintf(&result, "Characters %d-%d of %d.\n\n", offset, last, total)
	result.WriteString(part)
	if last < total {
		fmt.Fprintf(&result, "\n\n[More text remains. To continue, call web_fetch again with offset %d.]", last+1)
	}
	return tools.Result{Content: result.String()}
}
