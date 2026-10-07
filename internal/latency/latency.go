// Package latency records opt-in, content-free latency measurements.
package latency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Stage is the set of measured operations.
type Stage string

const (
	Preparation  Stage = "preparation"
	Checkpoint   Stage = "checkpoint"
	Request      Stage = "request"
	Encoding     Stage = "encoding"
	HTTP         Stage = "http"
	DNS          Stage = "dns"
	Connect      Stage = "connect"
	TLS          Stage = "tls"
	RequestWrite Stage = "request_write"
	FirstByte    Stage = "first_byte"
	FirstUseful  Stage = "first_useful"
	StreamRead   Stage = "stream_read"
	StreamDecode Stage = "stream_decode"
	Observer     Stage = "observer"
	Retry        Stage = "retry"
	Tool         Stage = "tool"
	Approval     Stage = "approval"
	Compaction   Stage = "compaction"
)

const maxEntries = 2048

func validStage(s Stage) bool {
	switch s {
	case Preparation, Checkpoint, Request, Encoding, HTTP, DNS, Connect, TLS, RequestWrite, FirstByte, FirstUseful, StreamRead, StreamDecode, Observer, Retry, Tool, Approval, Compaction:
		return true
	}
	return false
}

func operation(s string) string {
	switch s {
	case "run", "chat", "eval", "prepare", "save", "load", "attachments", "session", "messages", "attempt", "stream", "callback", "wait", "execute", "approve", "compact", "network", "other",
		"read_file", "write_file", "edit_file", "bash", "list_dir", "glob", "grep", "todo_write", "task", "turn_done", "tool_results", "compact_status", "check":
		return s
	default:
		return "other"
	}
}

func identifier(s string) string {
	if len(s) == 0 || len(s) > 128 {
		return "unknown"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/_.:-", c)) {
			return "unknown"
		}
	}
	return s
}

type entry struct {
	Stage     Stage   `json:"stage"`
	Operation string  `json:"operation"`
	Request   uint64  `json:"request,omitempty"`
	Attempt   int     `json:"attempt,omitempty"`
	Count     int64   `json:"count"`
	Failures  int64   `json:"failures"`
	Canceled  int64   `json:"canceled"`
	NS        int64   `json:"ns"`
	Protocol  string  `json:"protocol,omitempty"`
	Status    int     `json:"http_status,omitempty"`
	Reused    *bool   `json:"reused,omitempty"`
	Usage     *Tokens `json:"usage,omitempty"`
}

// Tokens retains absence separately from a reported zero, including cache misses.
type Tokens struct {
	Input         *int `json:"input_tokens,omitempty"`
	Output        *int `json:"output_tokens,omitempty"`
	CacheRead     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreation *int `json:"cache_creation_input_tokens,omitempty"`
}

type record struct {
	Version int     `json:"version"`
	Model   string  `json:"model"`
	Effort  string  `json:"effort"`
	State   string  `json:"state"`
	NS      int64   `json:"ns"`
	Dropped uint64  `json:"dropped"`
	Entries []entry `json:"entries"`
}

// Recorder is safe to share with concurrent read-only child requests.
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	nextRequest uint64
	finished    bool
	record      record
}

func New(model, effort string) *Recorder {
	if effort == "" {
		effort = "default"
	}
	return &Recorder{started: time.Now(), record: record{Version: 1, Model: identifier(model), Effort: identifier(effort), Entries: make([]entry, 0, 32)}}
}

type contextKey struct{}
type requestKey struct{}
type attemptKey struct{}

func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	return context.WithValue(ctx, contextKey{}, recorder)
}

func FromContext(ctx context.Context) *Recorder {
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	return r
}

func state(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	if err != nil {
		return "error"
	}
	return "success"
}

func (r *Recorder) add(e entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	if len(r.record.Entries) == maxEntries {
		r.record.Dropped++
		return
	}
	r.record.Entries = append(r.record.Entries, e)
}

func base(ctx context.Context, stage Stage, op string) entry {
	request, _ := ctx.Value(requestKey{}).(uint64)
	attempt, _ := ctx.Value(attemptKey{}).(int)
	return entry{Stage: stage, Operation: operation(op), Request: request, Attempt: attempt}
}

func result(e *entry, elapsed time.Duration, err error) {
	e.Count++
	e.NS += int64(elapsed)
	switch state(err) {
	case "error":
		e.Failures++
	case "canceled":
		e.Canceled++
	}
}

// Span is one elapsed interval. End is nil-safe and idempotent.
type Span struct {
	recorder *Recorder
	started  time.Time
	once     sync.Once
	entry    entry
}

