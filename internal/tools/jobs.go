package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// maxRunningJobs bounds the background processes of one session.
	maxRunningJobs = 8
	// jobStopDelay is the time that a job gets to stop after SIGTERM. Then
	// its process group gets SIGKILL.
	jobStopDelay = 2 * time.Second
	// listedCommandRunes bounds the command text on one line of a job list.
	listedCommandRunes = 80
	// noticeLines and noticeRunes bound the output in a notice about a job
	// end. noticeTailBytes holds noticeRunes runes of up to 3 bytes each.
	noticeLines     = 3
	noticeRunes     = 300
	noticeTailBytes = 1024
)

// Jobs owns the background commands of one session. Each job runs in its own
// process group, so a stop also reaches the children of the command. The
// session must call Close when it ends, so that no job outlives the session.
type Jobs struct {
	outputDirectory string

	mutex sync.Mutex
	// all holds every job of the session in start order. The ID of a job is
	// its index plus one, so an ID never names two jobs.
	all []*job
	// ended holds the jobs that ended since the last Notices call, in the
	// order in which they ended.
	ended  []*job
	closed bool
}

// NewJobs returns an empty job set. The output files of the jobs go to
// outputDirectory. Standard must get the same directory for the bash tool.
func NewJobs(outputDirectory string) *Jobs {
	return &Jobs{outputDirectory: outputDirectory}
}

type job struct {
	id          int
	commandLine string
	pid         int
	startedAt   time.Time
	output      *jobOutput
	// done closes when the shell has exited and its output has ended. end is
	// set before done closes, so a reader that sees done closed can read end
	// without a lock.
	done chan struct{}
	end  jobEnd
	// known is true when a bash_job kill result tells the model how the job
	// ended, so Notices must not tell it again. Jobs.mutex guards it.
	known bool
}

// jobEnd records how the shell of a job ended.
type jobEnd struct {
	at time.Time
	// state is nil only when the wait for the shell failed.
	state *os.ProcessState
	// problem is a wait error that state does not show, for example output
	// that a process outside the group kept open after the shell exited.
	problem error
}

func (end jobEnd) String() string {
	var description string
	switch {
	case end.state == nil:
		return "ended; the wait for its shell failed: " + oneLine(end.problem)
	case end.state.Exited():
		description = fmt.Sprintf("exited(%d)", end.state.ExitCode())
	default:
		// Without WUNTRACED, a waited process either exited or got a signal.
		description = "killed (" + end.state.String() + ")"
	}
	if end.problem != nil {
		description += " (" + oneLine(end.problem) + ")"
	}
	return description
}

// finished returns how the job ended, or false while the job runs.
func (j *job) finished() (jobEnd, bool) {
	select {
	case <-j.done:
		return j.end, true
	default:
		return jobEnd{}, false
	}
}

// sentence describes the end for a notice, e.g. "exited with code 1".
func (end jobEnd) sentence() string {
	var description string
	switch {
	case end.state == nil:
		return "ended; the wait for its shell failed: " + oneLine(end.problem)
	case end.state.Exited():
		description = fmt.Sprintf("exited with code %d", end.state.ExitCode())
	default:
		description = "was killed (" + end.state.String() + ")"
	}
	if end.problem != nil {
		description += " (" + oneLine(end.problem) + ")"
	}
	return description
}

// watch records how the shell of target ended and puts the job in the queue
// for Notices before done closes, so a job that shows as ended is also in
// the queue. It runs in its own goroutine for the life of the job.
func (j *Jobs) watch(target *job, command *exec.Cmd) {
	waitErr := command.Wait()
	target.output.closeFile()
	end := jobEnd{at: time.Now(), state: command.ProcessState}
	var exitError *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitError) {
		end.problem = waitErr
	}
	target.end = end
	j.mutex.Lock()
	j.ended = append(j.ended, target)
	j.mutex.Unlock()
	close(target.done)
}

