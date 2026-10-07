package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func putTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	filename := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runTool(t *testing.T, tool Tool, input string) Result {
	t.Helper()
	result, err := tool.Run(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReadFileRangesAndBounds(t *testing.T) {
	root := t.TempDir()
	putTestFile(t, root, "lines.txt", "one\ntwo\nthree\n")
	tool := lookup(t, root, "read_file")
	cases := []struct {
		input   string
		want    string
		isError bool
	}{
		{`{"path":"lines.txt"}`, "1: one\n2: two\n3: three\n[end of file: 3 lines]", false},
		{`{"path":"lines.txt","offset":2,"limit":1}`, "2: two\n[truncated: line limit; continue with offset 3]", false},
		{`{"path":"lines.txt","offset":3,"limit":1}`, "3: three\n[end of file: 3 lines]", false},
		{`{"path":"lines.txt","offset":4}`, "[end of file: 3 lines; offset 4 is beyond EOF]", false},
		{`{"path":"lines.txt","offset":999}`, "[end of file: 3 lines; offset 999 is beyond EOF]", false},
		{`{"path":"lines.txt","offset":0}`, "offset and limit must be positive", true},
		{`{"path":"lines.txt","limit":-1}`, "offset and limit must be positive", true},
	}
	for _, test := range cases {
		t.Run(test.input, func(t *testing.T) {
			result := runTool(t, tool, test.input)
			if result.IsError != test.isError || !strings.Contains(result.Content, test.want) {
				t.Fatalf("got %+v, want %q, error %v", result, test.want, test.isError)
			}
		})
	}
	putTestFile(t, root, "long.txt", strings.Repeat("x", 2*maxReadBytes)+"\nlast")
	result := runTool(t, tool, `{"path":"long.txt"}`)
	if result.IsError || len(result.Content) > maxReadBytes || !strings.Contains(result.Content, "truncated: 256 KiB") {
		t.Fatalf("large line: length %d, error %v", len(result.Content), result.IsError)
	}
	result = runTool(t, tool, `{"path":"long.txt","offset":2}`)
	if result.Content != "2: last\n[end of file: 2 lines]" {
		t.Fatalf("skipping large line = %q", result.Content)
	}
	putTestFile(t, root, "empty.txt", "")
	if result := runTool(t, tool, `{"path":"empty.txt"}`); result.Content != "[end of file: 0 lines]" {
		t.Fatalf("empty file = %+v", result)
	}
	putTestFile(t, root, "many.txt", strings.Repeat("line\n", 2001))
	result = runTool(t, tool, `{"path":"many.txt"}`)
	if !strings.Contains(result.Content, "2000: line\n[truncated: line limit; continue with offset 2001]") {
		t.Fatal("default line limit was not applied")
	}
}

type cancelReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}

func TestBoundedLineCancelsDuringLongScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := bufio.NewReader(cancelReader{reader: strings.NewReader(strings.Repeat("x", maxReadBytes)), cancel: cancel})
	_, _, err := boundedLine(ctx, reader, 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancellation = %v", err)
	}
}

