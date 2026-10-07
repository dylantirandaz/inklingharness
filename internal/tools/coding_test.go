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
	lines := []string{"first value", "second value", "third value"}
	putTestFile(t, root, "lines.txt", strings.Join(lines, "\n")+"\n")
	tool := lookup(t, root, "read_file")
	for _, test := range []struct {
		input string
		start int
		end   int
	}{
		{`{"path":"lines.txt"}`, 0, 3},
		{`{"path":"lines.txt","offset":2,"limit":1}`, 1, 2},
		{`{"path":"lines.txt","offset":3,"limit":1}`, 2, 3},
		{`{"path":"lines.txt","offset":4}`, 3, 3},
		{`{"path":"lines.txt","offset":999}`, 3, 3},
	} {
		result := runTool(t, tool, test.input)
		if result.IsError {
			t.Fatalf("%s: %+v", test.input, result)
		}
		for index, line := range lines {
			present := strings.Contains(result.Content, line)
			if present != (index >= test.start && index < test.end) {
				t.Fatalf("%s: line %d presence = %t", test.input, index+1, present)
			}
		}
	}
	for _, input := range []string{`{"path":"lines.txt","offset":0}`, `{"path":"lines.txt","limit":-1}`} {
		if result := runTool(t, tool, input); !result.IsError {
			t.Fatalf("accepted an invalid range: %s", input)
		}
	}
	putTestFile(t, root, "long.txt", strings.Repeat("x", 2*maxReadBytes)+"\nlast")
	result := runTool(t, tool, `{"path":"long.txt"}`)
	if result.IsError || len(result.Content) > maxReadBytes || strings.Contains(result.Content, "last") {
		t.Fatalf("large line: length %d, error %v", len(result.Content), result.IsError)
	}
	result = runTool(t, tool, `{"path":"long.txt","offset":2}`)
	if result.IsError || !strings.Contains(result.Content, "last") || strings.Contains(result.Content, "xxx") {
		t.Fatalf("skipping a large line = %q", result.Content)
	}
}

func TestReadFilePreservesLinesAcrossReaderBuffers(t *testing.T) {
	root := t.TempDir()
	lines := []string{strings.Repeat("a", 4095), strings.Repeat("界", 3000), "", "last"}
	putTestFile(t, root, "lines.txt", strings.Join(lines, "\r\n"))
	result := runTool(t, lookup(t, root, "read_file"), `{"path":"lines.txt"}`)
	if result.IsError || !strings.Contains(result.Content, strings.Join(lines, "\n")+"\n") {
		t.Fatal("file text changed at a reader boundary")
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
