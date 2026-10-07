package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary runs again as a fake MCP server, so the tests use a real
// child process and real pipes. The variable selects the role of the process.
const fakeRoleVariable = "INKLING_MCP_FAKE_ROLE"

// Settings of the fake server, given through ServerConfig.Env.
const (
	// markerVariable names a file. The server adds "<pid> <pgid> <dir>" to it
	// when it starts.
	markerVariable = "FAKE_MCP_MARKER"
	// eventLogVariable names a file. The server adds "sleep <id>" for a sleep
	// call and "cancelled <id>" for a cancellation.
	eventLogVariable = "FAKE_MCP_EVENT_LOG"
	// failStartVariable makes the server write its value to stderr and exit
	// with status 2 before the handshake.
	failStartVariable = "FAKE_MCP_FAIL_START"
	// stubbornVariable makes the server ignore the end of stdin. It also
	// starts a child in its process group and writes the child pid to the
	// file that the variable names.
	stubbornVariable = "FAKE_MCP_STUBBORN_CHILD_FILE"
	// cursorLoopVariable makes tools/list give the same cursor forever.
	cursorLoopVariable = "FAKE_MCP_CURSOR_LOOP"
)

func TestMain(m *testing.M) {
	switch os.Getenv(fakeRoleVariable) {
	case "":
		os.Exit(m.Run())
	case "server":
		os.Exit(runFakeServer())
	case "sleeper":
		time.Sleep(time.Hour)
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown fake role")
		os.Exit(2)
	}
}

// fakeToolPages are the pages of tools/list. The cursor of a page is its index.
var fakeToolPages = [][]string{{"echo", "mixed", "fail"}, {"rpc_error", "ping"}, {"sleep", "crash", "sized"}}

type fakeMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

type fakeServer struct {
	initialized bool
	// pingCallID is the tools/call that waits for the answer to a ping.
	pingCallID json.RawMessage
}

// stdoutMutex keeps the lines of concurrent answers apart.
var stdoutMutex sync.Mutex

func runFakeServer() int {
	if marker := os.Getenv(markerVariable); marker != "" {
		directory, err := os.Getwd()
		if err != nil {
			return 10
		}
		if err := appendLine(marker, fmt.Sprintf("%d %d %s", os.Getpid(), syscall.Getpgrp(), directory)); err != nil {
			return 11
		}
	}
	if message := os.Getenv(failStartVariable); message != "" {
		fmt.Fprintln(os.Stderr, message)
		return 2
	}
	childFile := os.Getenv(stubbornVariable)
	if childFile != "" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), fakeRoleVariable+"=sleeper")
		if err := child.Start(); err != nil {
			return 12
		}
		if err := os.WriteFile(childFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			return 13
		}
	}
	fake := fakeServer{}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var message fakeMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			fmt.Fprintln(os.Stderr, "fake: bad line:", err)
			return 1
		}
		if code, exit := fake.handle(message); exit {
			return code
		}
	}
	if childFile != "" {
		time.Sleep(time.Hour)
	}
	return 0
}

// handle answers one message. It returns true when the server must exit.
func (f *fakeServer) handle(message fakeMessage) (int, bool) {
	switch {
	case message.Method == "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil || params.ProtocolVersion != "2025-06-18" || params.ClientInfo.Name != "inkling" || params.ClientInfo.Version == "" {
			respondError(message.ID, -32602, "bad initialize params: "+string(message.Params))
			return 0, false
		}
		// The client must skip a log line and a notification.
		writeLine("fake server log line")
		writeMessage(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info", "data": "hello"}})
		respond(message.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}})
	case message.Method == "notifications/initialized":
		f.initialized = true
	case message.Method == "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return 14, true
		}
		if err := logEvent("cancelled " + string(params.RequestID)); err != nil {
			return 15, true
		}
	case message.Method != "" && !f.initialized:
		respondError(message.ID, -32002, "not initialized")
	case message.Method == "tools/list":
		f.list(message)
	case message.Method == "tools/call":
		return f.call(message)
	case message.Method != "":
		respondError(message.ID, -32601, "method not found")
	case string(message.ID) == `"server-ping"`:
		if string(message.Result) == "{}" {
			respond(f.pingCallID, textContent("pong"))
		} else {
			respond(f.pingCallID, textContent("bad ping reply: "+string(message.Result)))
		}
	default:
		fmt.Fprintln(os.Stderr, "fake: unexpected message")
		return 1, true
	}
	return 0, false
}

