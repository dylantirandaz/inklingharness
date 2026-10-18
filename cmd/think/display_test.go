package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

func TestToolRoutesSpanRequestsAndResetForNewPrompt(t *testing.T) {
	var output bytes.Buffer
	observer := newConsoleObserver(&output, &output, nil, true, presentation.Theme{}, false, false)
	result := tools.Result{Content: strings.Repeat("output line\n", 8)}
	observer.Begin()
	for range 3 {
		observer.ToolCall("bash", json.RawMessage(`{"command":"pwd"}`))
		observer.ToolResult("bash", result, time.Millisecond)
	}
	for _, route := range []string{"/tools 1", "/tools 2", "/tools 3"} {
		if strings.Count(output.String(), route) != 1 {
			t.Fatalf("tool route %q must occur once: %s", route, &output)
		}
	}
	observer.Begin()
	output.Reset()
	observer.ToolCall("bash", json.RawMessage(`{"command":"pwd"}`))
	observer.ToolResult("bash", result, time.Millisecond)
	if !strings.Contains(output.String(), "/tools 1") || strings.Contains(output.String(), "/tools 4") {
		t.Fatalf("new prompt did not reset the tool route: %s", &output)
	}
}
