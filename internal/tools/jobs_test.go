package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

type jobTools struct {
	jobs    *Jobs
	bash    Tool
	bashJob Tool
	root    string
}

func newJobTools(t *testing.T) jobTools {
	t.Helper()
	root := t.TempDir()
	outputs := filepath.Join(t.TempDir(), "outputs")
	jobs := newTestJobs(t, outputs)
	set, err := Standard(root, outputs, jobs)
	if err != nil {
		t.Fatal(err)
	}
	bash, _ := set.Lookup("bash")
	bashJob, _ := set.Lookup("bash_job")
	return jobTools{jobs: jobs, bash: bash, bashJob: bashJob, root: root}
}

func (tools jobTools) startResult(t *testing.T, command string) Result {
	t.Helper()
	input, err := json.Marshal(map[string]any{"command": command, "background": true})
	if err != nil {
		t.Fatal(err)
	}
	return runTool(t, tools.bash, string(input))
}

func (tools jobTools) start(t *testing.T, command string) (id, pid int) {
	t.Helper()
	result := tools.startResult(t, command)
	if result.IsError {
		t.Fatalf("start %q = %+v", command, result)
	}
	if _, err := fmt.Sscanf(result.Content, "started job %d (pid %d)", &id, &pid); err != nil {
		t.Fatalf("start result %q: %v", result.Content, err)
	}
	return id, pid
}

func (tools jobTools) call(t *testing.T, action string, id int) Result {
	t.Helper()
	return runTool(t, tools.bashJob, fmt.Sprintf(`{"action":%q,"id":%d}`, action, id))
}

// readOutput splits an output result into the new output and the status line.
func (tools jobTools) readOutput(t *testing.T, id int) (text, status string) {
	t.Helper()
	result := tools.call(t, "output", id)
	cut := strings.LastIndex(result.Content, "[job ")
	if result.IsError || cut < 0 || !strings.HasSuffix(result.Content, "]") {
		t.Fatalf("output = %+v", result)
	}
	text, status = result.Content[:cut], result.Content[cut:]
	if text == "(no new output)\n" {
		text = ""
	}
	return text, status
}

// readUntil reads the job output until the collected text contains want.
func (tools jobTools) readUntil(t *testing.T, id int, want string) (collected, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(collected, want) {
		if time.Now().After(deadline) {
			t.Fatalf("job %d output %q has no %q", id, collected, want)
		}
		var text string
		text, status = tools.readOutput(t, id)
		collected += text
		if text == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return collected, status
}

func (tools jobTools) waitForEnd(t *testing.T, id int) {
	t.Helper()
	target, found := tools.jobs.find(id)
	if !found {
		t.Fatalf("job %d missing", id)
	}
	select {
	case <-target.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("job %d did not end", id)
	}
}

func savedPath(t *testing.T, status string) string {
	t.Helper()
	_, path, found := strings.Cut(status, " in ")
	if !found {
		t.Fatalf("status %q names no output file", status)
	}
	return strings.TrimSuffix(path, "]")
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive (kill 0: %v)", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStandardToolOrder(t *testing.T) {
	outputs := filepath.Join(t.TempDir(), "outputs")
	set, err := Standard(t.TempDir(), outputs, newTestJobs(t, outputs))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range set.All() {
		names = append(names, tool.Name)
	}
	want := []string{"read_file", "write_file", "edit_file", "bash", "bash_job", "glob", "grep", "todo_write"}
	if !slices.Equal(names, want) {
		t.Fatalf("tool order = %v, want %v", names, want)
	}
	if bashJob, _ := set.Lookup("bash_job"); bashJob.ReadOnly {
		t.Fatal("bash_job stops processes, so it must not be ReadOnly")
	}
	if _, err := Standard(t.TempDir(), outputs, newTestJobs(t, filepath.Join(t.TempDir(), "other"))); err == nil {
		t.Fatal("Standard accepted jobs with a different output directory")
	}
}

func TestBackgroundBashReturnsAtOnce(t *testing.T) {
	tools := newJobTools(t)
	started := time.Now()
	result := tools.startResult(t, "sleep 30")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("background start took %s", elapsed)
	}
	var id, pid int
	if _, err := fmt.Sscanf(result.Content, "started job %d (pid %d)", &id, &pid); err != nil || result.IsError {
		t.Fatalf("start = %+v (%v)", result, err)
	}
	want := fmt.Sprintf("started job 1 (pid %d); use bash_job with action output to read its output", pid)
	if result.Content != want {
		t.Fatalf("start = %q, want %q", result.Content, want)
	}
	if group, err := syscall.Getpgid(pid); err != nil || group != pid {
		t.Fatalf("job process group = %d (%v), want its own group %d", group, err, pid)
	}
}