// Notices returns one line for each job that ended since the last call,
// oldest end first, so the session can tell the model about ends that it
// did not ask for. Each job is in at most one result. A job that bash_job
// kill stopped is in none, because the kill result told the model already.
func (j *Jobs) Notices() []string {
	j.mutex.Lock()
	var unknown []*job
	for _, target := range j.ended {
		if !target.known {
			unknown = append(unknown, target)
		}
	}
	j.ended = nil
	j.mutex.Unlock()
	if len(unknown) == 0 {
		return nil
	}
	notices := make([]string, len(unknown))
	for index, target := range unknown {
		notices[index] = fmt.Sprintf("background job %d (%s) %s; %s", target.id, shortCommand(target.commandLine), target.end.sentence(), target.output.lastOutput())
	}
	return notices
}

// setKnown records whether a kill result tells the model how target ended.
func (j *Jobs) setKnown(target *job, known bool) {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	target.known = known
}

// jobRefusal is an expected reason not to start a job. The model gets it as
// an error result.
type jobRefusal string

func (refusal jobRefusal) Error() string {
	return string(refusal)
}

// start runs commandLine in root as a new job and does not wait for it.
func (j *Jobs) start(root, commandLine string) (Result, error) {
	started, err := j.launch(root, commandLine)
	var refusal jobRefusal
	if errors.As(err, &refusal) {
		return Result{Content: refusal.Error(), IsError: true}, nil
	}
	if err != nil {
		return Result{}, err
	}
	content := fmt.Sprintf("started job %d (pid %d); use bash_job with action output to read its output", started.id, started.pid)
	keep := func(path string) bool {
		return path == started.output.path || j.writing(path)
	}
	if err := pruneOutputDirectory(j.outputDirectory, keep, maxOutputDirectoryBytes); err != nil {
		content += "; older output files were not removed: " + oneLine(err)
	}
	return Result{Content: content}, nil
}

// launch starts the shell under the lock, so Close cannot miss the new job.
func (j *Jobs) launch(root, commandLine string) (*job, error) {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	if j.closed {
		return nil, jobRefusal("cannot start a job: the session is ending")
	}
	running := 0
	for _, candidate := range j.all {
		if _, finished := candidate.finished(); !finished {
			running++
		}
	}
	if running >= maxRunningJobs {
		return nil, jobRefusal(fmt.Sprintf("cannot start a job: %d jobs are running, which is the maximum; stop one with bash_job action kill", running))
	}
	file, err := createOutputFile(j.outputDirectory)
	if err != nil {
		return nil, jobRefusal("cannot start a job: cannot create its output file: " + oneLine(err))
	}
	output := &jobOutput{path: file.Name(), file: file}
	command := exec.Command("/bin/bash", "-c", commandLine)
	command.Dir = root
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = output
	command.Stderr = output
	command.WaitDelay = pipeDrainDelay
	if err := command.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("bash: %w", err), file.Close(), os.Remove(file.Name()))
	}
	started := &job{
		id:          len(j.all) + 1,
		commandLine: commandLine,
		pid:         command.Process.Pid,
		startedAt:   time.Now(),
		output:      output,
		done:        make(chan struct{}),
	}
	j.all = append(j.all, started)
	go j.watch(started, command)
	return started, nil
}

// writing reports whether a running job writes its output to path. Pruning
// must keep such a file, because results name it and the job still adds to it.
func (j *Jobs) writing(path string) bool {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	for _, candidate := range j.all {
		if candidate.output.path != path {
			continue
		}
		if _, finished := candidate.finished(); !finished {
			return true
		}
	}
	return false
}

func (j *Jobs) find(id int) (*job, bool) {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	if id < 1 || id > len(j.all) {
		return nil, false
	}
	return j.all[id-1], true
}

func unknownJob(id int) Result {
	return Result{Content: fmt.Sprintf("no job with id %d; use bash_job action list to see the jobs", id), IsError: true}
}

func (j *Jobs) list() Result {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	if len(j.all) == 0 {
		return Result{Content: "no jobs"}
	}
	now := time.Now()
	var text strings.Builder
	for index, listed := range j.all {
		if index > 0 {
			text.WriteByte('\n')
		}
		if end, finished := listed.finished(); finished {
			fmt.Fprintf(&text, "job %d: %s after %s: %s", listed.id, end, roundRuntime(end.at.Sub(listed.startedAt)), shortCommand(listed.commandLine))
		} else {
			fmt.Fprintf(&text, "job %d: running for %s: %s", listed.id, roundRuntime(now.Sub(listed.startedAt)), shortCommand(listed.commandLine))
		}
	}
	return Result{Content: text.String()}
}

