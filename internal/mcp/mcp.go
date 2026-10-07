// Package mcp is a client for Model Context Protocol servers. A server is a
// child process that speaks JSON-RPC, one message per line, on its stdin and
// stdout. The model reaches all servers through two tools, so the tool list
// stays the same when servers change.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/tools"
)

const (
	protocolVersion = "2025-06-18"
	startupTimeout  = 30 * time.Second
	// maxResultBytes caps one tool result so a server cannot fill the context
	// window.
	maxResultBytes = 256 * 1024
)

// ServerConfig is one MCP server from the settings.
type ServerConfig struct {
	Name        string // unique; [a-z0-9_-]+
	Command     string
	Args        []string
	Env         map[string]string // added to the parent environment
	Description string            // one line, shown to the model
}

// Manager owns the configured servers. It starts a server only when the model
// first uses it, so a server that the model does not use costs nothing.
type Manager struct {
	servers map[string]*server
	names   []string // sorted
}

type server struct {
	config  ServerConfig
	workDir string

	once    sync.Once
	session *session // set by once

	listMutex sync.Mutex
	listing   string // "" until the first tools/list succeeds
}

// session is the result of the single start attempt of a server.
type session struct {
	ready chan struct{} // closes when the handshake ends
	// connection is nil when the process did not start.
	connection *connection
	err        error // set before ready closes
}

func failedSession(err error) *session {
	ready := make(chan struct{})
	close(ready)
	return &session{ready: ready, err: err}
}

// NewManager checks the configuration. It starts no process.
func NewManager(servers []ServerConfig, workDir string) (*Manager, error) {
	manager := &Manager{servers: make(map[string]*server, len(servers))}
	for index, config := range servers {
		if err := validate(config); err != nil {
			return nil, fmt.Errorf("mcp: server %d: %w", index+1, err)
		}
		if _, taken := manager.servers[config.Name]; taken {
			return nil, fmt.Errorf("mcp: server %d: duplicate name %q", index+1, config.Name)
		}
		// Copies, so a later change by the caller does not change a server.
		config.Args = slices.Clone(config.Args)
		config.Env = maps.Clone(config.Env)
		manager.servers[config.Name] = &server{config: config, workDir: workDir}
		manager.names = append(manager.names, config.Name)
	}
	slices.Sort(manager.names)
	return manager, nil
}

func validate(config ServerConfig) error {
	if !validName(config.Name) {
		return fmt.Errorf("name %q does not match [a-z0-9_-]+", config.Name)
	}
	if config.Command == "" {
		return fmt.Errorf("server %q has no command", config.Name)
	}
	if strings.ContainsAny(config.Description, "\r\n") {
		return fmt.Errorf("server %q: the description must be one line", config.Name)
	}
	for key := range config.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("server %q: environment variable name %q is not valid", config.Name, key)
		}
	}
	return nil
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range []byte(name) {
		switch {
		case 'a' <= character && character <= 'z', '0' <= character && character <= '9', character == '_', character == '-':
		default:
			return false
		}
	}
	return true
}

// Tools returns mcp_list and mcp_call, in this order.
func (m *Manager) Tools() []tools.Tool {
	return []tools.Tool{m.listTool(), m.callTool()}
}

// PromptSection tells the model which servers exist. It is "" when there are
// no servers.
func (m *Manager) PromptSection() string {
	if len(m.names) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString("MCP servers give more tools. Call mcp_list with the server name to get its tools before you use mcp_call.")
	for _, name := range m.names {
		text.WriteString("\n- " + name)
		if description := m.servers[name].config.Description; description != "" {
			text.WriteString(": " + description)
		}
	}
	return text.String()
}

// Close stops every started server. A server that was not started cannot
// start after Close.
func (m *Manager) Close() error {
	failures := make([]error, len(m.names))
	var group sync.WaitGroup
	for index, name := range m.names {
		server := m.servers[name]
		group.Add(1)
		go func() {
			defer group.Done()
			failures[index] = server.close()
		}()
	}
	group.Wait()
	return errors.Join(failures...)
}

func (s *server) close() error {
	s.once.Do(func() { s.session = failedSession(errStopped) })
	if s.session.connection == nil {
		return nil
	}
	if err := s.session.connection.stop(errStopped); err != nil {
		return fmt.Errorf("mcp: stop server %q: %w", s.config.Name, err)
	}
	return nil
}