func TestBashJobOutputIsIncremental(t *testing.T) {
	tools := newJobTools(t)
	// The first write ends inside the rune "€"; the second write completes it.
	id, _ := tools.start(t, `printf 'one\n\342\202'; while [ ! -f go ]; do sleep 0.02; done; printf '\254 two\n'; sleep 30`)
	first, _ := tools.readUntil(t, id, "one\n")
	if first != "one\n" {
		t.Fatalf("first output = %q, want the complete line without the split rune", first)
	}
	putTestFile(t, tools.root, "go", "")
	second, status := tools.readUntil(t, id, "two\n")
	if second != "€ two\n" {
		t.Fatalf("second output = %q, want only the new output", second)
	}
	path := savedPath(t, status)
	if want := "[job 1 running; full output (12 bytes) in " + path + "]"; status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "one\n€ two\n" {
		t.Fatalf("output file = %q", saved)
	}
	if text, _ := tools.readOutput(t, id); text != "" {
		t.Fatalf("third output = %q, want no new output", text)
	}
}

func TestBashJobOutputKeepsNewestBytesAndNotesDrop(t *testing.T) {
	tools := newJobTools(t)
	// Nobody reads while the job writes 1 MB, so the job must not wait for a reader.
	id, _ := tools.start(t, `head -c 1000000 /dev/zero | tr '\0' a; echo; echo done`)
	tools.waitForEnd(t, id)
	text, status := tools.readOutput(t, id)
	total := 1000000 + len("\ndone\n")
	notice := fmt.Sprintf("[%d earlier bytes dropped; use read_file on the output file to read them]\n", total-outputInlineBytes)
	want := notice + strings.Repeat("a", outputInlineBytes-len("\ndone\n")) + "\ndone\n"
	if text != want {
		t.Fatalf("output has %d bytes and starts with %.120q, want %d bytes after the notice %q", len(text), text, outputInlineBytes, notice)
	}
	path := savedPath(t, status)
	if wantStatus := fmt.Sprintf("[job 1 exited(0); full output (%d bytes) in %s]", total, path); status != wantStatus {
		t.Fatalf("status = %q, want %q", status, wantStatus)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(total) || info.Mode().Perm() != 0o600 {
		t.Fatalf("output file size %d, mode %v", info.Size(), info.Mode().Perm())
	}
	if again, _ := tools.readOutput(t, id); again != "" {
		t.Fatalf("second output = %q, want no new output", again)
	}
}

func TestBashJobListShowsStates(t *testing.T) {
	tools := newJobTools(t)
	if list := tools.call(t, "list", 0); list.Content != "invalid input: id does not apply to action list" {
		t.Fatalf("list with id = %+v", list)
	}
	if list := runTool(t, tools.bashJob, `{"action":"list"}`); list.IsError || list.Content != "no jobs" {
		t.Fatalf("empty list = %+v", list)
	}
	tools.start(t, "sleep 30")
	exited, _ := tools.start(t, "echo bye; exit 3")
	killed, _ := tools.start(t, "sleep 30")
	tools.start(t, "sleep 30 # "+strings.Repeat("é", 100))
	tools.waitForEnd(t, exited)
	if result := tools.call(t, "kill", killed); result.IsError || result.Content != "job 3 stopped: killed (signal: terminated)" {
		t.Fatalf("kill = %+v", result)
	}
	if text, status := tools.readOutput(t, exited); text != "bye\n" || !strings.HasPrefix(status, "[job 2 exited(3); ") {
		t.Fatalf("output of exited job = %q, %q", text, status)
	}

	list := runTool(t, tools.bashJob, `{"action":"list"}`)
	lines := strings.Split(list.Content, "\n")
	if list.IsError || len(lines) != 4 {
		t.Fatalf("list = %+v", list)
	}
	checks := []struct{ prefix, suffix string }{
		{"job 1: running for ", ": sleep 30"},
		{"job 2: exited(3) after ", ": echo bye; exit 3"},
		{"job 3: killed (signal: terminated) after ", ": sleep 30"},
		{"job 4: running for ", ": sleep 30 # " + strings.Repeat("é", 80-len("sleep 30 # ")) + "..."},
	}
	for index, check := range checks {
		if !strings.HasPrefix(lines[index], check.prefix) || !strings.HasSuffix(lines[index], check.suffix) {
			t.Errorf("line %d = %q, want %q ... %q", index+1, lines[index], check.prefix, check.suffix)
		}
	}
}

func TestBashJobKillStopsWholeProcessGroup(t *testing.T) {
	tools := newJobTools(t)
	id, _ := tools.start(t, "sleep 30 & echo $!; wait")
	text, _ := tools.readUntil(t, id, "\n")
	childPID, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("no child pid in %q", text)
	}
	result := tools.call(t, "kill", id)
	if result.IsError || result.Content != "job 1 stopped: killed (signal: terminated)" {
		t.Fatalf("kill = %+v", result)
	}
	waitForProcessExit(t, childPID)
	if again := tools.call(t, "kill", id); again.IsError || again.Content != "job 1 already ended: killed (signal: terminated)" {
		t.Fatalf("second kill = %+v", again)
	}
}

