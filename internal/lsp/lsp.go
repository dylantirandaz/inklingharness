// Package lsp is a small Language Server Protocol client. It gives a file
// that the agent wrote to a language server and returns the errors and
// warnings that the server reports, so the agent sees them at once.
package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// startupTimeout caps the initialize handshake. It goes on in the
	// background when a Diagnose call stops waiting for it.
	startupTimeout = 30 * time.Second
	// firstWait is the budget of the first file of a server, which often
	// loads the workspace before it reports.
	firstWait = 15 * time.Second
	// laterWait is the budget of all other files.
	laterWait = 3 * time.Second
)

// clientCapabilities asks for the version in publishDiagnostics, so the
// client can tell the diagnostics of the new text from those of the old.
var clientCapabilities = json.RawMessage(`{"textDocument":{"publishDiagnostics":{"versionSupport":true}}}`)

var errClosed = errors.New("lsp: the manager is closed")

// ServerConfig is one language server.
type ServerConfig struct {
	Name       string // unique; [a-z0-9_-]+
	Command    string // a name in PATH, found at first use, or an absolute path
	Args       []string
	Extensions []string // lowercase, with the dot, for example ".go"; unique over all servers
}

// DefaultServers gives the usual servers for Go, Rust, TypeScript and
// JavaScript, Python, and C and C++.
func DefaultServers() []ServerConfig {
	return []ServerConfig{
		{Name: "gopls", Command: "gopls", Extensions: []string{".go"}},
		{Name: "rust-analyzer", Command: "rust-analyzer", Extensions: []string{".rs"}},
		{Name: "typescript-language-server", Command: "typescript-language-server", Args: []string{"--stdio"}, Extensions: []string{".ts", ".tsx", ".js", ".jsx"}},
		{Name: "pyright", Command: "pyright-langserver", Args: []string{"--stdio"}, Extensions: []string{".py"}},
		{Name: "clangd", Command: "clangd", Extensions: []string{".c", ".h", ".cc", ".cpp", ".hpp"}},
	}
}

// Manager owns the configured servers. It starts a server only when a file
// of that server is first diagnosed, so a server that is not used costs
// nothing.
type Manager struct {
	servers     []*server // in configuration order
	byExtension map[string]*server
	waits       waitBudgets
	closed      atomic.Bool
}

// waitBudgets are the longest times that Diagnose waits. Tests make them
// shorter.
type waitBudgets struct {
	first time.Duration // until a server answered or timed out once
	later time.Duration
}

type server struct {
	config ServerConfig
	root   string

	// turn has room for one token and serializes the Diagnose calls of the
	// server, because each call takes the next version of its file.
	turn chan struct{}
	// warm is set when the first wait for diagnostics ended.
	warm atomic.Bool

	mutex   sync.Mutex
	session *session // nil until the first Diagnose or Close
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

// NewManager checks the configuration. It starts no process and does no I/O.
// The root is the workspace folder of all servers.
func NewManager(servers []ServerConfig, root string) (*Manager, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("lsp: the root %q is not an absolute path", root)
	}
	root = filepath.Clean(root)
	manager := &Manager{
		byExtension: make(map[string]*server),
		waits:       waitBudgets{first: firstWait, later: laterWait},
	}
	names := make(map[string]bool, len(servers))
	for index, config := range servers {
		if err := validate(config); err != nil {
			return nil, fmt.Errorf("lsp: server %d: %w", index+1, err)
		}
		if names[config.Name] {
			return nil, fmt.Errorf("lsp: server %d: duplicate name %q", index+1, config.Name)
		}
		names[config.Name] = true
		// Copies, so a later change by the caller does not change a server.
		config.Args = slices.Clone(config.Args)
		config.Extensions = slices.Clone(config.Extensions)
		server := &server{config: config, root: root, turn: make(chan struct{}, 1)}
		for _, extension := range config.Extensions {
			if other, taken := manager.byExtension[extension]; taken {
				return nil, fmt.Errorf("lsp: server %d: extension %q is already given to server %q", index+1, extension, other.config.Name)
			}
			manager.byExtension[extension] = server
		}
		manager.servers = append(manager.servers, server)
	}
	return manager, nil
}

func validate(config ServerConfig) error {
	if !validName(config.Name) {
		return fmt.Errorf("name %q does not match [a-z0-9_-]+", config.Name)
	}
	if config.Command == "" {
		return fmt.Errorf("server %q has no command", config.Name)
	}
	// A relative path would depend on the working directory of the process.
	if strings.ContainsRune(config.Command, filepath.Separator) && !filepath.IsAbs(config.Command) {
		return fmt.Errorf("server %q: the command %q must be a name in PATH or an absolute path", config.Name, config.Command)
	}
	if len(config.Extensions) == 0 {
		return fmt.Errorf("server %q has no extensions", config.Name)
	}
	for _, extension := range config.Extensions {
		// filepath.Ext gives the part after the last dot, so an extension
		// with a second dot cannot match.
		if len(extension) < 2 || extension[0] != '.' || strings.ContainsAny(extension[1:], "./\\") || extension != strings.ToLower(extension) || strings.ContainsFunc(extension, isSpaceOrControl) {
			return fmt.Errorf("server %q: extension %q must be a dot and lowercase characters, for example \".go\"", config.Name, extension)
		}
	}
	return nil
}