func (m *Manager) listTool() tools.Tool {
	return tools.Tool{
		Name:        "mcp_list",
		Description: "List the tools of one MCP server. The result gives the name, the description, and the input schema of each tool. Use it before mcp_call. The system prompt lists the servers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"server":{"type":"string","description":"Name of the MCP server."}},"required":["server"]}`),
		ReadOnly:    true,
		Run: func(ctx context.Context, input json.RawMessage) (tools.Result, error) {
			var arguments struct {
				Server string `json:"server"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			server, failure := m.lookup(arguments.Server)
			if server == nil {
				return failure, nil
			}
			listing, err := server.list(ctx)
			if err != nil {
				return serverFailure(ctx, server.config.Name, err)
			}
			return tools.Result{Content: listing}, nil
		},
	}
}

func (m *Manager) callTool() tools.Tool {
	return tools.Tool{
		Name:        "mcp_call",
		Description: "Call one tool of an MCP server. Use mcp_list first to get the tool names and input schemas. The result is the text that the tool returns; other content shows as a one-line placeholder. A result larger than 256 KiB is truncated.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"server":{"type":"string","description":"Name of the MCP server."},"tool":{"type":"string","description":"Name of the tool, from mcp_list."},"arguments":{"type":"object","description":"Arguments for the tool. They must agree with its input schema."}},"required":["server","tool"]}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (tools.Result, error) {
			var arguments struct {
				Server    string          `json:"server"`
				Tool      string          `json:"tool"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			server, failure := m.lookup(arguments.Server)
			if server == nil {
				return failure, nil
			}
			if arguments.Tool == "" {
				return invalidInput(errors.New("tool is required")), nil
			}
			toolArguments := bytes.TrimSpace(arguments.Arguments)
			switch {
			case len(toolArguments) == 0, string(toolArguments) == "null":
				toolArguments = json.RawMessage(`{}`)
			case toolArguments[0] != '{':
				return invalidInput(errors.New("arguments must be a JSON object")), nil
			}
			result, err := server.call(ctx, arguments.Tool, toolArguments)
			if err != nil {
				return serverFailure(ctx, server.config.Name, err)
			}
			return result, nil
		},
	}
}

// lookup returns the server, or a failure result that names the known
// servers when the name is not known.
func (m *Manager) lookup(name string) (*server, tools.Result) {
	if name == "" {
		return nil, invalidInput(errors.New("server is required"))
	}
	if server, found := m.servers[name]; found {
		return server, tools.Result{}
	}
	known := "none"
	if len(m.names) > 0 {
		known = strings.Join(m.names, ", ")
	}
	return nil, tools.Result{Content: fmt.Sprintf("unknown MCP server %q; known servers: %s", name, known), IsError: true}
}

func invalidInput(err error) tools.Result {
	return tools.Result{Content: "invalid input: " + err.Error(), IsError: true}
}

// serverFailure gives a server failure to the model, which can try another
// way. Only a cancelled call stays an error.
func serverFailure(ctx context.Context, name string, err error) (tools.Result, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return tools.Result{}, ctxErr
	}
	return tools.Result{Content: fmt.Sprintf("MCP server %q: %v", name, err), IsError: true}, nil
}