func Begin(ctx context.Context, stage Stage, op string) *Span {
	r := FromContext(ctx)
	if r == nil {
		return nil
	}
	if !validStage(stage) {
		panic("latency: unsupported stage")
	}
	return &Span{recorder: r, started: time.Now(), entry: base(ctx, stage, op)}
}

func (s *Span) End(err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		result(&s.entry, time.Since(s.started), err)
		s.recorder.add(s.entry)
	})
}

// Mark records elapsed time from the request start without a second clock origin.
func (s *Span) Mark(stage Stage) {
	if s == nil {
		return
	}
	if !validStage(stage) {
		panic("latency: unsupported stage")
	}
	e := s.entry
	e.Stage = stage
	e.Usage = nil
	result(&e, time.Since(s.started), nil)
	s.recorder.add(e)
}

// Meter aggregates repeated work without allocating a span for each delta.
// A meter belongs to one calling goroutine, like a response stream.
type Meter struct {
	recorder *Recorder
	entry    entry
}

func NewMeter(ctx context.Context, stage Stage, op string) *Meter {
	r := FromContext(ctx)
	if r == nil {
		return nil
	}
	if !validStage(stage) {
		panic("latency: unsupported stage")
	}
	return &Meter{recorder: r, entry: base(ctx, stage, op)}
}

func (m *Meter) Start() time.Time {
	if m == nil {
		return time.Time{}
	}
	return time.Now()
}

func (m *Meter) Add(started time.Time, err error) {
	m.AddExcluding(started, 0, err)
}

// Total and AddExcluding separate synchronous observer work from decode work.
func (m *Meter) Total() time.Duration {
	if m == nil {
		return 0
	}
	return time.Duration(m.entry.NS)
}

func (m *Meter) AddExcluding(started time.Time, excluded time.Duration, err error) {
	if m != nil {
		result(&m.entry, max(0, time.Since(started)-excluded), err)
	}
}

func (m *Meter) End() {
	if m != nil && m.entry.Count > 0 {
		m.recorder.add(m.entry)
	}
}

// RequestContext allocates a scope for one model request and its tool results.
// A scope must not be shared between model requests.
func RequestContext(ctx context.Context) context.Context {
	r := FromContext(ctx)
	if r == nil {
		return ctx
	}
	r.mu.Lock()
	r.nextRequest++
	id := r.nextRequest
	r.mu.Unlock()
	return context.WithValue(ctx, requestKey{}, id)
}

// StartRequest starts a caller-provided scope, or creates one for a direct call.
func StartRequest(ctx context.Context) (context.Context, *Span) {
	if FromContext(ctx) == nil {
		return ctx, nil
	}
	if _, scoped := ctx.Value(requestKey{}).(uint64); !scoped {
		ctx = RequestContext(ctx)
	}
	return ctx, Begin(ctx, Request, "messages")
}

// Usage merges only reported fields. The caller owns the span until End.
func (s *Span) Usage(tokens Tokens) {
	if s == nil {
		return
	}
	if s.entry.Usage == nil {
		s.entry.Usage = &Tokens{}
	}
	if tokens.Input != nil {
		s.entry.Usage.Input = tokens.Input
	}
	if tokens.Output != nil {
		s.entry.Usage.Output = tokens.Output
	}
	if tokens.CacheRead != nil {
		s.entry.Usage.CacheRead = tokens.CacheRead
	}
	if tokens.CacheCreation != nil {
		s.entry.Usage.CacheCreation = tokens.CacheCreation
	}
}

func (r *Recorder) Finish(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.record.NS = int64(time.Since(r.started))
		r.record.State = state(err)
		r.finished = true
	}
}

// AppendFile appends exactly one completed record. It never suppresses I/O errors.
func (r *Recorder) AppendFile(path string) error {
	if r == nil {
		return errors.New("timings: recorder is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		return errors.New("timings: finish the record before appending")
	}
	data, err := json.Marshal(r.record)
	if err != nil {
		return fmt.Errorf("timings: encode record: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return fmt.Errorf("timings: open output: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("timings: inspect output: %w", err), file.Close())
	}
	if !info.Mode().IsRegular() {
		return errors.Join(errors.New("timings: output must be a regular file"), file.Close())
	}
	if err := file.Chmod(0600); err != nil {
		return errors.Join(fmt.Errorf("timings: restrict output permissions: %w", err), file.Close())
	}
	n, writeErr := file.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	var syncErr error
	if writeErr == nil {
		syncErr = file.Sync()
	}
	if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
		return fmt.Errorf("timings: append output: %w", err)
	}
	return nil
}