func TestBashJobKillEscalatesToSIGKILL(t *testing.T) {
	tools := newJobTools(t)
	id, _ := tools.start(t, "trap '' TERM; echo ready; sleep 30")
	tools.readUntil(t, id, "ready\n")
	started := time.Now()
	result := tools.call(t, "kill", id)
	if elapsed := time.Since(started); elapsed < jobStopDelay {
		t.Fatalf("kill returned after %s, before the SIGTERM delay", elapsed)
	}
	if result.IsError || result.Content != "job 1 stopped: killed (signal: killed)" {
		t.Fatalf("kill = %+v", result)
	}
}

func TestJobsCloseKillsRunningJobs(t *testing.T) {
	tools := newJobTools(t)
	id, _ := tools.start(t, "sleep 30 & echo $!; wait")
	text, status := tools.readUntil(t, id, "\n")
	childPID, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("no child pid in %q", text)
	}
	path := savedPath(t, status)
	if !tools.jobs.writing(path) {
		t.Fatal("pruning may remove the output file of a running job")
	}
	if err := tools.jobs.Close(); err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, childPID)
	if list := runTool(t, tools.bashJob, `{"action":"list"}`); !strings.HasPrefix(list.Content, "job 1: killed (signal: killed) after ") {
		t.Fatalf("list after Close = %+v", list)
	}
	if tools.jobs.writing(path) {
		t.Fatal("an ended job still protects its output file")
	}
	if result := tools.startResult(t, "sleep 30"); !result.IsError || result.Content != "cannot start a job: the session is ending" {
		t.Fatalf("start after Close = %+v", result)
	}
}

func TestBashJobsLimitRunningJobs(t *testing.T) {
	tools := newJobTools(t)
	for range maxRunningJobs {
		tools.start(t, "sleep 30")
	}
	refused := tools.startResult(t, "sleep 30")
	if !refused.IsError || !strings.HasPrefix(refused.Content, "cannot start a job: 8 jobs are running") {
		t.Fatalf("ninth start = %+v", refused)
	}
	if result := tools.call(t, "kill", 1); result.IsError {
		t.Fatalf("kill = %+v", result)
	}
	if id, _ := tools.start(t, "sleep 30"); id != 9 {
		t.Fatalf("new job id = %d, want 9", id)
	}
}

