package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/latency"
)

func TestTimingPreservesUsageAndTracesActualReuse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamFixture)
	}))
	defer server.Close()
	recorder := latency.New("m", "default")
	ctx := latency.WithRecorder(context.Background(), recorder)
	client := NewClient("secret-key", server.URL, nil)
	for range 2 {
		response, err := client.Stream(ctx, Request{Model: "m", System: "secret-prompt"}, func(StreamEvent) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if response.Usage.CacheReadInputTokens != 5 || response.Usage.CacheCreationInputTokens != 3 {
			t.Fatalf("usage changed: %+v", response.Usage)
		}
	}
	recorder.Finish(nil)
	path := filepath.Join(t.TempDir(), "timings.jsonl")
	if err := recorder.AppendFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-key", "secret-prompt", server.URL, "Let me read", "read_file"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("timing leaked %q", secret)
		}
	}
	var record struct {
		Entries []struct {
			Stage    latency.Stage   `json:"stage"`
			Request  uint64          `json:"request"`
			Count    int64           `json:"count"`
			Protocol string          `json:"protocol"`
			Reused   *bool           `json:"reused"`
			Usage    *latency.Tokens `json:"usage"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint64]map[latency.Stage]bool)
	reused := 0
	for _, entry := range record.Entries {
		if seen[entry.Request] == nil {
			seen[entry.Request] = make(map[latency.Stage]bool)
		}
		seen[entry.Request][entry.Stage] = true
		if entry.Stage == latency.HTTP {
			if entry.Protocol != "HTTP/1.1" || entry.Reused == nil {
				t.Fatalf("missing actual transport metadata: %+v", entry)
			}
			if *entry.Reused {
				reused++
			}
		}
		if entry.Stage == latency.Request && (entry.Usage == nil || entry.Usage.CacheRead == nil || *entry.Usage.CacheRead != 5) {
			t.Fatalf("missing cache usage: %+v", entry)
		}
		if entry.Stage == latency.Observer && entry.Count < 2 {
			t.Fatalf("observer callbacks not aggregated: %+v", entry)
		}
	}
	if len(seen) != 2 || reused != 1 {
		t.Fatalf("request groups=%d reused=%d", len(seen), reused)
	}
	for request, stages := range seen {
		for _, stage := range []latency.Stage{latency.Request, latency.Encoding, latency.HTTP, latency.RequestWrite, latency.FirstByte, latency.FirstUseful, latency.StreamRead, latency.StreamDecode, latency.Observer} {
			if !stages[stage] {
				t.Fatalf("request %d missing %s", request, stage)
			}
		}
	}
	var report bytes.Buffer
	if err := latency.Summarize(bytes.NewReader(data), &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "hits=2 reported-zero=0 unreported=0 tokens=10") {
		t.Fatal(report.String())
	}
}
