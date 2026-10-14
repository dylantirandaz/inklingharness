// Package tinker fine-tunes a model of the user's choice on their own Think
// sessions with Tinker, and serves the resulting checkpoints back to Think.
//
// A round exports saved sessions as traces, trains a LoRA adapter on them with
// an embedded Python helper (it needs the tinker and tinker-cookbook
// packages), and saves both a resumable training state and sampler weights.
// The next round resumes from that state, so the model keeps learning from the
// sessions as they grow.
package tinker

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

//go:embed finetune.py
var finetuneScript []byte

// Prefix marks a model ID that Think serves from a Tinker checkpoint.
const Prefix = "tinker://"

// Trace is one session as training data: the raw Messages API history with
// the system prompt and tools the model saw.
type Trace struct {
	Session  string                     `json:"session"`
	Model    string                     `json:"model"`
	System   string                     `json:"system"`
	Tools    []anthropic.ToolDefinition `json:"tools"`
	Messages []json.RawMessage          `json:"messages"`
}

// NewTrace builds the trace of one session history.
func NewTrace(sessionID, model, system string, tools []anthropic.ToolDefinition, messages []anthropic.EncodedMessage) Trace {
	trace := Trace{Session: sessionID, Model: model, System: system, Tools: tools, Messages: make([]json.RawMessage, 0, len(messages))}
	for _, message := range messages {
		trace.Messages = append(trace.Messages, json.RawMessage(message.Wire()))
	}
	return trace
}

// Trainable reports whether the trace has an assistant turn to learn from.
func (t Trace) Trainable() bool {
	for _, message := range t.Messages {
		var head struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(message, &head) == nil && head.Role == "assistant" {
			return true
		}
	}
	return false
}

// Round is one fine-tuning job, running or finished.
type Round struct {
	Number    int            `json:"number"`
	Started   time.Time      `json:"started"`
	PID       int            `json:"pid"`
	Traces    string         `json:"traces"`
	Result    string         `json:"result"`
	Log       string         `json:"log"`
	Sessions  map[string]int `json:"sessions"`
	Resumed   string         `json:"resumed,omitempty"`
	BaseModel string         `json:"base_model"`
}

// Result is what the helper writes when a round ends.
type Result struct {
	StatePath   string  `json:"state_path"`
	SamplerPath string  `json:"sampler_path"`
	Rows        int     `json:"rows"`
	Tokens      int     `json:"tokens"`
	Steps       int     `json:"steps"`
	Loss        float64 `json:"loss"`
	Error       string  `json:"error"`
}

// State is the user's fine-tuning progress for one base model.
type State struct {
	BaseModel   string `json:"base_model"`
	StatePath   string `json:"state_path,omitempty"`
	SamplerPath string `json:"sampler_path,omitempty"`
	Rounds      int    `json:"rounds"`
	// Trained maps each session ID to the message count of its last trained trace.
	Trained   map[string]int `json:"trained"`
	Running   *Round         `json:"running,omitempty"`
	LastLoss  float64        `json:"last_loss,omitempty"`
	LastError string         `json:"last_error,omitempty"`
}

// Dir is the private folder that holds the state, traces and logs.
func Dir() (string, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" || !filepath.IsAbs(root) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(root, "inkling", "tinker"), nil
}

// Load reads the state in dir; a missing state is empty.
func Load(dir string) (State, error) {
	state := State{Trained: map[string]int{}}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("tinker: read state: %w", err)
	}
	if state.Trained == nil {
		state.Trained = map[string]int{}
	}
	return state, nil
}

// Save writes the state atomically.
func Save(dir string, state State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temporary := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(dir, "state.json"))
}