// roundRuntime keeps a run time short but keeps a fast exit visible.
func roundRuntime(runtime time.Duration) time.Duration {
	if runtime < time.Second {
		return runtime.Round(time.Millisecond)
	}
	return runtime.Round(time.Second)
}

// shortCommand puts the command on one line and keeps its first
// listedCommandRunes runes.
func shortCommand(commandLine string) string {
	flat := strings.Join(strings.Fields(commandLine), " ")
	count := 0
	for index := range flat {
		if count == listedCommandRunes {
			return flat[:index] + "..."
		}
		count++
	}
	return flat
}

func (j *Jobs) readOutput(id int) Result {
	target, found := j.find(id)
	if !found {
		return unknownJob(id)
	}
	// The state comes before the output. Output that arrives between the two
	// steps stays for the next call, so no byte is lost or shown twice.
	end, finished := target.finished()
	status := fmt.Sprintf("job %d running", id)
	if finished {
		status = fmt.Sprintf("job %d %s", id, end)
	}
	return Result{Content: target.output.take(status, finished)}
}

// kill sends SIGTERM to the process group of the job and SIGKILL after
// jobStopDelay. The SIGKILL also goes out when the shell stopped in time,
// because a child that ignores SIGTERM can stay in the group after the shell.
func (j *Jobs) kill(id int) Result {
	target, found := j.find(id)
	if !found {
		return unknownJob(id)
	}
	// Each successful result below tells the model how the job ended. The
	// mark comes before the signal, so a Notices call during the stop cannot
	// report the job. A failed stop removes the mark again.
	j.setKnown(target, true)
	if end, finished := target.finished(); finished {
		return Result{Content: fmt.Sprintf("job %d already ended: %s", id, end)}
	}
	if err := signalGroup(target.pid, syscall.SIGTERM); err != nil {
		j.setKnown(target, false)
		return Result{Content: fmt.Sprintf("cannot stop job %d: %v", id, err), IsError: true}
	}
	timer := time.NewTimer(jobStopDelay)
	defer timer.Stop()
	select {
	case <-target.done:
	case <-timer.C:
	}
	if err := signalGroup(target.pid, syscall.SIGKILL); err != nil {
		j.setKnown(target, false)
		return Result{Content: fmt.Sprintf("cannot kill job %d: %v", id, err), IsError: true}
	}
	<-target.done
	return Result{Content: fmt.Sprintf("job %d stopped: %s", id, target.end)}
}

// Close kills the process group of every running job and waits until each
// shell has ended. Later starts fail, so no job can outlive the session.
func (j *Jobs) Close() error {
	j.mutex.Lock()
	j.closed = true
	all := j.all
	j.mutex.Unlock()

	killed := make([]*job, 0, len(all))
	var failures []error
	for _, target := range all {
		if _, finished := target.finished(); finished {
			continue
		}
		if err := signalGroup(target.pid, syscall.SIGKILL); err != nil {
			failures = append(failures, fmt.Errorf("tools: kill job %d: %w", target.id, err))
			continue
		}
		killed = append(killed, target)
	}
	for _, target := range killed {
		<-target.done
	}
	return errors.Join(failures...)
}

