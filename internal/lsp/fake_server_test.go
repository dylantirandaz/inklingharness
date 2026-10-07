package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The test binary runs again as a fake language server, so the tests use a
// real child process and real pipes. The second argument is the mode:
//
//   - echo: before each answer, it sends requests to the client and checks
//     the replies. Then it sends stale diagnostics for the previous version
//     and the real ones for the new version.
//   - unversioned: it sends diagnostics without a version.
//   - silent: it sends no diagnostics and ignores shutdown.
//   - garbage: it writes a broken frame after initialized.
const fakeServerArgument = "inkling-fake-language-server"

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == fakeServerArgument {
		os.Exit(runFakeServer(os.Args[2]))
	}
	os.Exit(m.Run())
}

func fakeServer(t *testing.T, mode string) ServerConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Name: "fake", Command: executable, Args: []string{fakeServerArgument, mode}, Extensions: []string{".fake"}}
}

type fakeDocument struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type fakeSyncParams struct {
	TextDocument   fakeDocument `json:"textDocument"`
	ContentChanges []struct {
		Text string `json:"text"`
	} `json:"contentChanges"`
}

func runFakeServer(mode string) int {
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readMessage(reader)
		if err != nil {
			// The client closed stdin.
			return 0
		}
		var message incoming
		if err := json.Unmarshal(body, &message); err != nil {
			return 3
		}
		switch message.Method {
		case "initialize":
			var params initializeParams
			if err := json.Unmarshal(message.Params, &params); err != nil || params.RootURI == "" || len(params.WorkspaceFolders) != 1 || params.WorkspaceFolders[0].URI != params.RootURI || !strings.Contains(string(params.Capabilities), `"versionSupport":true`) {
				fakeSend(errorReply{JSONRPC: "2.0", ID: message.ID, Error: rpcError{Code: codeInvalidParams, Message: "bad initialize params"}})
				continue
			}
			fakeSend(resultReply{JSONRPC: "2.0", ID: message.ID, Result: map[string]any{"capabilities": map[string]any{"textDocumentSync": 1}}})
		case "initialized":
			if mode == "garbage" {
				os.Stdout.WriteString("Content-Length: banana\r\n\r\n")
			}
		case "shutdown":
			if mode != "silent" {
				fakeSend(resultReply{JSONRPC: "2.0", ID: message.ID})
			}
		case "exit":
			return 0
		case "textDocument/didOpen", "textDocument/didChange":
			var params fakeSyncParams
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return 4
			}
			fakeAnswer(mode, reader, message.Method, params)
		}
	}
}

func fakeAnswer(mode string, reader *bufio.Reader, method string, params fakeSyncParams) {
	document := params.TextDocument
	text := document.Text
	if method == "textDocument/didChange" {
		if len(params.ContentChanges) != 1 {
			text = fmt.Sprintf("%d content changes", len(params.ContentChanges))
		} else {
			text = params.ContentChanges[0].Text
		}
	}
	switch mode {
	case "echo":
		checks := fakeAskClient(reader)
		if method == "textDocument/didOpen" && document.LanguageID != "fake" {
			checks = "bad languageId " + document.LanguageID
		}
		if document.Version > 1 {
			stale := document.Version - 1
			fakePublish(document.URI, &stale, "stale")
		}
		version := document.Version
		fakePublish(document.URI, &version, fmt.Sprintf("version %d: %s; %s", version, text, checks))
	case "unversioned":
		fakePublish(document.URI, nil, "unversioned: "+text)
	case "silent", "garbage":
	default:
		panic("unknown fake mode " + mode)
	}
}

// fakeAskClient sends the requests that real servers send and checks the
// replies.
func fakeAskClient(reader *bufio.Reader) string {
	requests := []struct {
		method string
		params any
		want   string
	}{
		{method: "workspace/configuration", params: map[string]any{"items": []any{map[string]any{"section": "a"}, map[string]any{"section": "b"}}}, want: `result [null,null]`},
		{method: "client/registerCapability", params: map[string]any{"registrations": []any{}}, want: `result null`},
		{method: "window/workDoneProgress/create", params: map[string]any{"token": "t"}, want: `result null`},
		{method: "fake/unknown", params: map[string]any{}, want: fmt.Sprintf("error %d", codeMethodNotFound)},
	}
	for index, request := range requests {
		fakeSend(map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("ask-%d", index), "method": request.method, "params": request.params})
	}
	replies := make(map[string]string)
	for len(replies) < len(requests) {
		body, err := readMessage(reader)
		if err != nil {
			return "replies bad: " + err.Error()
		}
		var message incoming
		if err := json.Unmarshal(body, &message); err != nil || message.Method != "" {
			continue
		}
		var id string
		if err := json.Unmarshal(message.ID, &id); err != nil {
			return "replies bad: id " + string(message.ID)
		}
		switch {
		case message.Error != nil:
			replies[id] = fmt.Sprintf("error %d", message.Error.Code)
		default:
			replies[id] = "result " + string(message.Result)
		}
	}
	var problems []string
	for index, request := range requests {
		if got := replies[fmt.Sprintf("ask-%d", index)]; got != request.want {
			problems = append(problems, fmt.Sprintf("%s gave %q, want %q", request.method, got, request.want))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return "replies bad: " + strings.Join(problems, ", ")
	}
	return "replies ok"
}

func fakePublish(uri string, version *int, message string) {
	params := map[string]any{
		"uri": uri,
		"diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}},
			"severity": severityError,
			"message":  message,
		}},
	}
	if version != nil {
		params["version"] = *version
	}
	fakeSend(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": params})
}

func fakeSend(message any) {
	body, err := json.Marshal(message)
	if err != nil {
		panic(err)
	}
	if err := writeMessage(os.Stdout, body); err != nil {
		os.Exit(5)
	}
}