// Settle applies a finished round to the state. It reports whether the state
// changed; a round that still runs leaves it unchanged.
func Settle(state *State) (bool, error) {
	round := state.Running
	if round == nil {
		return false, nil
	}
	data, err := os.ReadFile(round.Result)
	if errors.Is(err, os.ErrNotExist) {
		if processAlive(round.PID) {
			return false, nil
		}
		state.Running = nil
		state.LastError = fmt.Sprintf("round %d stopped without a result; see %s", round.Number, round.Log)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return false, fmt.Errorf("tinker: read round result: %w", err)
	}
	state.Running = nil
	if result.Error != "" || result.SamplerPath == "" {
		state.LastError = fmt.Sprintf("round %d failed: %s (log: %s)", round.Number, firstNonEmpty(result.Error, "no checkpoint"), round.Log)
		return true, nil
	}
	state.Rounds = round.Number
	state.StatePath, state.SamplerPath = result.StatePath, result.SamplerPath
	state.LastLoss, state.LastError = result.Loss, ""
	for id, count := range round.Sessions {
		state.Trained[id] = count
	}
	return true, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

// Python is the interpreter that runs the helper: THINK_TINKER_PYTHON, or python3.
func Python() string {
	if python := os.Getenv("THINK_TINKER_PYTHON"); python != "" {
		return python
	}
	return "python3"
}

// Start writes the traces and starts one detached round, so training finishes
// even if Think exits. sessions maps each trace's session to its message count.
func Start(dir string, state *State, traces []Trace, sessions map[string]int) (*Round, error) {
	if os.Getenv("TINKER_API_KEY") == "" {
		return nil, errors.New("TINKER_API_KEY is not set; create a key in the Tinker console and export it")
	}
	if state.Running != nil {
		return nil, fmt.Errorf("round %d is still training", state.Running.Number)
	}
	number := state.Rounds + 1
	roundDir := filepath.Join(dir, "rounds", strconv.Itoa(number)+"-"+time.Now().UTC().Format("20060102T150405"))
	if err := os.MkdirAll(roundDir, 0o700); err != nil {
		return nil, err
	}
	round := &Round{Number: number, Started: time.Now().UTC(), Traces: filepath.Join(roundDir, "traces.jsonl"), Result: filepath.Join(roundDir, "result.json"), Log: filepath.Join(roundDir, "train.log"), Sessions: sessions, Resumed: state.StatePath, BaseModel: state.BaseModel}
	if err := WriteTraces(round.Traces, traces); err != nil {
		return nil, err
	}
	script := filepath.Join(dir, "finetune.py")
	if err := os.WriteFile(script, finetuneScript, 0o600); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(round.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	arguments := []string{script, "--traces", round.Traces, "--base-model", state.BaseModel, "--name", fmt.Sprintf("think-round-%d", number), "--out", round.Result}
	if state.StatePath != "" {
		arguments = append(arguments, "--resume", state.StatePath)
	}
	command := exec.Command(Python(), arguments...)
	command.Stdout, command.Stderr = logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("tinker: start %s: %w", Python(), err)
	}
	round.PID = command.Process.Pid
	// Reap the child when it exits; the result file is the outcome.
	go func() { _ = command.Wait() }()
	state.Running = round
	if err := Save(dir, *state); err != nil {
		return nil, err
	}
	return round, nil
}

// WriteTraces writes one JSON trace per line.
func WriteTraces(path string, traces []Trace) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := encodeTraces(file, traces); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func encodeTraces(w io.Writer, traces []Trace) error {
	encoder := json.NewEncoder(w)
	for _, trace := range traces {
		if err := encoder.Encode(trace); err != nil {
			return err
		}
	}
	return nil
}

// Describe is a short status of the state for the /tinker command.
func Describe(state State) string {
	if state.BaseModel == "" {
		return "No Tinker model yet. Use /tinker MODEL, for example /tinker Qwen/Qwen3-30B-A3B-Instruct-2507."
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Tinker base model: %s\n", state.BaseModel)
	fmt.Fprintf(&text, "Rounds finished: %d, sessions trained: %d\n", state.Rounds, len(state.Trained))
	if state.SamplerPath != "" {
		fmt.Fprintf(&text, "Latest checkpoint: %s (loss %.4f)\n", state.SamplerPath, state.LastLoss)
	}
	if state.Running != nil {
		fmt.Fprintf(&text, "Round %d training since %s on %d sessions; log: %s\n", state.Running.Number, state.Running.Started.Local().Format("15:04"), len(state.Running.Sessions), state.Running.Log)
	}
	if state.LastError != "" {
		fmt.Fprintf(&text, "Last error: %s\n", state.LastError)
	}
	return text.String()
}
