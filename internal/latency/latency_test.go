package latency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDisabledTimingDoesNotAllocate(t *testing.T) {
	ctx := context.Background()
	if allocations := testing.AllocsPerRun(100, func() {
		Begin(ctx, Preparation, "prepare").End(nil)
		requestCtx, span := StartRequest(RequestContext(ctx))
		_, attempt := StartAttempt(requestCtx, 1)
		attempt.End(nil, nil)
		span.End(nil)
		meter := NewMeter(ctx, StreamRead, "stream")
		meter.Add(meter.Start(), nil)
		meter.End()
	}); allocations != 0 {
		t.Fatalf("disabled timing allocated %g objects", allocations)
	}
}

func TestConcurrentRequestsAndPrivateAppend(t *testing.T) {
	r := New("thinkingmachines/inkling-small", "default")
	ctx := WithRecorder(context.Background(), r)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			scope := RequestContext(ctx)
			requestCtx, span := StartRequest(scope)
			Begin(requestCtx, Encoding, "messages").End(nil)
			Begin(scope, Tool, "read_file").End(nil)
			zero := 0
			span.Usage(Tokens{CacheRead: &zero})
			span.Mark(FirstUseful)
			span.End(nil)
		}()
	}
	workers.Wait()
	r.Finish(nil)
	path := filepath.Join(t.TempDir(), "timings.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := r.AppendFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private file: %v, %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recorded record
	if err := validate(bytes.TrimSpace(data), &recorded); err != nil {
		t.Fatal(err)
	}
	ids := make(map[uint64]bool)
	for _, e := range recorded.Entries {
		if e.Stage == Request {
			if e.Request == 0 || ids[e.Request] {
				t.Fatalf("reused request ID %d", e.Request)
			}
			ids[e.Request] = true
			if e.Usage.CacheRead == nil || *e.Usage.CacheRead != 0 || e.Usage.CacheCreation != nil {
				t.Fatalf("cache presence lost: %+v", e.Usage)
			}
		}
	}
	if len(ids) != 8 {
		t.Fatalf("got %d request IDs", len(ids))
	}
	for _, entry := range recorded.Entries {
		if entry.Stage == Tool && !ids[entry.Request] {
			t.Fatalf("tool has no matching model request: %d", entry.Request)
		}
	}
	var summary bytes.Buffer
	if err := Summarize(bytes.NewReader(data), &summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.String(), "hits=0 reported-zero=8 unreported=0") {
		t.Fatal(summary.String())
	}
}

func TestFailuresBoundsAndAppendErrors(t *testing.T) {
	r := New("m", "default")
	ctx := WithRecorder(context.Background(), r)
	for range maxEntries + 3 {
		Begin(ctx, Preparation, "secret/path with spaces").End(errors.New("secret error"))
	}
	r.Finish(context.Canceled)
	if r.record.State != "canceled" || r.record.Dropped != 3 {
		t.Fatalf("record state=%s dropped=%d", r.record.State, r.record.Dropped)
	}
	data, err := json.Marshal(r.record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("secret")) {
		t.Fatal("content leaked into timing metadata")
	}
	if err := r.AppendFile(t.TempDir()); err == nil {
		t.Fatal("directory append succeeded")
	}
	if err := New("m", "default").AppendFile(filepath.Join(t.TempDir(), "unfinished")); err == nil {
		t.Fatal("unfinished record appended")
	}
}

func TestAppendRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "original")
	content := []byte("keep this file unchanged\n")
	if err := os.WriteFile(target, content, 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "timings.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	recorder := New("m", "default")
	recorder.Finish(nil)
	if err := recorder.AppendFile(link); err == nil {
		t.Fatal("timings followed a symlink")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, content) || after.Mode() != before.Mode() {
		t.Fatal("timings changed the linked file")
	}
}

func TestSummaryRejectsMalformedAndSeparatesFailures(t *testing.T) {
	good := `{"version":1,"model":"m","effort":"default","state":"success","ns":10,"dropped":0,"entries":[]}`
	for _, bad := range []string{"", "null", good + " {}", strings.Replace(good, `"version":1`, `"version":2`, 1), strings.Replace(good, `"entries":[]`, `"entries":null`, 1), strings.Replace(good, `"ns":10`, `"ns":-1`, 1), strings.Replace(good, `"dropped":0,`, `"secret":"x",`, 1)} {
		var output bytes.Buffer
		if err := Summarize(strings.NewReader(bad), &output); err == nil {
			t.Fatalf("accepted malformed record %q", bad)
		}
		if output.Len() != 0 {
			t.Fatal("wrote partial summary before validation")
		}
	}
	failed := strings.Replace(strings.Replace(good, `"success"`, `"error"`, 1), `"ns":10`, `"ns":9000000000`, 1)
	var output bytes.Buffer
	if err := Summarize(strings.NewReader(good+"\n"+failed+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"failures: 1", "10ns", "9s"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("missing %q: %s", text, output.String())
		}
	}
}
