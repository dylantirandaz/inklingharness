package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// stopDelay is how long a server gets to exit after its stdin closes, and
	// how long the reader gets to take the last lines after the server exits.
	stopDelay = 2 * time.Second
	// maxLineBytes caps one message from a server, without the newline, so a
	// broken server cannot fill the memory.
	maxLineBytes = 16 << 20
	// stderrTailBytes is the part of the stderr output that error messages
	// show. Servers often write the cause of a failure there.
	stderrTailBytes = 4 << 10
)

var errStopped = errors.New("the server is stopped")

// connection is one server process and its JSON-RPC session. Three
// goroutines serve it: the writer owns stdin, the reader owns stdout, and the
// waiter reaps the process.
type connection struct {
	command *exec.Cmd
	stdin   *os.File
	stdout  *os.File
	stderr  *tailBuffer

	// outgoing has no buffer. A line that the writer accepted is written,
	// so a request that gives up before the hand-off needs no cancellation.
	outgoing chan []byte
	nextID   atomic.Int64

	pendingMutex sync.Mutex
	pending      map[int64]chan response

	readerDone chan struct{}
	exited     chan struct{}
	// dead closes when the session can carry no more messages. failure is
	// set before, and tells why.
	dead     chan struct{}
	failOnce sync.Once
	failure  error
}

// response is the outcome of one request: a result or an error, never both.
type response struct {
	result json.RawMessage
	err    error
}

// rpcError is a JSON-RPC error response. It is an expected failure, for
// example a bad tool argument, so the model receives it.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// incoming holds every field that tells the kinds of server messages apart.
type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type outgoingRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type outgoingNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type emptyReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  struct{}        `json:"result"`
}

type cancelledParams struct {
	RequestID int64  `json:"requestId"`
	Reason    string `json:"reason"`
}

// startConnection starts the process. It does not do the MCP handshake.
func startConnection(config ServerConfig, workDir string) (*connection, error) {
	command := exec.Command(config.Command, config.Args...)
	command.Dir = workDir
	command.Env = environment(config.Env)
	// The server gets its own process group, so a stop kills its children too.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A child of the server can keep stderr open after the server exits.
	command.WaitDelay = stopDelay
	stderr := &tailBuffer{}
	command.Stderr = stderr

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdinReader.Close()
		stdinWriter.Close()
		return nil, err
	}
	// Own pipes, not StdinPipe and StdoutPipe: Wait closes those when the
	// process exits, and the reader can then lose the last response.
	command.Stdin = stdinReader
	command.Stdout = stdoutWriter
	err = command.Start()
	// The child has its own copies of these ends. The parent must close them,
	// or the reader never sees the end of the output.
	stdinReader.Close()
	stdoutWriter.Close()
	if err != nil {
		stdinWriter.Close()
		stdoutReader.Close()
		return nil, err
	}

	c := &connection{
		command:    command,
		stdin:      stdinWriter,
		stdout:     stdoutReader,
		stderr:     stderr,
		outgoing:   make(chan []byte),
		pending:    make(map[int64]chan response),
		readerDone: make(chan struct{}),
		exited:     make(chan struct{}),
		dead:       make(chan struct{}),
	}
	go c.write()
	go c.read()
	go c.wait()
	return c, nil
}

// environment adds the configured variables to the parent environment. A
// later entry wins over an earlier one with the same key.
func environment(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	variables := os.Environ()
	for _, key := range slices.Sorted(maps.Keys(extra)) {
		variables = append(variables, key+"="+extra[key])
	}
	return variables
}

// request sends one request and waits for its response.
func (c *connection) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	line, err := encodeLine(outgoingRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	replies := make(chan response, 1)
	c.pendingMutex.Lock()
	c.pending[id] = replies
	c.pendingMutex.Unlock()
	defer func() {
		c.pendingMutex.Lock()
		delete(c.pending, id)
		c.pendingMutex.Unlock()
	}()

	if err := c.send(ctx, line); err != nil {
		return nil, err
	}
	select {
	case reply := <-replies:
		return reply.result, reply.err
	case <-c.dead:
		// A response can arrive just before the server exits.
		select {
		case reply := <-replies:
			return reply.result, reply.err
		default:
			return nil, c.failure
		}
	case <-ctx.Done():
		// The protocol does not let a client cancel initialize.
		if method != "initialize" {
			if notice, err := encodeLine(outgoingNotification{JSONRPC: "2.0", Method: "notifications/cancelled", Params: cancelledParams{RequestID: id, Reason: "the client cancelled the request"}}); err == nil {
				// The caller must not wait for a writer that the server blocks.
				go c.send(context.Background(), notice)
			}
		}
		return nil, ctx.Err()
	}
}

func (c *connection) notify(ctx context.Context, method string) error {
	line, err := encodeLine(outgoingNotification{JSONRPC: "2.0", Method: method})
	if err != nil {
		return err
	}
	return c.send(ctx, line)
}