func isSpaceOrControl(character rune) bool {
	return character <= ' ' || character == 0x7f
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

// Diagnose gives the current text of a file to its server and returns the
// errors and warnings of that text, one per line. The path must be absolute.
//
// The result is "" when no server handles the extension, when the server is
// not available, and when the server does not report in time. A slow or
// broken server must not stop the agent; Unavailable tells why a server
// failed. Diagnose returns an error only when ctx ends, when the file cannot
// be read, and after Close.
func (m *Manager) Diagnose(ctx context.Context, path string) (string, error) {
	if m.closed.Load() {
		return "", errClosed
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("lsp: the path %q is not absolute", path)
	}
	path = filepath.Clean(path)
	server, found := m.byExtension[strings.ToLower(filepath.Ext(path))]
	if !found {
		return "", nil
	}
	return server.diagnose(ctx, path, m.waits)
}

// Unavailable lists the servers that could not start or that stopped, each
// as "name: reason", in configuration order. A server that has not started
// yet is not in the list.
func (m *Manager) Unavailable() []string {
	var reasons []string
	for _, server := range m.servers {
		if err := server.failure(); err != nil {
			reasons = append(reasons, server.config.Name+": "+err.Error())
		}
	}
	return reasons
}

// Close stops every started server. A server that was not started cannot
// start after Close.
func (m *Manager) Close() error {
	m.closed.Store(true)
	failures := make([]error, len(m.servers))
	var group sync.WaitGroup
	for index, server := range m.servers {
		group.Add(1)
		go func() {
			defer group.Done()
			failures[index] = server.close()
		}()
	}
	group.Wait()
	return errors.Join(failures...)
}

func (s *server) diagnose(ctx context.Context, path string, waits waitBudgets) (string, error) {
	budget := waits.later
	if !s.warm.Load() {
		budget = waits.first
	}
	budgetCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	connection, err := s.connect(budgetCtx)
	if err != nil {
		return settle(ctx, err)
	}
	select {
	case s.turn <- struct{}{}:
	case <-budgetCtx.Done():
		return settle(ctx, budgetCtx.Err())
	}
	defer func() { <-s.turn }()
	// The read is in the turn, so the server gets the texts of one file in
	// the order of the writes.
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("lsp: %w", err)
	}
	diagnostics, err := connection.diagnose(budgetCtx, path, string(content))
	if err == nil || (errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil) {
		// The workspace is loaded, or the server is too slow to wait for it.
		s.warm.Store(true)
	}
	if err != nil {
		return settle(ctx, err)
	}
	return report(s.root, path, diagnostics), nil
}

// settle turns a failure into the result of Diagnose. Only an ended ctx and
// a closed manager are errors.
func settle(ctx context.Context, err error) (string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if errors.Is(err, errClosed) {
		return "", errClosed
	}
	return "", nil
}

// connect starts the server on first use. Concurrent first calls share one
// start, which goes on when the caller that began it gives up.
func (s *server) connect(ctx context.Context) (*connection, error) {
	s.mutex.Lock()
	if s.session == nil {
		s.session = s.start()
	}
	session := s.session
	s.mutex.Unlock()
	select {
	case <-session.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if session.err != nil {
		return nil, session.err
	}
	if err := session.connection.failed(); err != nil {
		return nil, err
	}
	return session.connection, nil
}

func (s *server) start() *session {
	program, err := exec.LookPath(s.config.Command)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return failedSession(fmt.Errorf("command %q not found", s.config.Command))
	}
	if err != nil {
		return failedSession(err)
	}
	connection, err := startConnection(program, s.config.Args, s.root)
	if err != nil {
		return failedSession(fmt.Errorf("start %s: %w", program, err))
	}
	started := &session{ready: make(chan struct{}), connection: connection}
	go func() {
		defer close(started.ready)
		if err := initialize(connection, s.root); err != nil {
			// The kill error adds nothing: the handshake already failed.
			_ = connection.terminate(err)
			started.err = err
		}
	}()
	return started
}

type clientInfo struct {
	Name string `json:"name"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type initializeParams struct {
	// ProcessID lets a server exit when the client dies without Close.
	ProcessID        int               `json:"processId"`
	ClientInfo       clientInfo        `json:"clientInfo"`
	RootURI          string            `json:"rootUri"`
	WorkspaceFolders []workspaceFolder `json:"workspaceFolders"`
	Capabilities     json.RawMessage   `json:"capabilities"`
}

func initialize(connection *connection, root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	uri := fileURI(root)
	params := initializeParams{
		ProcessID:        os.Getpid(),
		ClientInfo:       clientInfo{Name: "inkling"},
		RootURI:          uri,
		WorkspaceFolders: []workspaceFolder{{URI: uri, Name: filepath.Base(root)}},
		Capabilities:     clientCapabilities,
	}
	_, err := connection.request(ctx, "initialize", params)
	if err == nil {
		err = connection.notify(ctx, "initialized", struct{}{})
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

// failure tells why the server is not available. It is nil while the server
// is not started, starts, or runs, and after Close.
func (s *server) failure() error {
	s.mutex.Lock()
	session := s.session
	s.mutex.Unlock()
	if session == nil {
		return nil
	}
	select {
	case <-session.ready:
	default:
		return nil
	}
	err := session.err
	if err == nil {
		err = session.connection.failed()
	}
	if errors.Is(err, errClosed) {
		return nil
	}
	return err
}

func (s *server) close() error {
	s.mutex.Lock()
	if s.session == nil {
		s.session = failedSession(errClosed)
	}
	session := s.session
	s.mutex.Unlock()
	if session.connection == nil {
		return nil
	}
	if err := session.connection.stop(); err != nil {
		return fmt.Errorf("lsp: stop server %q: %w", s.config.Name, err)
	}
	return nil
}
