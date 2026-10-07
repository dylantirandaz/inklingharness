// Command limits measures a think binary in a real pseudo-terminal and fails
// when a measure crosses its limit. The release script runs it, so a slow or
// large build stops the release. It is a development tool and is not part of
// the shipped binary.
//
// Usage: go run ./scripts/limits [flags] path/to/think
//
// The chat command does not check the API key before the first prompt, so a
// dummy key is sufficient. But chat opens one HEAD connection at startup to
// prewarm the API connection. To keep the measures free of network time and
// network failures, the child gets -base-url that points to a local HTTP
// server in this process. The prewarm code path still runs.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"
	"unsafe"
)

const (
	// readyText is in the prompt placeholder that chat draws when it can
	// take input.
	readyText = "Ask Inkling"
	// deviceAttributesQuery ends the terminal background query. A real
	// terminal answers it; without the answer chat waits for its timeout.
	deviceAttributesQuery = "\x1b[c"
	// terminalReply is the answer of a dark terminal that supports OSC 11.
	terminalReply = "\x1b]11;rgb:1e1e/1c1c/1a1a\x1b\\\x1b[?62;22c"
	quitInput     = "/quit\r"

	terminalRows    = 30
	terminalColumns = 100

	readyTimeout = 5 * time.Second
	exitTimeout  = 5 * time.Second
	// settleTime lets work that follows the first frame finish before the
	// idle period starts.
	settleTime = time.Second
	idleTime   = 10 * time.Second

	startupRuns = 7
	idleRuns    = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("limits", flag.ContinueOnError)
	maxStartupMilliseconds := flags.Float64("max-startup-ms", 40, "limit for the median startup time in milliseconds")
	maxIdleCPUMilliseconds := flags.Float64("max-idle-cpu-ms", 5, "limit for the CPU time of 10 s idle in milliseconds")
	maxBinaryMebibytes := flags.Float64("max-binary-mib", 10, "limit for the binary size in MiB")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/limits [flags] path/to/think")
		return 2
	}
	binary, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "limits: %v\n", err)
		return 2
	}

	crossed := 0
	report := func(name, value, limit string, ok bool) {
		verdict := "ok"
		if !ok {
			verdict = "LIMIT CROSSED"
			crossed++
		}
		fmt.Printf("%-9s %-34s limit %-12s %s\n", name, value, limit, verdict)
	}

	info, err := os.Stat(binary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "limits: %v\n", err)
		return 1
	}
	mebibytes := float64(info.Size()) / (1 << 20)
	report("binary", fmt.Sprintf("%.2f MiB", mebibytes), fmt.Sprintf("%g MiB", *maxBinaryMebibytes), mebibytes < *maxBinaryMebibytes)

	harness, err := newHarness(binary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "limits: %v\n", err)
		return 1
	}
	defer harness.close()

	startup, err := harness.medianStartup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "limits: startup: %v\n", err)
		return 1
	}
	report("startup", fmt.Sprintf("%.1f ms (median of %d)", milliseconds(startup), startupRuns),
		fmt.Sprintf("%g ms", *maxStartupMilliseconds), milliseconds(startup) < *maxStartupMilliseconds)

	withIdle, withoutIdle, err := harness.medianIdleCPU()
	if err != nil {
		fmt.Fprintf(os.Stderr, "limits: idle CPU: %v\n", err)
		return 1
	}
	// The difference cancels the CPU time of startup and exit.
	idleCost := milliseconds(withIdle - withoutIdle)
	report("idle CPU", fmt.Sprintf("%.1f ms per %s (%.1f - %.1f ms)", idleCost, idleTime, milliseconds(withIdle), milliseconds(withoutIdle)),
		fmt.Sprintf("%g ms", *maxIdleCPUMilliseconds), idleCost < *maxIdleCPUMilliseconds)

	if crossed > 0 {
		fmt.Fprintf(os.Stderr, "limits: %d limit(s) crossed; fix the regression or change the limit on purpose\n", crossed)
		return 1
	}
	return 0
}