// signalGroup sends signal to every process in the group that the shell of a
// job leads. ESRCH means that the group is gone. EPERM means that no process
// in the group can get the signal: macOS gives it for a group that holds only
// zombies, and the shell is our own process, so it is not alive. In both
// cases nothing is left to stop.
func signalGroup(leader int, signal syscall.Signal) error {
	err := syscall.Kill(-leader, signal)
	if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

// jobOutput receives the combined output of one job. A ring keeps the
// output that no output call returned yet, and a file gets all output. A
// reader holds the lock only to copy at most outputInlineBytes, so it never
// stops the job for long.
type jobOutput struct {
	mutex sync.Mutex
	// unread is a ring buffer. It is allocated at the first write and
	// released when the job has ended and all output was read.
	unread       []byte
	unreadStart  int
	unreadLength int
	// pending counts the bytes that arrived since the last read. When it is
	// larger than unreadLength, the ring dropped the oldest bytes.
	pending int64
	total   int64
	// tail holds the last bytes of the output, also when an output call
	// read them already, for the notice about the end of the job.
	tail       [noticeTailBytes]byte
	tailLength int

	path string
	// file is nil after the job ends or after a write fails. The writes are
	// not buffered, so the file is complete when a result names it.
	file    *os.File
	saved   int64
	saveErr error
}

func (o *jobOutput) Write(chunk []byte) (int, error) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	o.total += int64(len(chunk))
	o.pending += int64(len(chunk))
	o.keepUnread(chunk)
	o.keepTail(chunk)
	if o.file != nil {
		o.save(chunk)
	}
	return len(chunk), nil
}

func (o *jobOutput) keepTail(chunk []byte) {
	if len(chunk) >= len(o.tail) {
		o.tailLength = copy(o.tail[:], chunk[len(chunk)-len(o.tail):])
		return
	}
	kept := min(o.tailLength, len(o.tail)-len(chunk))
	copy(o.tail[:kept], o.tail[o.tailLength-kept:o.tailLength])
	o.tailLength = kept + copy(o.tail[kept:], chunk)
}

// lastOutput describes the end of the output for a notice: at most the last
// noticeLines lines and noticeRunes runes, quoted, so the notice stays on
// one line.
func (o *jobOutput) lastOutput() string {
	o.mutex.Lock()
	tail := o.tail[:o.tailLength]
	if o.total > int64(o.tailLength) {
		tail = trimIncompleteRuneStart(tail)
	}
	text := strings.TrimRight(string(tail), "\r\n")
	o.mutex.Unlock()
	if text == "" {
		return "no output"
	}
	if lines := strings.Split(text, "\n"); len(lines) > noticeLines {
		text = strings.Join(lines[len(lines)-noticeLines:], "\n")
	}
	if extra := utf8.RuneCountInString(text) - noticeRunes; extra > 0 {
		for index := range text {
			if extra == 0 {
				text = "..." + text[index:]
				break
			}
			extra--
		}
	}
	return "last output: " + strconv.Quote(text)
}

func (o *jobOutput) keepUnread(chunk []byte) {
	if o.unread == nil {
		o.unread = make([]byte, outputInlineBytes)
	}
	size := len(o.unread)
	if len(chunk) >= size {
		copy(o.unread, chunk[len(chunk)-size:])
		o.unreadStart, o.unreadLength = 0, size
		return
	}
	end := (o.unreadStart + o.unreadLength) % size
	copied := copy(o.unread[end:], chunk)
	copy(o.unread, chunk[copied:])
	o.unreadLength += len(chunk)
	if overflow := o.unreadLength - size; overflow > 0 {
		o.unreadStart = (o.unreadStart + overflow) % size
		o.unreadLength = size
	}
}

// save writes chunk to the output file up to maxSavedOutputBytes. After a
// write error the partial file stays, because earlier results name it.
func (o *jobOutput) save(chunk []byte) {
	room := maxSavedOutputBytes - o.saved
	if room <= 0 {
		return
	}
	if int64(len(chunk)) > room {
		chunk = chunk[:room]
	}
	written, err := o.file.Write(chunk)
	o.saved += int64(written)
	if err != nil {
		o.saveErr = errors.Join(err, o.file.Close())
		o.file = nil
	}
}

func (o *jobOutput) closeFile() {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if o.file == nil {
		return
	}
	if err := o.file.Close(); err != nil {
		o.saveErr = err
	}
	o.file = nil
}

