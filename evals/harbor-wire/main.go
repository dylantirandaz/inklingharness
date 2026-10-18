// Command harbor-wire records the real peer process and its inference traffic,
// sent to OpenRouter or to Prime Intellect Inference.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/record"
)

// wireProfile is the fixed inference profile of one run: the model, the one
// OpenRouter provider it may use, the reasoning effort, and the output cap.
// The defaults are the Inkling Small profile of the earlier comparisons.
type wireProfile struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Effort    string `json:"effort"`
	MaxTokens int    `json:"max_tokens"`
	// Upstream is empty for OpenRouter, or "prime" for Prime Intellect
	// Inference, which serves one route per model and takes no provider field.
	Upstream string `json:"upstream,omitempty"`
}

const primeUpstream = "prime"

// upstreamHost returns the API host; both serve /api/v1/messages and
// /api/v1/responses at the same paths.
func (p wireProfile) upstreamHost() string {
	if p.Upstream == primeUpstream {
		return "https://api.pinference.ai"
	}
	return "https://openrouter.ai"
}

func (p wireProfile) keyVariable() string {
	if p.Upstream == primeUpstream {
		return "PRIME_API_KEY"
	}
	return "OPENROUTER_API_KEY"
}

// upstreamBody drops the OpenRouter route after it was checked: Prime has no
// provider routing, so the field would only be noise in its request.
func upstreamBody(body []byte, fixed wireProfile) ([]byte, error) {
	if fixed.Upstream != primeUpstream {
		return body, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	delete(fields, "provider")
	return json.Marshal(fields)
}

func defaultProfile() wireProfile {
	return wireProfile{Model: "thinkingmachines/inkling-small", Provider: "deepinfra/fp8", Effort: "high", MaxTokens: 16384}
}

func (p wireProfile) route() json.RawMessage {
	encoded, err := json.Marshal(map[string]any{"order": []string{p.Provider}, "allow_fallbacks": false})
	if err != nil {
		panic(err)
	}
	return encoded
}

var copyBuffers sync.Pool

type messagesPolicy string

const (
	verifyMessages    messagesPolicy = "verify"
	normalizeMessages messagesPolicy = "normalize"
)

type requestProfile struct {
	Model        string `json:"model"`
	MaxTokens    int    `json:"max_tokens"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Provider struct {
		Order          []string `json:"order"`
		AllowFallbacks *bool    `json:"allow_fallbacks"`
	} `json:"provider"`
}

func requestBody(body []byte, path string, policy messagesPolicy, fixed wireProfile) ([]byte, error) {
	var profile requestProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, fmt.Errorf("decode request profile: %w", err)
	}
	if profile.Model != fixed.Model {
		return nil, fmt.Errorf("expected model %q, got %q", fixed.Model, profile.Model)
	}
	if path == "/api/v1/messages/count_tokens" {
		return upstreamBody(body, fixed)
	}
	maxTokens := json.RawMessage(strconv.Itoa(fixed.MaxTokens))
	effort, err := json.Marshal(fixed.Effort)
	if err != nil {
		return nil, err
	}
	if path == "/api/v1/messages" && policy == verifyMessages {
		if profile.MaxTokens != fixed.MaxTokens || profile.OutputConfig.Effort != fixed.Effort || len(profile.Provider.Order) != 1 || profile.Provider.Order[0] != fixed.Provider || profile.Provider.AllowFallbacks == nil || *profile.Provider.AllowFallbacks {
			return nil, errors.New("Messages request differs from the fixed profile")
		}
		return upstreamBody(body, fixed)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	switch path {
	case "/api/v1/messages":
		if policy != normalizeMessages {
			return nil, fmt.Errorf("unsupported Messages policy %q", policy)
		}
		delete(fields, "thinking")
		delete(fields, "temperature")
		fields["max_tokens"] = maxTokens
		output := make(map[string]json.RawMessage)
		if existing, exists := fields["output_config"]; exists {
			if err := json.Unmarshal(existing, &output); err != nil || output == nil {
				return nil, errors.New("output_config must be an object")
			}
		}
		output["effort"] = effort
		encoded, err := json.Marshal(output)
		if err != nil {
			return nil, err
		}
		fields["output_config"] = encoded
	case "/api/v1/responses":
		if profile.Reasoning.Effort != fixed.Effort {
			return nil, fmt.Errorf("Responses request must use native %s reasoning effort", fixed.Effort)
		}
		fields["max_output_tokens"] = maxTokens
	default:
		return nil, fmt.Errorf("unsupported endpoint %q", path)
	}
	if fixed.Upstream == primeUpstream {
		delete(fields, "provider")
	} else {
		fields["provider"] = fixed.route()
	}
	return json.Marshal(fields)
}

type exchange struct {
	Path            string    `json:"path"`
	Method          string    `json:"method"`
	Query           string    `json:"query"`
	StartedAt       time.Time `json:"started_at"`
	ElapsedSeconds  float64   `json:"elapsed_seconds"`
	StatusCode      *int      `json:"status_code"`
	ContentType     string    `json:"content_type"`
	ContentEncoding string    `json:"content_encoding"`
	// UpstreamRequestID is Prime's X-Inference-Id for the call.
	UpstreamRequestID string  `json:"upstream_request_id,omitempty"`
	Error             *string `json:"error"`
}

type proxy struct {
	directory string
	key       string
	policy    messagesPolicy
	profile   wireProfile
	client    *http.Client
	sequence  atomic.Uint64
	failure   chan error
	cancel    context.CancelFunc
}

func (p *proxy) fail(err error) {
	select {
	case p.failure <- err:
	default:
	}
	p.cancel()
}

func (p *proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	directory := filepath.Join(p.directory, fmt.Sprintf("%04d", p.sequence.Add(1)))
	if err := os.Mkdir(directory, 0o700); err != nil {
		p.fail(err)
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	metadata := exchange{Path: request.URL.Path, Method: request.Method, Query: request.URL.RawQuery, StartedAt: started.UTC()}
	defer func() {
		metadata.ElapsedSeconds = time.Since(started).Seconds()
		encoded, err := json.Marshal(metadata)
		if err == nil {
			err = os.WriteFile(filepath.Join(directory, "metadata.json"), encoded, 0o600)
		}
		if err != nil {
			p.fail(fmt.Errorf("write exchange metadata: %w", err))
		}
	}()
	reject := func(err error, code int, fatal bool) {
		message := err.Error()
		metadata.Error = &message
		http.Error(writer, message, code)
		if fatal {
			p.fail(err)
		}
	}
	rootWarmup := request.Method == http.MethodHead && (request.URL.Path == "/api" || request.URL.Path == "/api/v1")
	if !rootWarmup && request.URL.Path != "/api/v1/messages" && request.URL.Path != "/api/v1/responses" && request.URL.Path != "/api/v1/messages/count_tokens" {
		reject(fmt.Errorf("unsupported endpoint %q", request.URL.Path), http.StatusNotFound, true)
		return
	}
	if request.Method != http.MethodPost && request.Method != http.MethodHead {
		reject(fmt.Errorf("unsupported request method %q", request.Method), http.StatusMethodNotAllowed, true)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		reject(err, http.StatusBadRequest, true)
		return
	}
	if err := os.WriteFile(filepath.Join(directory, "incoming.json"), body, 0o600); err != nil {
		reject(err, http.StatusInternalServerError, true)
		return
	}
	forwarded := body
	if request.Method == http.MethodPost {
		forwarded, err = requestBody(body, request.URL.Path, p.policy, p.profile)
		if err != nil {
			reject(err, http.StatusBadRequest, true)
			return
		}
	}
	recorder, err := record.NewDirectoryRecorder(directory)
	if err != nil {
		reject(err, http.StatusInternalServerError, true)
		return
	}
	responseLog, err := recorder.Begin([][]byte{forwarded})
	if err != nil {
		reject(err, http.StatusInternalServerError, true)
		return
	}
	defer func() {
		if err := responseLog.Close(); err != nil {
			message := err.Error()
			metadata.Error = &message
			p.fail(err)
		}
	}()
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, p.profile.upstreamHost()+request.URL.RequestURI(), bytes.NewReader(forwarded))
	if err != nil {
		reject(err, http.StatusInternalServerError, true)
		return
	}
	upstream.Header = request.Header.Clone()
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Content-Length", "Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Te", "Trailer", "Upgrade"} {
		upstream.Header.Del(name)
	}
	if request.Method == http.MethodPost {
		upstream.Header.Set("Authorization", "Bearer "+p.key)
		if p.profile.Upstream != primeUpstream {
			// Completed responses can lack a later generation lookup record.
			upstream.Header.Set("X-OpenRouter-Metadata", "enabled")
		}
	}
	if p.profile.Upstream == primeUpstream {
		// Prime's edge rejects some client user agents; one fixed agent keeps
		// every harness's requests equally admissible.
		upstream.Header.Set("User-Agent", "harbor-wire/1")
	}
	upstream.Header.Set("Accept-Encoding", "identity")
	response, err := p.client.Do(upstream)
	if err != nil {
		reject(err, http.StatusBadGateway, false)
		return
	}
	defer response.Body.Close()
	metadata.StatusCode = &response.StatusCode
	metadata.ContentType = response.Header.Get("Content-Type")
	metadata.ContentEncoding = response.Header.Get("Content-Encoding")
	metadata.UpstreamRequestID = response.Header.Get("X-Inference-Id")
	for name, values := range response.Header {
		switch strings.ToLower(name) {
		case "connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade":
			continue
		}
		writer.Header()[name] = values
	}
	writer.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(writer)
	if err := controller.Flush(); err != nil {
		message := err.Error()
		metadata.Error = &message
		return
	}
	var buffer *[32 * 1024]byte
	if existing := copyBuffers.Get(); existing != nil {
		var ok bool
		buffer, ok = existing.(*[32 * 1024]byte)
		if !ok {
			panic("invalid private copy buffer")
		}
	} else {
		buffer = new([32 * 1024]byte)
	}
	defer copyBuffers.Put(buffer)
	_, err = io.CopyBuffer(flushedWriter{writer: writer, controller: controller}, io.TeeReader(response.Body, responseLog), buffer[:])
	if err != nil {
		message := err.Error()
		metadata.Error = &message
	}
}

type flushedWriter struct {
	writer     http.ResponseWriter
	controller *http.ResponseController
}

func (w flushedWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err == nil {
		err = w.controller.Flush()
	}
	return n, err
}

type memoryResult struct {
	peak *uint64
	err  error
}

func sampleMemory(pid int, stopped <-chan struct{}) <-chan memoryResult {
	result := make(chan memoryResult, 1)
	go func() {
		file, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			result <- memoryResult{err: err}
			return
		}
		defer file.Close()
		var peak *uint64
		var buffer [8192]byte
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			n, err := file.ReadAt(buffer[:], 0)
			if err != nil && !errors.Is(err, io.EOF) {
				if errors.Is(err, syscall.ESRCH) && peak != nil {
					result <- memoryResult{peak: peak}
				} else {
					result <- memoryResult{peak: peak, err: err}
				}
				return
			}
			remaining := buffer[:n]
			found := false
			for len(remaining) > 0 {
				line, rest, _ := bytes.Cut(remaining, []byte{'\n'})
				remaining = rest
				value, match := bytes.CutPrefix(line, []byte("VmRSS:"))
				if !match {
					continue
				}
				value = bytes.TrimSpace(value)
				value, units, separated := bytes.Cut(value, []byte{' '})
				amount, parseErr := strconv.ParseUint(string(value), 10, 64)
				if parseErr != nil || !separated || !bytes.Equal(bytes.TrimSpace(units), []byte("kB")) {
					result <- memoryResult{peak: peak, err: fmt.Errorf("invalid VmRSS in the native process status: %q", line)}
					return
				}
				amount *= 1024
				if peak == nil {
					peak = new(uint64)
				}
				if amount > *peak {
					*peak = amount
				}
				found = true
				break
			}
			if !found {
				// Linux omits memory fields after address-space release, even while State is R.
				if peak != nil && errors.Is(err, io.EOF) {
					result <- memoryResult{peak: peak}
					return
				}
				result <- memoryResult{peak: peak, err: errors.New("VmRSS is absent from the native process status")}
				return
			}
			select {
			case <-stopped:
				result <- memoryResult{peak: peak}
				return
			case <-ticker.C:
			}
		}
	}()
	return result
}

func childEnvironment(messagesURL, responsesURL string) []string {
	changes := map[string]string{
		"HARBOR_MESSAGES_BASE_URL":  messagesURL,
		"HARBOR_RESPONSES_BASE_URL": responsesURL,
		"ANTHROPIC_BASE_URL":        messagesURL,
		"HARBOR_WIRE_TOKEN":         "private-loopback-only",
		"OPENROUTER_API_KEY":        "private-loopback-only",
		"ANTHROPIC_AUTH_TOKEN":      "private-loopback-only",
		"OPENAI_API_KEY":            "private-loopback-only",
		"PRIME_API_KEY":             "private-loopback-only",
		"ANTHROPIC_API_KEY":         "",
	}
	inherited := os.Environ()
	environment := make([]string, 0, len(inherited)+len(changes))
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if _, replace := changes[name]; !replace {
			environment = append(environment, entry)
		}
	}
	for name, value := range changes {
		environment = append(environment, name+"="+value)
	}
	return environment
}

type runReport struct {
	ElapsedSeconds          float64 `json:"elapsed_seconds"`
	SampledMainPeakRSSBytes *uint64 `json:"sampled_main_peak_rss_bytes"`
	MemoryError             *string `json:"memory_error"`
	ProcessError            *string `json:"process_error"`
	RecorderError           *string `json:"recorder_error"`
	Requests                uint64  `json:"requests"`
}

func errorText(err error) *string {
	if err == nil {
		return nil
	}
	text := err.Error()
	return &text
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("harbor-wire", flag.ContinueOnError)
	var directory *string
	var policy *messagesPolicy
	flags.Func("logs", "required new recording directory", func(value string) error {
		if value == "" {
			return errors.New("logs directory must not be empty")
		}
		directory = &value
		return nil
	})
	flags.Func("messages-policy", "required: verify or normalize", func(value string) error {
		switch messagesPolicy(value) {
		case verifyMessages, normalizeMessages:
			selected := messagesPolicy(value)
			policy = &selected
			return nil
		default:
			return fmt.Errorf("unknown Messages policy %q", value)
		}
	})
	profile := defaultProfile()
	flags.StringVar(&profile.Model, "model", profile.Model, "the only OpenRouter model the child may request")
	flags.StringVar(&profile.Provider, "provider", profile.Provider, "the only OpenRouter provider slug, without fallbacks")
	flags.StringVar(&profile.Effort, "effort", profile.Effort, "the reasoning effort every request uses")
	flags.IntVar(&profile.MaxTokens, "max-tokens", profile.MaxTokens, "the output token cap every request uses")
	flags.Func("upstream", "openrouter (default) or prime", func(value string) error {
		switch value {
		case "openrouter":
			profile.Upstream = ""
		case primeUpstream:
			profile.Upstream = primeUpstream
		default:
			return fmt.Errorf("unknown upstream %q", value)
		}
		return nil
	})
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if profile.Model == "" || profile.Provider == "" || profile.Effort == "" || profile.MaxTokens <= 0 {
		return errors.New("model, provider, effort, and a positive max-tokens are required")
	}
	if directory == nil || policy == nil || len(flags.Args()) == 0 {
		return errors.New("logs, messages-policy, and a child command are required")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return errors.New("harbor-wire requires native Linux amd64")
	}
	key, exists := os.LookupEnv(profile.keyVariable())
	if !exists || key == "" {
		return fmt.Errorf("%s is required", profile.keyVariable())
	}
	if err := os.Mkdir(*directory, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*directory, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	encodedProfile, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*directory, "profile.json"), append(encodedProfile, '\n'), 0o600); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	proxyContext, cancelProxy := context.WithCancel(ctx)
	defer cancelProxy()
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return errors.New("the default HTTP transport has an unexpected type")
	}
	transport := defaultTransport.Clone()
	transport.DisableCompression = true
	defer transport.CloseIdleConnections()
	handler := &proxy{directory: *directory, key: key, policy: *policy, profile: profile, client: &http.Client{Transport: transport}, failure: make(chan error, 1), cancel: cancelProxy}
	server := &http.Server{Handler: handler}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			handler.fail(err)
		}
	}()
	base := "http://" + listener.Addr().String()
	command := exec.CommandContext(proxyContext, flags.Args()[0], flags.Args()[1:]...)
	command.Env = childEnvironment(base+"/api", base+"/api/v1")
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	started := time.Now()
	if err := command.Start(); err != nil {
		server.Close()
		return err
	}
	stopped := make(chan struct{})
	memory := sampleMemory(command.Process.Pid, stopped)
	processErr := command.Wait()
	elapsed := time.Since(started).Seconds()
	close(stopped)
	memoryReading := <-memory
	shutdownContext, stopShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopShutdown()
	shutdownErr := server.Shutdown(shutdownContext)
	if shutdownErr != nil {
		server.Close()
	}
	var recorderErr error
	select {
	case recorderErr = <-handler.failure:
	default:
	}
	recorderErr = errors.Join(recorderErr, shutdownErr)
	report := runReport{ElapsedSeconds: elapsed, SampledMainPeakRSSBytes: memoryReading.peak, MemoryError: errorText(memoryReading.err), ProcessError: errorText(processErr), RecorderError: errorText(recorderErr), Requests: handler.sequence.Load()}
	encoded, err := json.Marshal(report)
	if err == nil {
		err = os.WriteFile(filepath.Join(*directory, "run.json"), encoded, 0o600)
	}
	return errors.Join(processErr, recorderErr, memoryReading.err, err)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "harbor-wire:", err)
		os.Exit(1)
	}
}