func (f *fakeServer) list(message fakeMessage) {
	var params struct {
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		respondError(message.ID, -32602, "bad params")
		return
	}
	index := 0
	if params.Cursor != "" {
		parsed, err := strconv.Atoi(params.Cursor)
		if err != nil || parsed < 1 || parsed >= len(fakeToolPages) {
			respondError(message.ID, -32602, "bad cursor")
			return
		}
		index = parsed
	}
	var page []map[string]any
	for _, name := range fakeToolPages[index] {
		schema := json.RawMessage(`{"type":"object"}`)
		if name == "echo" {
			schema = json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
		}
		page = append(page, map[string]any{"name": name, "description": "Fake tool " + name + ".", "inputSchema": schema})
	}
	result := map[string]any{"tools": page}
	switch {
	case os.Getenv(cursorLoopVariable) != "":
		result["nextCursor"] = "1"
	case index+1 < len(fakeToolPages):
		result["nextCursor"] = strconv.Itoa(index + 1)
	}
	respond(message.ID, result)
}

func (f *fakeServer) call(message fakeMessage) (int, bool) {
	var params struct {
		Name      string `json:"name"`
		Arguments struct {
			Text              string `json:"text"`
			DelayMilliseconds int    `json:"delay_milliseconds"`
			Bytes             int    `json:"bytes"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		respondError(message.ID, -32602, "bad params")
		return 0, false
	}
	switch params.Name {
	case "echo":
		// A delayed answer comes after the answers to later calls.
		delay := time.Duration(params.Arguments.DelayMilliseconds) * time.Millisecond
		go func() {
			time.Sleep(delay)
			respond(message.ID, textContent(params.Arguments.Text))
		}()
	case "mixed":
		respond(message.ID, map[string]any{"content": []map[string]any{
			{"type": "text", "text": "first"},
			{"type": "image", "data": "iVBORw0KGgo=", "mimeType": "image/png"},
			{"type": "audio", "data": "UklGRg==", "mimeType": "audio/wav"},
			{"type": "resource_link", "uri": "file:///tmp/a.txt", "name": "a.txt"},
			{"type": "resource", "resource": map[string]any{"uri": "file:///tmp/b.txt", "text": "hidden"}},
			{"type": "widget"},
			{"type": "text", "text": "last"},
		}})
	case "fail":
		respond(message.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "it failed"}}, "isError": true})
	case "rpc_error":
		respondError(message.ID, -32602, "bad arguments")
	case "ping":
		f.pingCallID = message.ID
		writeMessage(map[string]any{"jsonrpc": "2.0", "id": "server-ping", "method": "ping"})
	case "sleep":
		// The server never answers. Only a cancellation ends the call.
		if err := logEvent("sleep " + string(message.ID)); err != nil {
			return 16, true
		}
	case "crash":
		fmt.Fprintln(os.Stderr, "fatal: boom")
		return 3, true
	case "sized":
		// The line, without its newline, has exactly the requested length.
		prefix := `{"jsonrpc":"2.0","id":` + string(message.ID) + `,"result":{"content":[{"type":"text","text":"`
		suffix := `"}]}}`
		writeLine(prefix + strings.Repeat("x", params.Arguments.Bytes-len(prefix)-len(suffix)) + suffix)
	default:
		respondError(message.ID, -32602, "unknown tool "+params.Name)
	}
	return 0, false
}

func textContent(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func respond(id json.RawMessage, result any) {
	writeMessage(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func respondError(id json.RawMessage, code int, text string) {
	writeMessage(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": text}})
}

func writeMessage(message map[string]any) {
	line, err := json.Marshal(message)
	if err != nil {
		panic(err)
	}
	writeLine(string(line))
}

func writeLine(line string) {
	stdoutMutex.Lock()
	defer stdoutMutex.Unlock()
	if _, err := os.Stdout.WriteString(line + "\n"); err != nil {
		os.Exit(17)
	}
}

// logEvent records an event when the test asks for the event log.
func logEvent(line string) error {
	path := os.Getenv(eventLogVariable)
	if path == "" {
		return nil
	}
	return appendLine(path, line)
}

func appendLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(line + "\n"); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