func TestTreeToolsSearchRealFiles(t *testing.T) {
	root := t.TempDir()
	for _, filename := range []string{"z.txt", "a.go", "src/b.go", "src/deep/c.go", ".git/hidden.go", "node_modules/hidden.go", "src/node_modules/hidden.go"} {
		putTestFile(t, root, filename, "needle\n")
	}
	if err := os.Symlink(filepath.Join(root, "src"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a.go"), filepath.Join(root, "linked.go")); err != nil {
		t.Fatal(err)
	}
	glob := lookup(t, root, "glob")
	result := runTool(t, glob, `{"pattern":"**/*.go"}`)
	if result.IsError || result.Content != "a.go\nsrc/b.go\nsrc/deep/c.go" {
		t.Fatalf("glob = %+v", result)
	}
	result = runTool(t, glob, `{"pattern":"src/**/c.?o"}`)
	if result.Content != "src/deep/c.go" {
		t.Fatalf("nested glob = %+v", result)
	}
	result = runTool(t, glob, `{"path":"src","pattern":"**/*.go"}`)
	if result.Content != "b.go\ndeep/c.go" {
		t.Fatalf("scoped glob = %+v", result)
	}
	result = runTool(t, glob, `{"pattern":"*.go"}`)
	if result.Content != "a.go" {
		t.Fatalf("single-level glob = %+v", result)
	}
	result = runTool(t, glob, `{"pattern":"**/*.go","limit":1}`)
	if !strings.HasPrefix(result.Content, "a.go\n[truncated:") {
		t.Fatalf("limited glob = %+v", result)
	}
	for _, input := range []string{`{"pattern":"["}`, `{"pattern":"**/["}`, `{"pattern":"../*.go"}`, `{}`, `{"pattern":"*","limit":0}`} {
		if result := runTool(t, glob, input); !result.IsError {
			t.Fatalf("invalid glob %s accepted: %+v", input, result)
		}
	}
	list := lookup(t, root, "list_dir")
	result = runTool(t, list, `{"path":"src"}`)
	if result.Content != "b.go\ndeep/\nnode_modules/" {
		t.Fatalf("list_dir = %+v", result)
	}
	result = runTool(t, list, `{"path":"src","limit":1}`)
	if !strings.HasPrefix(result.Content, "b.go\n[truncated:") {
		t.Fatalf("limited list = %+v", result)
	}
	if result := runTool(t, list, `{"limit":0}`); !result.IsError {
		t.Fatal("list_dir accepted a nonpositive limit")
	}
	grep := lookup(t, root, "grep")
	result = runTool(t, grep, `{"pattern":"needle","include":"*.go"}`)
	if result.IsError || result.Content != "a.go:1:needle\nsrc/b.go:1:needle\nsrc/deep/c.go:1:needle" {
		t.Fatalf("grep tree = %+v", result)
	}
}

func TestGrepValidationAndSkippedContent(t *testing.T) {
	root := t.TempDir()
	putTestFile(t, root, "a.txt", "other\nNeedle\nneedle\n")
	putTestFile(t, root, "binary.txt", "needle\n"+strings.Repeat("a", 8192)+"\x00")
	putTestFile(t, root, "large.txt", strings.Repeat("x", maxGrepFileBytes+1))
	putTestFile(t, root, "long.txt", strings.Repeat("x", maxGrepLineBytes+1)+"needle\nneedle\n")
	grep := lookup(t, root, "grep")
	result := runTool(t, grep, `{"pattern":"needle"}`)
	want := "a.txt:3:needle\nlong.txt:2:needle\n[skipped: 1 files over 2 MiB, 1 binary files (NUL), 1 lines over 16 KiB]"
	if result.IsError || result.Content != want {
		t.Fatalf("grep = %+v, want %q", result, want)
	}
	result = runTool(t, grep, `{"pattern":"needle","case_sensitive":false,"path":"a.txt"}`)
	if result.Content != "a.txt:2:Needle\na.txt:3:needle" {
		t.Fatalf("case-insensitive grep = %+v", result)
	}
	result = runTool(t, grep, `{"pattern":"needle","limit":1,"path":"a.txt","case_sensitive":false}`)
	if !strings.HasPrefix(result.Content, "a.txt:2:Needle\n[truncated:") {
		t.Fatalf("limited grep = %+v", result)
	}
	for _, input := range []string{`{"pattern":"["}`, `{}`, `{"pattern":"x","include":"["}`, `{"pattern":"x","limit":0}`} {
		if result := runTool(t, grep, input); !result.IsError {
			t.Fatalf("invalid grep %s accepted", input)
		}
	}
}

func TestMissingReplacementFieldsNeverMutateFiles(t *testing.T) {
	root := t.TempDir()
	putTestFile(t, root, "existing.txt", "keep me")
	write := lookup(t, root, "write_file")
	edit := lookup(t, root, "edit_file")
	for _, test := range []struct {
		tool  Tool
		input string
	}{
		{write, `{"path":"existing.txt"}`},
		{write, `{"content":"replace"}`},
		{write, `{"path":null,"content":"replace"}`},
		{write, `{"path":"existing.txt","content":null}`},
		{write, `{"path":"missing/created.txt"}`},
		{edit, `{"path":"existing.txt","old_string":"keep me"}`},
		{edit, `{"path":"existing.txt","old_string":"keep me","new_string":null}`},
		{edit, `{"old_string":"keep me","new_string":"replace"}`},
		{edit, `{"path":null,"old_string":"keep me","new_string":"replace"}`},
		{edit, `{"path":"existing.txt","new_string":"replace"}`},
	} {
		if result := runTool(t, test.tool, test.input); !result.IsError {
			t.Fatalf("omission accepted: %s", test.input)
		}
		content, err := os.ReadFile(filepath.Join(root, "existing.txt"))
		if err != nil || string(content) != "keep me" {
			t.Fatalf("rejected call changed file: %q, %v", content, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected write created parent directory: %v", err)
	}
	if result := runTool(t, edit, `{"path":"existing.txt","old_string":"keep me","new_string":""}`); result.IsError {
		t.Fatalf("explicit empty replacement rejected: %+v", result)
	}
	if result := runTool(t, write, `{"path":"empty.txt","content":""}`); result.IsError {
		t.Fatalf("explicit empty content rejected: %+v", result)
	}
	for _, name := range []string{"existing.txt", "empty.txt"} {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || len(content) != 0 {
			t.Fatalf("explicit empty %s = %q, %v", name, content, err)
		}
	}
}

func TestTodoWriteValidationReplacementAndConcurrency(t *testing.T) {
	tool := lookup(t, t.TempDir(), "todo_write")
	if tool.ReadOnly {
		t.Fatal("todo_write must not be ReadOnly")
	}
	result := runTool(t, tool, `{"items":[{"content":"first","status":"in_progress"},{"content":"second","status":"pending"}]}`)
	if result.IsError || !strings.Contains(result.Content, "1. [in_progress] first\n2. [pending] second") {
		t.Fatalf("todo_write = %+v", result)
	}
	for _, input := range []string{`{}`, `{"items":null}`, `{"items":[{"content":"x","status":"bad"}]}`, `{"items":[{"status":"pending"}]}`, `{"items":[{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"}]}`} {
		if result := runTool(t, tool, input); !result.IsError {
			t.Fatalf("invalid todo accepted: %s", input)
		}
	}
	result = runTool(t, tool, `{"items":[{"content":"third","status":"completed"}]}`)
	if result.IsError || strings.Contains(result.Content, "first") || !strings.Contains(result.Content, "1. [completed] third") {
		t.Fatalf("replacement = %+v", result)
	}
	result = runTool(t, tool, `{"items":[]}`)
	if result.IsError || !strings.Contains(result.Content, "task list replaced: 0 items") {
		t.Fatalf("cleared list = %+v", result)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := tool.Run(context.Background(), json.RawMessage(`{"items":[{"content":"task","status":"pending"}]}`))
			if err != nil || result.IsError {
				t.Errorf("concurrent todo = %+v, %v", result, err)
			}
		}()
	}
	group.Wait()
}

func TestReadOnlyToolsHonorCancellation(t *testing.T) {
	root := t.TempDir()
	putTestFile(t, root, "f.txt", "text")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name  string
		input string
	}{
		{"read_file", `{"path":"f.txt"}`},
		{"list_dir", `{}`},
		{"glob", `{"pattern":"**"}`},
		{"grep", `{"pattern":"text"}`},
	} {
		tool := lookup(t, root, test.name)
		if !tool.ReadOnly {
			t.Fatalf("%s must be ReadOnly", test.name)
		}
		if _, err := tool.Run(ctx, json.RawMessage(test.input)); !errors.Is(err, context.Canceled) {
			t.Errorf("%s cancellation = %v", test.name, err)
		}
	}
}