func TestBashJobRejectsInvalidInput(t *testing.T) {
	tools := newJobTools(t)
	tests := []struct {
		tool  Tool
		input string
		want  string
	}{
		{tools.bash, `{"command":"sleep 30","background":true,"timeout_seconds":5}`, "invalid input: timeout_seconds does not apply to a background command"},
		{tools.bashJob, `{}`, `invalid input: action must be list, output, or kill, not ""`},
		{tools.bashJob, `{"action":"stop","id":1}`, `invalid input: action must be list, output, or kill, not "stop"`},
		{tools.bashJob, `{"action":"output"}`, "invalid input: action output requires id"},
		{tools.bashJob, `{"action":"kill"}`, "invalid input: action kill requires id"},
		{tools.bashJob, `{"action":"output","id":"1"}`, "invalid input: json: "},
		{tools.bashJob, `{"action":"output","id":7}`, "no job with id 7; use bash_job action list to see the jobs"},
		{tools.bashJob, `{"action":"kill","id":0}`, "no job with id 0; use bash_job action list to see the jobs"},
	}
	for _, test := range tests {
		result := runTool(t, test.tool, test.input)
		if !result.IsError || !strings.HasPrefix(result.Content, test.want) {
			t.Errorf("%s %s = %+v, want error %q", test.tool.Name, test.input, result, test.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tools.bash.Run(ctx, json.RawMessage(`{"command":"sleep 30","background":true}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("background start after cancellation = %v", err)
	}
	if list := runTool(t, tools.bashJob, `{"action":"list"}`); list.Content != "no jobs" {
		t.Fatalf("a rejected call started a job: %+v", list)
	}
}

func TestJobNoticesReportEachEndOnceInEndOrder(t *testing.T) {
	tools := newJobTools(t)
	slow, _ := tools.start(t, `printf 'one\ntwo\nthree\nfour\n'; until [ -e go ]; do sleep 0.01; done; exit 3`)
	fast, _ := tools.start(t, "exit 0")
	tools.waitForEnd(t, fast)
	if err := os.WriteFile(filepath.Join(tools.root, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tools.waitForEnd(t, slow)
	// A read of the final output does not hide the end, and the notice
	// still holds the output.
	tools.readOutput(t, slow)
	want := []string{
		"background job 2 (exit 0) exited with code 0; no output",
		`background job 1 (printf 'one\ntwo\nthree\nfour\n'; until [ -e go ]; do sleep 0.01; done; exit 3) exited with code 3; last output: "two\nthree\nfour"`,
	}
	if got := tools.jobs.Notices(); !slices.Equal(got, want) {
		t.Fatalf("notices = %q, want %q", got, want)
	}
	if again := tools.jobs.Notices(); again != nil {
		t.Fatalf("second notices = %q, want none", again)
	}
}

func TestJobNoticesSkipEndsThatKillReported(t *testing.T) {
	tools := newJobTools(t)
	running, _ := tools.start(t, "sleep 30")
	if result := tools.call(t, "kill", running); result.IsError {
		t.Fatalf("kill = %+v", result)
	}
	ended, _ := tools.start(t, "exit 4")
	tools.waitForEnd(t, ended)
	if result := tools.call(t, "kill", ended); result.Content != "job 2 already ended: exited(4)" {
		t.Fatalf("kill of an ended job = %+v", result)
	}
	signaled, _ := tools.start(t, "echo bye; kill -TERM $$")
	tools.waitForEnd(t, signaled)
	want := []string{`background job 3 (echo bye; kill -TERM $$) was killed (signal: terminated); last output: "bye"`}
	if got := tools.jobs.Notices(); !slices.Equal(got, want) {
		t.Fatalf("notices = %q, want %q", got, want)
	}
}

func TestJobNoticesBoundTheOutput(t *testing.T) {
	tools := newJobTools(t)
	long, _ := tools.start(t, `printf 'first\n%0500d\n' 0`)
	tools.waitForEnd(t, long)
	// 1000 two-byte runes are more than the kept tail, so the tail starts
	// inside a rune.
	wide, _ := tools.start(t, `s=$(printf '%1000s' ''); printf '%s\n' "${s// /é}"`)
	tools.waitForEnd(t, wide)
	notices := tools.jobs.Notices()
	want := []string{
		`last output: "...` + strings.Repeat("0", noticeRunes) + `"`,
		`last output: "...` + strings.Repeat("é", noticeRunes) + `"`,
	}
	if len(notices) != len(want) {
		t.Fatalf("notices = %q", notices)
	}
	for index, notice := range notices {
		if !strings.HasSuffix(notice, want[index]) || !utf8.ValidString(notice) {
			t.Errorf("notice %d = %q, want suffix %q", index, notice, want[index])
		}
	}
}

func TestJobNoticesUnderConcurrentEnds(t *testing.T) {
	tools := newJobTools(t)
	const count = 6
	for range count {
		tools.start(t, "exit 1")
	}
	var mutex sync.Mutex
	seen := make(map[string]int)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				notices := tools.jobs.Notices()
				mutex.Lock()
				for _, notice := range notices {
					seen[notice]++
				}
				complete := len(seen) == count
				mutex.Unlock()
				if complete {
					return
				}
			}
		}()
	}
	group.Wait()
	for id := 1; id <= count; id++ {
		notice := fmt.Sprintf("background job %d (exit 1) exited with code 1; no output", id)
		if seen[notice] != 1 {
			t.Errorf("notice %q came %d times, want once", notice, seen[notice])
		}
	}
	if len(seen) != count {
		t.Errorf("notices = %q", seen)
	}
}