// take returns the output that no earlier call returned, then a status line.
// While the job runs, it keeps back a rune that the job did not finish
// writing, so that a cut between two calls cannot split a rune.
func (o *jobOutput) take(status string, final bool) string {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	unread := o.orderedUnread()
	dropped := o.pending - int64(len(unread))
	if dropped > 0 {
		trimmed := trimIncompleteRuneStart(unread)
		dropped += int64(len(unread) - len(trimmed))
		unread = trimmed
	}
	taken := unread
	if !final {
		taken = trimIncompleteRuneEnd(unread)
	}
	o.consumeAllBut(len(unread) - len(taken))
	if final {
		// No write can follow the end of the job, so the ring is not needed.
		o.unread, o.unreadStart = nil, 0
	}

	var text strings.Builder
	text.Grow(len(taken) + len(status) + len(o.path) + 256)
	if dropped > 0 {
		fmt.Fprintf(&text, "[%d earlier bytes dropped; use read_file on the output file to read them]\n", dropped)
	}
	if len(taken) == 0 && dropped == 0 {
		text.WriteString("(no new output)\n")
	}
	text.Write(taken)
	if len(taken) > 0 && !bytes.HasSuffix(taken, []byte("\n")) {
		text.WriteByte('\n')
	}
	text.WriteString("[" + status + "; " + o.fileNote() + "]")
	return text.String()
}

// orderedUnread copies the ring content, oldest bytes first.
func (o *jobOutput) orderedUnread() []byte {
	if o.unreadLength == 0 {
		return nil
	}
	end := o.unreadStart + o.unreadLength
	if end <= len(o.unread) {
		return bytes.Clone(o.unread[o.unreadStart:end])
	}
	ordered := make([]byte, 0, o.unreadLength)
	ordered = append(ordered, o.unread[o.unreadStart:]...)
	return append(ordered, o.unread[:end-len(o.unread)]...)
}

// consumeAllBut marks the ring as read except for its last held bytes.
func (o *jobOutput) consumeAllBut(held int) {
	if o.unreadLength > 0 {
		o.unreadStart = (o.unreadStart + o.unreadLength - held) % len(o.unread)
	}
	o.unreadLength = held
	o.pending = int64(held)
}

func (o *jobOutput) fileNote() string {
	switch {
	case o.saveErr != nil:
		return fmt.Sprintf("output file %s holds only the first %d bytes: %s", o.path, o.saved, oneLine(o.saveErr))
	case o.total > maxSavedOutputBytes:
		return fmt.Sprintf("output file %s holds only the first %d of %d bytes", o.path, maxSavedOutputBytes, o.total)
	default:
		return fmt.Sprintf("full output (%d bytes) in %s", o.total, o.path)
	}
}

func oneLine(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", "; ")
}

// bashJobTool is one tool for list, output, and kill, so the model learns one
// schema for its jobs.
func bashJobTool(jobs *Jobs) Tool {
	return Tool{
		Name:        "bash_job",
		Description: "Manage the background jobs that bash starts with background true. Action list shows each job with its id, state, run time, and command. Action output returns the output of job id that no earlier output call returned, at most 32 KiB; when more arrived, it drops the oldest part and says so. The full output of each job goes to a file that the result names; use read_file to read it. Action kill sends SIGTERM to the job and its child processes, then SIGKILL after 2 seconds, and returns the final state. All jobs stop when the session ends.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["list","output","kill"]},"id":{"type":"integer","minimum":1,"description":"Job id. Required for output and kill."}},"required":["action"]}`),
		// ReadOnly is false because action kill stops processes. The flag
		// applies to the whole tool, so the read actions follow the same rules.
		ReadOnly: false,
		Run: func(_ context.Context, input json.RawMessage) (Result, error) {
			var arguments struct {
				Action string `json:"action"`
				ID     *int   `json:"id"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			switch arguments.Action {
			case "list":
				if arguments.ID != nil {
					return invalidInput(errors.New("id does not apply to action list")), nil
				}
				return jobs.list(), nil
			case "output":
				if arguments.ID == nil {
					return invalidInput(errors.New("action output requires id")), nil
				}
				return jobs.readOutput(*arguments.ID), nil
			case "kill":
				if arguments.ID == nil {
					return invalidInput(errors.New("action kill requires id")), nil
				}
				return jobs.kill(*arguments.ID), nil
			default:
				return invalidInput(fmt.Errorf("action must be list, output, or kill, not %q", arguments.Action)), nil
			}
		},
	}
}
