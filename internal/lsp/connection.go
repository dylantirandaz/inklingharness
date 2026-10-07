package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// stopDelay is how long a server gets to shut down and exit, and how long
	// the reader gets to take the last messages after the server exits.
	stopDelay = 2 * time.Second
	// stderrTailBytes is the part of the stderr output that failure reasons
	// show. Servers often write the cause of a crash there.
	stderrTailBytes = 2 << 10
)

// JSON-RPC error codes.
const (
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

// connection is one server process and its JSON-RPC session. Three
// goroutines serve it: the writer owns stdin, the reader owns stdout, and the
// waiter reaps the process.
type connection struct {
	command *exec.Cmd
	stdin   *os.File
	stdout  *os.File
	stderr  *tailBuffer

	// outgoing carries message bodies and has no buffer. A body that the
	// writer accepted is written, so a sender that gives up before the
	// hand-off leaves the session state unchanged.
	outgoing chan []byte
	nextID   atomic.Int64

	pendingMutex sync.Mutex
	pending      map[int64]chan response

	documentsMutex sync.Mutex
	documents      map[string]*document // by clean absolute path
	// published closes, and a new channel takes its place, when a document
	// gets diagnostics.
	published chan struct{}

	// stopping is set when the client stops the server, so the exit of the
	// server is not a crash.
	stopping   atomic.Bool
	readerDone chan struct{}
	exited     chan struct{}
	// dead closes when the session can carry no more messages. failure is
	// set before, and tells why.
	dead     chan struct{}
	failOnce sync.Once
	failure  error
}

// document is one file that the client opened on the server.
type document struct {
	// version is the last version that the writer accepted. It is 0 until
	// didOpen.
	version int
	// publications counts the diagnostics notifications for the file.
	publications int
	// publishedVersion is the version in the last notification. It is nil
	// when the server did not send one.
	publishedVersion *int
	diagnostics      []diagnostic // from the last notification
}

// response is the outcome of one request: a result or an error, never both.
type response struct {
	result json.RawMessage
	err    error
}

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
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type outgoingRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type outgoingNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// resultReply has no omitempty: a null result must be present.
type resultReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type errorReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcError        `json:"error"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type versionedDocument struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// contentChange has no range, so it replaces the full text.
type contentChange struct {
	Text string `json:"text"`
}

type didChangeParams struct {
	TextDocument   versionedDocument `json:"textDocument"`
	ContentChanges []contentChange   `json:"contentChanges"`
}

type publishParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version"`
	Diagnostics []diagnostic `json:"diagnostics"`
}

type configurationParams struct {
	Items []json.RawMessage `json:"items"`
}

// startConnection starts the process. It does not do the LSP handshake.
func startConnection(program string, arguments []string, root string) (*connection, error) {
	command := exec.Command(program, arguments...)
	command.Dir = root
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
	// process exits, and the reader can then lose the last messages.
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
		documents:  make(map[string]*document),
		published:  make(chan struct{}),
		readerDone: make(chan struct{}),
		exited:     make(chan struct{}),
		dead:       make(chan struct{}),
	}
	go c.write()
	go c.read()
	go c.wait()
	return c, nil
}

// request sends one request and waits for its response.
func (c *connection) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	body, err := json.Marshal(outgoingRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
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

	if err := c.send(ctx, body); err != nil {
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
		// The client sends only initialize and shutdown. The protocol does not
		// let a client cancel initialize, and a shutdown that is late ends in
		// a kill, so no cancellation goes to the server.
		return nil, ctx.Err()
	}
}

func (c *connection) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(outgoingNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.send(ctx, body)
}

