package extend

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func remember(t *testing.T, workDir, fact string) (string, bool) {
	t.Helper()
	input, err := json.Marshal(map[string]string{"fact": fact})
	if err != nil {
		t.Fatal(err)
	}
	result, err := MemoryTool(workDir).Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return result.Content, result.IsError
}

func TestMemoryRoundTrip(t *testing.T) {
	workDir := t.TempDir()
	if text, err := LoadMemory(workDir); err != nil || text != "" {
		t.Fatalf("no memory = %q, %v", text, err)
	}
	if _, failed := remember(t, workDir, "  Run tests with go test -race ./...  "); failed {
		t.Fatal("first fact failed")
	}
	// A file that a person edited without a final new line still gets one
	// fact on each line.
	file, err := os.OpenFile(MemoryPath(workDir), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("- written by hand")
	file.Close()
	if _, failed := remember(t, workDir, "The API key lives in OPENROUTER_API_KEY."); failed {
		t.Fatal("second fact failed")
	}
	data, err := os.ReadFile(MemoryPath(workDir))
	if err != nil {
		t.Fatal(err)
	}
	if want := "- Run tests with go test -race ./...\n- written by hand\n- The API key lives in OPENROUTER_API_KEY.\n"; string(data) != want {
		t.Fatalf("file = %q", data)
	}
	text, err := LoadMemory(workDir)
	if err != nil || !strings.Contains(text, "check a fact before you depend on it") || !strings.HasSuffix(text, "- The API key lives in OPENROUTER_API_KEY.") {
		t.Fatalf("loaded = %q, %v", text, err)
	}
}

func TestMemoryRejectsBadFacts(t *testing.T) {
	workDir := t.TempDir()
	for _, fact := range []string{"", "   ", "two\nlines", "carriage\rreturn", strings.Repeat("x", memoryFactLimit+1)} {
		if message, failed := remember(t, workDir, fact); !failed || !strings.HasPrefix(message, "invalid input") {
			t.Fatalf("fact %q accepted: %s", fact, message)
		}
	}
	if _, failed := remember(t, workDir, strings.Repeat("é", memoryFactLimit)); failed {
		t.Fatal("a fact of exactly the limit in characters was rejected")
	}
	result, err := MemoryTool(workDir).Run(context.Background(), json.RawMessage(`{"fact":3}`))
	if err != nil || !result.IsError {
		t.Fatalf("a number fact = %+v, %v", result, err)
	}
}

// A long memory keeps its newest facts and starts at a whole line.
func TestMemoryLoadKeepsTheEnd(t *testing.T) {
	workDir := t.TempDir()
	var file strings.Builder
	for index := range 2000 {
		file.WriteString("- fact number " + strings.Repeat("é", 3) + " " + string(rune('a'+index%26)) + "\n")
	}
	file.WriteString("- newest fact\n")
	if err := os.MkdirAll(workDir+"/.inkling", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MemoryPath(workDir), []byte(file.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := LoadMemory(workDir)
	if err != nil {
		t.Fatal(err)
	}
	body := text[strings.Index(text, "(Older facts are omitted"):]
	lines := strings.Split(body, "\n")
	if !strings.HasSuffix(text, "- newest fact") || !strings.HasPrefix(lines[1], "- fact number") || len(text) > memoryLoadLimit+300 {
		t.Fatalf("loaded %d bytes, first line %q", len(text), lines[1])
	}
}
