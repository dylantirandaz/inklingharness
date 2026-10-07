package lsp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// readAll reads messages until the first error, from input that a writer
// goroutine sends through a pipe.
func readAll(t *testing.T, input string) ([]string, error) {
	t.Helper()
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		_, err := io.WriteString(pipeWriter, input)
		pipeWriter.CloseWithError(err)
	}()
	defer pipeReader.Close()
	reader := bufio.NewReader(pipeReader)
	var bodies []string
	for {
		body, err := readMessage(reader)
		if err != nil {
			return bodies, err
		}
		bodies = append(bodies, string(body))
	}
}

func TestReadMessage(t *testing.T) {
	tooLarge := fmt.Sprintf("Content-Length: %d\r\n\r\n", maxMessageBytes+1)
	tests := []struct {
		name   string
		input  string
		bodies []string
		// failure is "eof", "unexpected eof", or a part of the frame error.
		failure string
	}{
		{name: "empty input", input: "", failure: "eof"},
		{name: "one message", input: "Content-Length: 2\r\n\r\n{}", bodies: []string{"{}"}, failure: "eof"},
		{name: "two messages", input: "Content-Length: 2\r\n\r\n{}Content-Length: 4\r\n\r\nnull", bodies: []string{"{}", "null"}, failure: "eof"},
		{name: "body holds a header", input: "Content-Length: 25\r\n\r\nContent-Length: 1\r\n\r\nabcd", bodies: []string{"Content-Length: 1\r\n\r\nabcd"}, failure: "eof"},
		{name: "other header and other case", input: "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\ncontent-LENGTH:  3 \r\n\r\n123", bodies: []string{"123"}, failure: "eof"},
		{name: "bare line feeds", input: "Content-Length: 2\n\n{}", bodies: []string{"{}"}, failure: "eof"},
		{name: "empty body", input: "Content-Length: 0\r\n\r\n", bodies: []string{""}, failure: "eof"},
		{name: "end in header", input: "Content-Len", failure: "unexpected eof"},
		{name: "end before blank line", input: "Content-Length: 2\r\n", failure: "unexpected eof"},
		{name: "end before body", input: "Content-Length: 5\r\n\r\n", failure: "unexpected eof"},
		{name: "end in body", input: "Content-Length: 5\r\n\r\nab", failure: "unexpected eof"},
		{name: "end after good message", input: "Content-Length: 2\r\n\r\n{}Content-Length: 9\r\n\r\n{", bodies: []string{"{}"}, failure: "unexpected eof"},
		{name: "no length", input: "Content-Type: x\r\n\r\n{}", failure: "has no Content-Length"},
		{name: "blank line only", input: "\r\n", failure: "has no Content-Length"},
		{name: "no colon", input: "hello world\r\n\r\n", failure: "has no colon"},
		{name: "log line before frame", input: "starting server\nContent-Length: 2\r\n\r\n{}", failure: "has no colon"},
		{name: "negative length", input: "Content-Length: -1\r\n\r\n", failure: "not a decimal number"},
		{name: "signed length", input: "Content-Length: +2\r\n\r\n{}", failure: "not a decimal number"},
		{name: "length with letters", input: "Content-Length: 2x\r\n\r\n{}", failure: "not a decimal number"},
		{name: "empty length", input: "Content-Length:\r\n\r\n", failure: "not a decimal number"},
		{name: "length overflows", input: "Content-Length: 99999999999999999999999\r\n\r\n", failure: "not a decimal number"},
		{name: "two lengths", input: "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", failure: "more than one Content-Length"},
		{name: "long header line", input: "X-Note: " + strings.Repeat("a", 5000) + "\r\nContent-Length: 2\r\n\r\n{}", failure: "larger than 4 KiB"},
		{name: "many header lines", input: strings.Repeat("X-Note: a\r\n", 500) + "Content-Length: 2\r\n\r\n{}", failure: "larger than 4 KiB"},
		{name: "length over limit", input: tooLarge, failure: "more than the 32 MiB limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bodies, err := readAll(t, test.input)
			if !slices.Equal(bodies, test.bodies) {
				t.Fatalf("bodies = %q, want %q", bodies, test.bodies)
			}
			var frame *frameError
			switch test.failure {
			case "eof":
				if err != io.EOF {
					t.Fatalf("error = %v, want io.EOF", err)
				}
			case "unexpected eof":
				if err != io.ErrUnexpectedEOF {
					t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
				}
			default:
				if !errors.As(err, &frame) || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("error = %v, want a frame error with %q", err, test.failure)
				}
			}
		})
	}
}

// TestReadMessageSizeLimit checks that the reader refuses a large body from
// the header alone and accepts a body of exactly the limit.
func TestReadMessageSizeLimit(t *testing.T) {
	t.Run("refused before the body", func(t *testing.T) {
		pipeReader, pipeWriter := io.Pipe()
		defer pipeReader.Close()
		// The stream stays open, so a reader that waits for the body blocks.
		go io.WriteString(pipeWriter, fmt.Sprintf("Content-Length: %d\r\n\r\n", maxMessageBytes+1))
		result := make(chan error, 1)
		go func() {
			_, err := readMessage(bufio.NewReader(pipeReader))
			result <- err
		}()
		select {
		case err := <-result:
			var frame *frameError
			if !errors.As(err, &frame) {
				t.Fatalf("error = %v, want a frame error", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the reader waits for the body of a message over the limit")
		}
	})
	t.Run("exact limit", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), maxMessageBytes)
		pipeReader, pipeWriter := io.Pipe()
		defer pipeReader.Close()
		go func() {
			pipeWriter.CloseWithError(writeMessage(pipeWriter, body))
		}()
		got, err := readMessage(bufio.NewReader(pipeReader))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("got %d bytes, want %d", len(got), len(body))
		}
	})
}

func TestWriteMessageRoundTrip(t *testing.T) {
	bodies := []string{`{"jsonrpc":"2.0"}`, "", "Content-Length: 3\r\n\r\nabc", "ünïcödé ✓", strings.Repeat("y", 100_000)}
	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()
	go func() {
		for _, body := range bodies {
			if err := writeMessage(pipeWriter, []byte(body)); err != nil {
				pipeWriter.CloseWithError(err)
				return
			}
		}
		pipeWriter.Close()
	}()
	reader := bufio.NewReader(pipeReader)
	for index, want := range bodies {
		got, err := readMessage(reader)
		if err != nil {
			t.Fatalf("message %d: %v", index, err)
		}
		if string(got) != want {
			t.Fatalf("message %d = %.40q, want %.40q", index, got, want)
		}
	}
	if _, err := readMessage(reader); err != io.EOF {
		t.Fatalf("after the last message: error = %v, want io.EOF", err)
	}
}