// send gives a line to the writer.
func (c *connection) send(ctx context.Context, line []byte) error {
	select {
	case c.outgoing <- line:
		return nil
	case <-c.dead:
		return c.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}

func encodeLine(message any) ([]byte, error) {
	line, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	// json.Marshal escapes control characters, so the line has no newline.
	return append(line, '\n'), nil
}

// failed returns the failure of a dead session, or nil.
func (c *connection) failed() error {
	select {
	case <-c.dead:
		return c.failure
	default:
		return nil
	}
}

// fail ends the session. The first cause wins, because later causes are
// usually results of the first.
func (c *connection) fail(cause error) {
	c.failOnce.Do(func() {
		c.failure = cause
		close(c.dead)
	})
}

func (c *connection) write() {
	defer c.stdin.Close()
	for {
		select {
		case line := <-c.outgoing:
			if _, err := c.stdin.Write(line); err != nil {
				c.awaitExit(fmt.Errorf("write to the server: %w", err))
				return
			}
		case <-c.dead:
			// A closed stdin tells the server to exit.
			return
		}
	}
}

func (c *connection) read() {
	defer close(c.readerDone)
	defer c.stdout.Close()
	scanner := bufio.NewScanner(c.stdout)
	// The buffer holds the newline too.
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes+1)
	for scanner.Scan() {
		c.dispatch(scanner.Bytes())
	}
	if errors.Is(scanner.Err(), bufio.ErrTooLong) {
		c.fail(errors.New("the server sent a message line larger than 16 MiB"))
		c.kill()
		return
	}
	c.awaitExit(errors.New("the server closed its output but did not exit"))
}

func (c *connection) dispatch(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var message incoming
	if err := json.Unmarshal(line, &message); err != nil {
		// Some servers write log lines to stdout against the protocol. Skip
		// them, so that such a server stays usable.
		return
	}
	hasID := len(message.ID) > 0 && string(message.ID) != "null"
	switch {
	case message.Method != "" && hasID:
		// The client declares no capabilities, so ping is the only request
		// that a server can send to it.
		if message.Method == "ping" {
			if reply, err := encodeLine(emptyReply{JSONRPC: "2.0", ID: message.ID, Result: struct{}{}}); err == nil {
				// The reader must not wait for the writer: the writer can wait
				// for a server that waits for the reader.
				go c.send(context.Background(), reply)
			}
		}
	case message.Method != "":
		// Notifications carry nothing that this client uses.
	case hasID:
		c.deliver(message)
	default:
		// A message without method and id is not JSON-RPC. Skip it like a log line.
	}
}

func (c *connection) deliver(message incoming) {
	id, err := strconv.ParseInt(string(message.ID), 10, 64)
	if err != nil {
		// The client sends only integer ids, so this response is not for it.
		return
	}
	c.pendingMutex.Lock()
	replies, found := c.pending[id]
	delete(c.pending, id)
	c.pendingMutex.Unlock()
	if !found {
		// The request was cancelled.
		return
	}
	switch {
	case message.Error != nil:
		replies <- response{err: message.Error}
	case len(message.Result) == 0:
		replies <- response{err: errors.New("the server sent a response without result or error")}
	default:
		replies <- response{result: message.Result}
	}
}

func (c *connection) wait() {
	waitErr := c.command.Wait()
	close(c.exited)
	timer := time.NewTimer(stopDelay)
	select {
	case <-c.readerDone:
	case <-timer.C:
		// A child of the server keeps stdout open.
		c.stdout.Close()
		<-c.readerDone
	}
	timer.Stop()
	var status string
	switch state := c.command.ProcessState; state {
	case nil:
		// Wait failed before it could get the status.
		status = waitErr.Error()
	default:
		status = state.String()
	}
	c.fail(fmt.Errorf("the server exited (%s)%s", status, c.stderrNote()))
}

// awaitExit gives a server that broke its pipe time to exit. The exit status
// tells more than the broken pipe, so the waiter reports it.
func (c *connection) awaitExit(cause error) {
	timer := time.NewTimer(stopDelay)
	defer timer.Stop()
	select {
	case <-c.exited:
	case <-timer.C:
		c.fail(cause)
		c.kill()
	}
}

// stop ends the session, closes stdin, and kills the process group when the
// server does not exit in time. It returns after the process is reaped.
func (c *connection) stop(cause error) error {
	c.fail(cause)
	timer := time.NewTimer(stopDelay)
	defer timer.Stop()
	select {
	case <-c.exited:
		return nil
	case <-timer.C:
	}
	err := c.kill()
	<-c.exited
	return err
}

func (c *connection) kill() error {
	err := syscall.Kill(-c.command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (c *connection) stderrNote() string {
	tail := c.stderr.String()
	if tail == "" {
		return "; stderr is empty"
	}
	return "; stderr tail:\n" + tail
}

// tailBuffer keeps the last bytes that a server wrote to stderr.
type tailBuffer struct {
	mutex sync.Mutex
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	written := len(p)
	if len(p) >= stderrTailBytes {
		b.data = append(b.data[:0], p[len(p)-stderrTailBytes:]...)
		return written, nil
	}
	b.data = append(b.data, p...)
	if excess := len(b.data) - stderrTailBytes; excess > 0 {
		b.data = append(b.data[:0], b.data[excess:]...)
	}
	return written, nil
}

// String drops the partial character that a cut can leave at the start.
func (b *tailBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(b.data), ""))
}