// harness owns the local API stand-in and a temporary directory for the
// child runs.
type harness struct {
	binary   string
	baseURL  string
	listener net.Listener
	root     string
}

func newHarness(binary string) (*harness, error) {
	root, err := os.MkdirTemp("", "think-limits-")
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	// Any status completes the prewarm; the child never sends a prompt.
	go http.Serve(listener, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	return &harness{binary: binary, baseURL: "http://" + listener.Addr().String(), listener: listener, root: root}, nil
}

func (h *harness) close() {
	h.listener.Close()
	os.RemoveAll(h.root)
}

func (h *harness) medianStartup() (time.Duration, error) {
	// The first run fills the file system cache; it is not counted.
	samples := make([]time.Duration, 0, startupRuns)
	for index := range startupRuns + 1 {
		child, err := h.start(fmt.Sprintf("startup-%d", index))
		if err != nil {
			return 0, err
		}
		startup, err := child.waitReady()
		if err != nil {
			return 0, err
		}
		if _, err := child.quit(); err != nil {
			return 0, err
		}
		if index > 0 {
			samples = append(samples, startup)
		}
	}
	return median(samples), nil
}

// medianIdleCPU returns the median CPU time of runs with idleTime of idle
// and of runs without it.
func (h *harness) medianIdleCPU() (withIdle, withoutIdle time.Duration, err error) {
	withIdleSamples := make([]time.Duration, 0, idleRuns)
	withoutIdleSamples := make([]time.Duration, 0, idleRuns)
	for index := range idleRuns {
		for _, idle := range []time.Duration{idleTime, 0} {
			cpu, err := h.cpuAfterIdle(fmt.Sprintf("idle-%d-%s", index, idle), idle)
			if err != nil {
				return 0, 0, err
			}
			if idle == 0 {
				withoutIdleSamples = append(withoutIdleSamples, cpu)
			} else {
				withIdleSamples = append(withIdleSamples, cpu)
			}
		}
	}
	return median(withIdleSamples), median(withoutIdleSamples), nil
}

func (h *harness) cpuAfterIdle(name string, idle time.Duration) (time.Duration, error) {
	child, err := h.start(name)
	if err != nil {
		return 0, err
	}
	if _, err := child.waitReady(); err != nil {
		return 0, err
	}
	time.Sleep(settleTime + idle)
	return child.quit()
}

// child is one think process on its own pseudo-terminal.
type child struct {
	command *exec.Cmd
	master  *os.File
	started time.Time
	// ready receives the time of the first output that contains readyText.
	ready chan time.Time
	// readerDone closes when the master side reports the end of output.
	readerDone chan struct{}
	exited     chan error
	// output is owned by the reader goroutine until readerDone closes.
	output []byte
}

func (h *harness) start(name string) (*child, error) {
	directory := filepath.Join(h.root, name)
	home := filepath.Join(directory, "home")
	workDir := filepath.Join(directory, "work")
	for _, path := range []string{home, workDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	master, slavePath, err := openPseudoTerminal()
	if err != nil {
		return nil, fmt.Errorf("open pseudo-terminal: %w", err)
	}
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open %s: %w", slavePath, err)
	}
	// Darwin accepts the window size only on the slave side.
	size := windowSize{rows: terminalRows, columns: terminalColumns}
	if err := ioctl(slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size))); err != nil {
		slave.Close()
		master.Close()
		return nil, fmt.Errorf("set window size: %w", err)
	}

	command := exec.Command(h.binary, "chat", "-base-url", h.baseURL)
	command.Dir = workDir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"XDG_STATE_HOME=" + filepath.Join(directory, "state"),
		"XDG_CONFIG_HOME=" + filepath.Join(directory, "config"),
		"OPENROUTER_API_KEY=sk-or-dummy",
	}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	// Ctty is descriptor 0 in the child, which is the slave side.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	started := time.Now()
	err = command.Start()
	// The child has its own copy. The master sees the end of output only
	// when every copy of the slave side is closed.
	slave.Close()
	if err != nil {
		master.Close()
		return nil, err
	}
	c := &child{
		command:    command,
		master:     master,
		started:    started,
		ready:      make(chan time.Time, 1),
		readerDone: make(chan struct{}),
		exited:     make(chan error, 1),
	}
	go c.read()
	go func() { c.exited <- command.Wait() }()
	return c, nil
}