// send gives a message body to the writer.
func (c *connection) send(ctx context.Context, body []byte) error {
	select {
	case c.outgoing <- body:
		return nil
	case <-c.dead:
		return c.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}

// diagnose gives the text of a file to the server and waits for the
// diagnostics of that text. The caller must not call it for the same
// connection from two goroutines at a time, because each call takes the next
// version.
func (c *connection) diagnose(ctx context.Context, path, text string) ([]diagnostic, error) {
	c.documentsMutex.Lock()
	file, known := c.documents[path]
	if !known {
		file = &document{}
		c.documents[path] = file
	}
	// A failed didOpen leaves version 0, so the next call sends didOpen again.
	opened := file.version > 0
	version := file.version + 1
	// A notification without version that comes after this point is the
	// answer to this text.
	mark := file.publications
	c.documentsMutex.Unlock()

	uri := fileURI(path)
	var err error
	if opened {
		err = c.notify(ctx, "textDocument/didChange", didChangeParams{
			TextDocument:   versionedDocument{URI: uri, Version: version},
			ContentChanges: []contentChange{{Text: text}},
		})
	} else {
		err = c.notify(ctx, "textDocument/didOpen", didOpenParams{
			TextDocument: textDocumentItem{URI: uri, LanguageID: languageID(strings.ToLower(filepath.Ext(path))), Version: version, Text: text},
		})
	}
	if err != nil {
		return nil, err
	}
	c.documentsMutex.Lock()
	file.version = version
	c.documentsMutex.Unlock()

	for {
		c.documentsMutex.Lock()
		current := file.publications > mark && (file.publishedVersion == nil || *file.publishedVersion >= version)
		diagnostics := file.diagnostics
		published := c.published
		c.documentsMutex.Unlock()
		if current {
			return diagnostics, nil
		}
		select {
		case <-published:
		case <-c.dead:
			return nil, c.failure
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// stop asks the server to shut down and exit. Then it kills the process
// group, so children of the server stop too. It returns after the process is
// reaped.
func (c *connection) stop() error {
	c.stopping.Store(true)
	if c.failed() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), stopDelay)
		if _, err := c.request(ctx, "shutdown", nil); err == nil {
			if c.notify(ctx, "exit", nil) == nil {
				select {
				case <-c.exited:
				case <-ctx.Done():
				}
			}
		}
		cancel()
	}
	return c.terminate(errClosed)
}

// terminate ends the session and kills the process group. It returns after
// the process is reaped.
func (c *connection) terminate(cause error) error {
	c.fail(cause)
	err := c.kill()
	<-c.exited
	return err
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
		case body := <-c.outgoing:
			if err := writeMessage(c.stdin, body); err != nil {
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
	reader := bufio.NewReaderSize(c.stdout, 64<<10)
	for {
		body, err := readMessage(reader)
		if frame := (*frameError)(nil); errors.As(err, &frame) {
			c.fail(fmt.Errorf("the server broke the protocol: %w", err))
			c.kill()
			return
		}
		if err != nil {
			c.awaitExit(errors.New("the server closed its output but did not exit"))
			return
		}
		c.dispatch(body)
	}
}

func (c *connection) dispatch(body []byte) {
	var message incoming
	if err := json.Unmarshal(body, &message); err != nil {
		// The frame was sound, so the next message can still be read.
		return
	}
	hasID := len(message.ID) > 0 && string(message.ID) != "null"
	switch {
	case message.Method != "" && hasID:
		c.answer(message)
	case message.Method == "textDocument/publishDiagnostics":
		c.publish(message.Params)
	case message.Method != "":
		// Other notifications, for example logs and progress, carry nothing
		// that this client uses.
	case hasID:
		c.deliver(message)
	default:
		// A message without method and id is not JSON-RPC. Skip it.
	}
}

// answer replies to a request from the server.
func (c *connection) answer(request incoming) {
	var reply any
	switch request.Method {
	case "workspace/configuration":
		var params configurationParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			reply = errorReply{JSONRPC: "2.0", ID: request.ID, Error: rpcError{Code: codeInvalidParams, Message: err.Error()}}
			break
		}
		// A null item tells the server to use its default settings.
		reply = resultReply{JSONRPC: "2.0", ID: request.ID, Result: make([]any, len(params.Items))}
	case "client/registerCapability", "window/workDoneProgress/create":
		reply = resultReply{JSONRPC: "2.0", ID: request.ID}
	default:
		reply = errorReply{JSONRPC: "2.0", ID: request.ID, Error: rpcError{Code: codeMethodNotFound, Message: "the client does not support " + request.Method}}
	}
	body, err := json.Marshal(reply)
	if err != nil {
		panic(fmt.Sprintf("lsp: encode a reply: %v", err))
	}
	// The reader must not wait for the writer: the writer can wait for a
	// server that waits for the reader.
	go c.send(context.Background(), body)
}

// publish keeps the diagnostics of an opened file and wakes the waiters.
func (c *connection) publish(raw json.RawMessage) {
	var params publishParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	path, valid := uriPath(params.URI)
	if !valid {
		return
	}
	c.documentsMutex.Lock()
	defer c.documentsMutex.Unlock()
	file, opened := c.documents[path]
	if !opened {
		// Servers also report files that the client did not open. Nobody
		// waits for them.
		return
	}
	file.publications++
	file.publishedVersion = params.Version
	file.diagnostics = params.Diagnostics
	close(c.published)
	c.published = make(chan struct{})
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
		// The request gave up.
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
	if c.stopping.Load() {
		c.fail(errClosed)
		return
	}
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