// connect starts the server on first use. Concurrent first calls share one
// start, which goes on when the caller that began it gives up.
func (s *server) connect(ctx context.Context) (*connection, error) {
	s.once.Do(func() { s.session = s.start() })
	select {
	case <-s.session.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.session.err != nil {
		return nil, s.session.err
	}
	if err := s.session.connection.failed(); err != nil {
		return nil, err
	}
	return s.session.connection, nil
}

func (s *server) start() *session {
	connection, err := startConnection(s.config, s.workDir)
	if err != nil {
		return failedSession(fmt.Errorf("start: %w", err))
	}
	started := &session{ready: make(chan struct{}), connection: connection}
	go func() {
		defer close(started.ready)
		if err := initialize(connection); err != nil {
			// The stop error adds nothing: the handshake already failed.
			_ = connection.stop(err)
			started.err = fmt.Errorf("start: %w", err)
		}
	}()
	return started
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    struct{}       `json:"capabilities"`
	ClientInfo      implementation `json:"clientInfo"`
}

func initialize(connection *connection) error {
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	params := initializeParams{ProtocolVersion: protocolVersion, ClientInfo: implementation{Name: "inkling", Version: clientVersion()}}
	_, err := connection.request(ctx, "initialize", params)
	if err == nil {
		err = connection.notify(ctx, "notifications/initialized")
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("initialize: no answer in %s%s", startupTimeout, connection.stderrNote())
	default:
		return fmt.Errorf("initialize: %w", err)
	}
}

func clientVersion() string {
	if info, found := debug.ReadBuildInfo(); found && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

type listParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type toolsPage struct {
	Tools      []toolInfo `json:"tools"`
	NextCursor string     `json:"nextCursor"`
}

type toolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// list returns the cached tool list, or fetches it. Concurrent first calls
// can each fetch it. This costs only some requests, and the lock is not held
// during a request, so a slow server cannot block a cancelled caller.
func (s *server) list(ctx context.Context) (string, error) {
	// Connect first, so a dead server is reported and not hidden by the cache.
	connection, err := s.connect(ctx)
	if err != nil {
		return "", err
	}
	s.listMutex.Lock()
	cached := s.listing
	s.listMutex.Unlock()
	if cached != "" {
		return cached, nil
	}
	var all []toolInfo
	seen := make(map[string]bool)
	for cursor := ""; ; {
		raw, err := connection.request(ctx, "tools/list", listParams{Cursor: cursor})
		if err != nil {
			return "", err
		}
		var page toolsPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", fmt.Errorf("tools/list result: %w", err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return "", fmt.Errorf("tools/list gave the cursor %q again", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	listing := formatTools(s.config.Name, all)
	s.listMutex.Lock()
	s.listing = listing
	s.listMutex.Unlock()
	return listing, nil
}

func formatTools(serverName string, all []toolInfo) string {
	if len(all) == 0 {
		return fmt.Sprintf("MCP server %q has no tools.", serverName)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "MCP server %q has %d tools. Call them with mcp_call and server %q.", serverName, len(all), serverName)
	for _, tool := range all {
		text.WriteString("\n\ntool: " + tool.Name)
		if tool.Description != "" {
			text.WriteString("\ndescription: " + tool.Description)
		}
		text.WriteString("\ninput schema: ")
		var schema bytes.Buffer
		if json.Compact(&schema, tool.InputSchema) == nil && schema.Len() > 0 {
			text.Write(schema.Bytes())
		} else {
			text.WriteString("(none)")
		}
	}
	return capText(text.String())
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type callResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError"`
}

type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Resource *struct {
		URI string `json:"uri"`
	} `json:"resource"`
}

func (s *server) call(ctx context.Context, tool string, arguments json.RawMessage) (tools.Result, error) {
	connection, err := s.connect(ctx)
	if err != nil {
		return tools.Result{}, err
	}
	raw, err := connection.request(ctx, "tools/call", callParams{Name: tool, Arguments: arguments})
	if err != nil {
		return tools.Result{}, err
	}
	var result callResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return tools.Result{}, fmt.Errorf("tools/call result: %w", err)
	}
	parts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		parts = append(parts, item.render())
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = "(no content)"
	}
	return tools.Result{Content: capText(text), IsError: result.IsError}, nil
}

// render gives text items in full and a one-line placeholder for all others.
func (item contentItem) render() string {
	switch item.Type {
	case "text":
		return item.Text
	case "image", "audio":
		if item.MimeType == "" {
			return "[" + item.Type + " content omitted]"
		}
		return "[" + oneLine(item.MimeType) + " content omitted]"
	case "resource_link":
		return "[resource link " + oneLine(item.URI) + "]"
	case "resource":
		uri := ""
		if item.Resource != nil {
			uri = " " + oneLine(item.Resource.URI)
		}
		return "[embedded resource" + uri + " omitted]"
	default:
		// Later protocol versions can add content types.
		label := oneLine(item.Type)
		if label == "" {
			label = "untyped"
		}
		return "[" + label + " content omitted]"
	}
}

// oneLine keeps a server value from breaking a placeholder line.
func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// capText cuts text at a character boundary so the result with its notice
// fits in maxResultBytes.
func capText(text string) string {
	if len(text) <= maxResultBytes {
		return text
	}
	keep := maxResultBytes - 128
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	return text[:keep] + fmt.Sprintf("\n[truncated: 256 KiB output limit; %d of %d bytes omitted]", len(text)-keep, len(text))
}