// read drains the master side, so the child never blocks on a full terminal
// buffer, and answers each device attributes query like a real terminal.
func (c *child) read() {
	defer close(c.readerDone)
	buffer := make([]byte, 32*1024)
	answered, scanned := 0, 0
	readySeen := false
	for {
		count, err := c.master.Read(buffer)
		if count > 0 {
			now := time.Now()
			c.output = append(c.output, buffer[:count]...)
			if !readySeen && bytes.Contains(c.output, []byte(readyText)) {
				readySeen = true
				c.ready <- now
			}
			// Start a little before the old end, so a query split across
			// two reads is still found.
			from := max(0, scanned-len(deviceAttributesQuery)+1)
			queries := answered + bytes.Count(c.output[from:], []byte(deviceAttributesQuery))
			for ; answered < queries; answered++ {
				if _, err := c.master.Write([]byte(terminalReply)); err != nil {
					return
				}
			}
			scanned = len(c.output)
		}
		if err != nil {
			// The master reports EIO or EOF when the last slave copy closes.
			return
		}
	}
}

// waitReady returns the startup time. On failure it stops the child.
func (c *child) waitReady() (time.Duration, error) {
	select {
	case readyAt := <-c.ready:
		return readyAt.Sub(c.started), nil
	case <-time.After(readyTimeout):
		c.abort()
		return 0, fmt.Errorf("child did not show %q within %s; output tail: %q", readyText, readyTimeout, tail(c.output))
	case err := <-c.exited:
		c.exited <- err
		c.abort()
		return 0, fmt.Errorf("child exited before it showed %q: %v; output tail: %q", readyText, err, tail(c.output))
	}
}

// quit sends /quit and returns the CPU time of the child. The kernel reports
// user and system time through wait4 rusage.
func (c *child) quit() (time.Duration, error) {
	if _, err := io.WriteString(c.master, quitInput); err != nil {
		c.abort()
		return 0, fmt.Errorf("send %q: %w", quitInput, err)
	}
	select {
	case err := <-c.exited:
		if finishErr := c.finish(); finishErr != nil {
			return 0, finishErr
		}
		if err != nil {
			return 0, fmt.Errorf("child exit after %q: %v; output tail: %q", quitInput, err, tail(c.output))
		}
		state := c.command.ProcessState
		return state.UserTime() + state.SystemTime(), nil
	case <-time.After(exitTimeout):
		c.abort()
		return 0, fmt.Errorf("child did not exit within %s after %q", exitTimeout, quitInput)
	}
}

// abort kills the process group of the child and releases the terminal.
func (c *child) abort() {
	syscall.Kill(-c.command.Process.Pid, syscall.SIGKILL)
	<-c.exited
	c.exited <- nil
	c.finish()
}

func (c *child) finish() error {
	defer c.master.Close()
	select {
	case <-c.readerDone:
		return nil
	case <-time.After(exitTimeout):
		return errors.New("terminal output did not end after the child exited")
	}
}

type windowSize struct {
	rows, columns, widthPixels, heightPixels uint16
}

func ioctl(fd, request, argument uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, argument)
	if errno != 0 {
		return errno
	}
	return nil
}

func median(samples []time.Duration) time.Duration {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
	return sorted[middle]
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func tail(output []byte) []byte {
	const size = 400
	if len(output) <= size {
		return output
	}
	return output[len(output)-size:]
}
