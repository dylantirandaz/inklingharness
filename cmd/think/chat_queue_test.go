//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func ioctl(fd, request, argument uintptr) error {
	if _, _, code := syscall.Syscall(syscall.SYS_IOCTL, fd, request, argument); code != 0 {
		return code
	}
	return nil
}

// openTestTerminal opens a pseudo-terminal of rows by cols cells and closes
// it when the test ends.
func openTestTerminal(t *testing.T, rows, cols int) (master, slave *os.File) {
	t.Helper()
	master, path, err := openPseudoTerminal()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	slave, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatal(err)
	}
	size := struct{ rows, columns, horizontalPixels, verticalPixels uint16 }{rows: uint16(rows), columns: uint16(cols)}
	if err := ioctl(slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		slave.Close()
		master.Close()
	})
	return master, slave
}

// screenOutput collects what a program writes, for reads from another
// goroutine.
type screenOutput struct {
	mutex sync.Mutex
	data  bytes.Buffer
}

func (output *screenOutput) Write(data []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.data.Write(data)
}

func (output *screenOutput) String() string {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.data.String()
}

// waitFor waits until the output holds text.
func (output *screenOutput) waitFor(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the terminal never showed %q", text)
}

func sse(event, payload string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload)
}

// reply is one model reply in the event stream of the Messages API: text,
// then an optional read_file call, then the stop reason.
func reply(text, toolInput, stop string) string {
	stream := sse("message_start", `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":10,"output_tokens":1}}}`) +
		sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		sse("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text)) +
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`)
	if toolInput != "" {
		stream += sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`) +
			sse("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":%q}}`, toolInput)) +
			sse("content_block_stop", `{"type":"content_block_stop","index":1}`)
	}
	return stream + sse("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":5}}`, stop)) +
		sse("message_stop", `{"type":"message_stop"}`)
}

// lastUserBlocks returns the type of each block in the last message of a
// request, or the text for a text block.
func lastUserBlocks(t *testing.T, body []byte) []string {
	t.Helper()
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("last message role = %q", last.Role)
	}
	var blocks []string
	for _, block := range last.Content {
		if block.Type == "text" {
			blocks = append(blocks, block.Text)
			continue
		}
		blocks = append(blocks, block.Type)
	}
	return blocks
}

// A steering message typed while the first request runs reaches the model
// after the tool result, and a message queued for later starts the next turn
// with no prompt typed.
func TestChatQueueSteersAndFollowsUp(t *testing.T) {
	master, slave := openTestTerminal(t, 30, 100)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(keyEnvironmentVariable, "dummy")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	t.Setenv(motionVariable, "off")

	requests := make(chan []byte, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)
	replies := []string{
		reply("reading", `{"path":"a.txt"}`, "tool_use"),
		reply("steered answer", "", "end_turn"),
		reply("follow-up answer", "", "end_turn"),
	}
	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		number := int(served.Add(1))
		if number > len(replies) {
			t.Errorf("unexpected request %d", number)
			return
		}
		requests <- body
		if number == 1 {
			<-release
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, replies[number-1])
	}))
	defer server.Close()

	output, stderr := &screenOutput{}, &screenOutput{}
	go func() { _, _ = io.Copy(output, master) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go func() { exited <- chatCommand(ctx, []string{"-base-url", server.URL}, slave, slave, stderr) }()
	next := func() []byte {
		t.Helper()
		select {
		case body := <-requests:
			return body
		case <-time.After(10 * time.Second):
			t.Fatalf("no request came; errors: %s", stderr.String())
			return nil
		}
	}

	output.waitFor(t, promptPlaceholder)
	if _, err := master.WriteString("read a\r"); err != nil {
		t.Fatal(err)
	}
	next()
	if _, err := master.WriteString("make the fix minimal\ralso update the README\x1b\r"); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "steer  make the fix minimal")
	output.waitFor(t, "later  also update the README")
	releaseFirst()

	if got := lastUserBlocks(t, next()); fmt.Sprint(got) != fmt.Sprint([]string{"tool_result", "make the fix minimal"}) {
		t.Fatalf("request after the tool ends with %q", got)
	}
	if got := lastUserBlocks(t, next()); fmt.Sprint(got) != fmt.Sprint([]string{"also update the README"}) {
		t.Fatalf("follow-up request ends with %q", got)
	}
	output.waitFor(t, "follow-up answer")
	cancel()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("chat did not end")
	}
}
