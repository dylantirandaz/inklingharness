package session

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

func newTestSession(t *testing.T, workDir string) Session {
	t.Helper()
	value, err := New(workDir, "thinkingmachines/inkling:free", "high")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func encode(t *testing.T, message anthropic.Message) anthropic.EncodedMessage {
	t.Helper()
	encoded, err := anthropic.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func text(t *testing.T, value string) anthropic.EncodedMessage {
	t.Helper()
	return encode(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock{Text: value}}})
}

func texts(t *testing.T, prefix string, count, size int) []anthropic.EncodedMessage {
	t.Helper()
	result := make([]anthropic.EncodedMessage, count)
	for index := range result {
		result[index] = text(t, fmt.Sprintf("%s %d %s", prefix, index, strings.Repeat("x", size)))
	}
	return result
}

func sameHistory(left, right []anthropic.EncodedMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !left[index].Equal(right[index]) {
			return false
		}
	}
	return true
}

func saveMessages(t *testing.T, store *Store, value Session, messages []anthropic.EncodedMessage) {
	t.Helper()
	value.Messages = messages
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
}

// loadFresh reads a session through a Store that has no state from earlier
// saves, as a new process does.
func loadFresh(t *testing.T, root, id string) Session {
	t.Helper()
	loaded, err := NewStore(root).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func logSize(t *testing.T, root, id string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, id+".log"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func readLog(t *testing.T, root, id string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, id+".log"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var testCastagnoli = crc32.MakeTable(crc32.Castagnoli)

// requireCompleteRecords fails unless the log holds only complete records with
// valid checksums.
func requireCompleteRecords(t *testing.T, root, id string) {
	t.Helper()
	data := readLog(t, root, id)
	for offset := 0; offset < len(data); {
		if len(data)-offset < 8 {
			t.Fatalf("log has %d stray bytes at offset %d", len(data)-offset, offset)
		}
		end := offset + 8 + int(binary.LittleEndian.Uint32(data[offset:]))
		if end > len(data) || crc32.Checksum(data[offset+8:end], testCastagnoli) != binary.LittleEndian.Uint32(data[offset+4:]) {
			t.Fatalf("log has a damaged record at offset %d", offset)
		}
		offset = end
	}
}

// writeLegacy writes value in the single JSON file format of earlier releases.
func writeLegacy(t *testing.T, root string, value Session) {
	t.Helper()
	var messages []anthropic.Message
	for _, message := range value.Messages {
		decoded, err := message.Decode()
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, decoded)
	}
	legacy := struct {
		ID        string              `json:"id"`
		CreatedAt time.Time           `json:"created_at"`
		UpdatedAt time.Time           `json:"updated_at"`
		WorkDir   string              `json:"work_dir"`
		Model     string              `json:"model"`
		Effort    string              `json:"effort"`
		Messages  []anthropic.Message `json:"messages"`
		Usage     anthropic.Usage     `json:"usage"`
	}{value.ID, value.CreatedAt, value.UpdatedAt, value.WorkDir, value.Model, value.Effort, messages, value.Usage}
	var data bytes.Buffer
	if err := json.NewEncoder(&data).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, value.ID+".json"), data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func allBlockKinds(t *testing.T) []anthropic.EncodedMessage {
	t.Helper()
	return []anthropic.EncodedMessage{
		encode(t, anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{
			anthropic.TextBlock{Text: "answer <b> & \"quoted\" ünïcode"},
			anthropic.ThinkingBlock{Thinking: "private reasoning", Signature: "signature"},
			anthropic.RedactedThinkingBlock{Data: "encrypted"},
			anthropic.ToolUseBlock{ID: "call", Name: "read", Input: json.RawMessage(`{"path":"x"}`)},
			anthropic.OpaqueBlock{Raw: json.RawMessage(`{"type":"future","nested":{"value":[1,"two",true,null]}}`)},
		}}),
		encode(t, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{
			anthropic.ToolResultBlock{ToolUseID: "call", Content: "missing", IsError: true},
		}}),
	}
}

func TestNewAndDefaultStore(t *testing.T) {
	value := newTestSession(t, ".")
	if !validID(value.ID) || !filepath.IsAbs(value.WorkDir) {
		t.Fatalf("invalid new session: %+v", value)
	}
	if value.CreatedAt.Location() != time.UTC || !value.CreatedAt.Equal(value.UpdatedAt) {
		t.Fatalf("timestamps are not equal UTC values: %+v", value)
	}
	if another := newTestSession(t, "."); another.ID == value.ID {
		t.Fatal("random session IDs collided")
	}
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	store, err := DefaultStore()
	if err != nil || store.root != filepath.Join(root, "inkling", "sessions") {
		t.Fatalf("XDG store = %+v, %v", store, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", root)
	store, err = DefaultStore()
	if err != nil || store.root != filepath.Join(root, ".local", "state", "inkling", "sessions") {
		t.Fatalf("home store = %+v, %v", store, err)
	}
}

func TestRoundTripAllContentAndPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	value := newTestSession(t, t.TempDir())
	value.Messages = allBlockKinds(t)
	value.Usage = anthropic.Usage{InputTokens: 12, OutputTokens: 34, CacheCreationInputTokens: 56, CacheReadInputTokens: 78}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	loaded := loadFresh(t, root, value.ID)
	if loaded.UpdatedAt.Before(value.UpdatedAt) || loaded.UpdatedAt.Location() != time.UTC {
		t.Fatalf("invalid persisted update time: %v", loaded.UpdatedAt)
	}
	value.UpdatedAt = loaded.UpdatedAt
	if !reflect.DeepEqual(value, loaded) {
		t.Fatalf("round trip mismatch:\nwant %#v\ngot  %#v", value, loaded)
	}
	for path, mode := range map[string]fs.FileMode{
		root:                                 0700,
		filepath.Join(root, value.ID+".log"): 0600,
		filepath.Join(root, value.ID+".meta.json"): 0600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("permissions for %s: got %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
}

func TestInvalidIDsAndSymlinks(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, id := range []string{"", ".", "..", "../escape", "/absolute", strings.Repeat("a", 31), strings.Repeat("A", 32), strings.Repeat("g", 32), strings.Repeat("a", 32) + "/../escape"} {
		if _, err := store.Load(id); err == nil {
			t.Errorf("Load accepted invalid ID %q", id)
		}
		if err := store.Save(Session{ID: id}); err == nil {
			t.Errorf("Save accepted invalid ID %q", id)
		}
		if _, err := store.OutputDirectory(id); err == nil {
			t.Errorf("OutputDirectory accepted invalid ID %q", id)
		}
	}
	value := newTestSession(t, t.TempDir())
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{value.ID + ".log", value.ID + ".json"} {
		path := filepath.Join(store.root, name)
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore(store.root).Load(value.ID); err == nil {
			t.Fatalf("Load followed the session symlink %s", name)
		}
		if err := store.Save(value); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "unchanged" {
			t.Fatalf("Save followed the session symlink %s: %q, %v", name, data, err)
		}
		if err := os.Remove(filepath.Join(store.root, value.ID+".log")); err != nil {
			t.Fatal(err)
		}
	}
	index := filepath.Join(store.root, value.ID+".meta.json")
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, index); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(""); err == nil {
		t.Fatal("List followed a session index symlink")
	}
}

func TestOutputDirectoryIsAbsoluteAndNotCreated(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	store := NewStore("sessions")
	value := newTestSession(t, work)
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	directory, err := store.OutputDirectory(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The working directory can be a symlinked temporary path.
	resolvedWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	resolvedDirectory, err := filepath.EvalSymlinks(filepath.Dir(directory))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(directory) || filepath.Base(directory) != value.ID+".outputs" || resolvedDirectory != filepath.Join(resolvedWork, "sessions") {
		t.Fatalf("output directory = %q", directory)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("output directory exists before any output: %v", err)
	}
}

func TestListLatestAndWorkdirFiltering(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "not-created"))
	if _, err := store.Latest(""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty Latest error = %v", err)
	}
	if all, err := store.List(""); err != nil || len(all) != 0 {
		t.Fatalf("empty List = %v, %v", all, err)
	}
	work := t.TempDir()
	first := newTestSession(t, work)
	second := newTestSession(t, t.TempDir())
	third := newTestSession(t, work)
	for _, value := range []Session{first, second, third} {
		if err := store.Save(value); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.List("")
	if err != nil || len(all) != 3 {
		t.Fatalf("List all = %v, %v", all, err)
	}
	for index := 1; index < len(all); index++ {
		if all[index].UpdatedAt.After(all[index-1].UpdatedAt) {
			t.Fatal("List is not newest-first")
		}
	}
	filtered, err := store.List(filepath.Join(work, "."))
	if err != nil || len(filtered) != 2 {
		t.Fatalf("filtered List = %v, %v", filtered, err)
	}
	for _, summary := range filtered {
		if summary.WorkDir != work || summary.ID == second.ID || summary.Model != first.Model || summary.MessageCount != 0 {
			t.Fatalf("incorrect summary: %+v", summary)
		}
	}
	latest, err := store.Latest(work)
	if err != nil || latest.ID != filtered[0].ID {
		t.Fatalf("Latest = %+v, %v", latest, err)
	}
	if _, err := store.Latest(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unmatched Latest error = %v", err)
	}
	if _, err := store.Load(newTestSession(t, work).ID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing Load error = %v", err)
	}
}

func TestLatestTieKeepsCompleteHistoryAndReportsCorruption(t *testing.T) {
	store := NewStore(t.TempDir())
	work := t.TempDir()
	first := newTestSession(t, work)
	second := newTestSession(t, work)
	first.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	second.UpdatedAt = first.UpdatedAt
	first.Messages = []anthropic.EncodedMessage{text(t, "first history")}
	second.Messages = []anthropic.EncodedMessage{text(t, "second history")}
	for _, value := range []Session{first, second} {
		writeLegacy(t, store.root, value)
	}
	expected := first
	if second.ID < first.ID {
		expected = second
	}
	latest, err := store.Latest(work)
	if err != nil || !reflect.DeepEqual(latest, expected) {
		t.Fatalf("timestamp tie selected the wrong conversation: %+v, %v", latest, err)
	}
	for _, name := range []string{strings.Repeat("f", 32) + ".json", strings.Repeat("e", 32) + ".meta.json"} {
		path := filepath.Join(store.root, name)
		if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Latest(work); err == nil {
			t.Fatalf("Latest ignored the corrupt file %s", name)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSaveAppendsOnlyTheChange(t *testing.T) {
	store := NewStore(t.TempDir())
	value := newTestSession(t, t.TempDir())
	history := texts(t, "turn", 20, 4096)
	saveMessages(t, store, value, history)
	before := readLog(t, store.root, value.ID)
	next := text(t, "next turn")
	history = append(history, next)
	saveMessages(t, store, value, history)
	after := readLog(t, store.root, value.ID)
	if growth := len(after) - len(before); !bytes.HasPrefix(after, before) || growth <= len(next.Wire()) || growth > len(next.Wire())+1024 {
		t.Fatalf("one new message rewrote the log or grew it by %d bytes", growth)
	}
	value.Usage = anthropic.Usage{InputTokens: 7, OutputTokens: 9}
	saveMessages(t, store, value, history)
	final := readLog(t, store.root, value.ID)
	if growth := len(final) - len(after); !bytes.HasPrefix(final, after) || growth > 1024 {
		t.Fatalf("a metadata change rewrote the log or grew it by %d bytes", growth)
	}
	loaded := loadFresh(t, store.root, value.ID)
	if !sameHistory(loaded.Messages, history) || loaded.Usage != value.Usage {
		t.Fatalf("appended records did not load: %d messages, usage %+v", len(loaded.Messages), loaded.Usage)
	}
}

func TestCompactionReplacesHistoryPrefix(t *testing.T) {
	store := NewStore(t.TempDir())
	value := newTestSession(t, t.TempDir())
	long := texts(t, "old", 40, 8192)
	saveMessages(t, store, value, long)
	compacted := []anthropic.EncodedMessage{text(t, "summary"), long[39]}
	saveMessages(t, store, value, compacted)
	if size, limit := logSize(t, store.root, value.ID), 2*wireBytes(compacted)+rewriteSlack; size > limit {
		t.Fatalf("log keeps replaced history: %d bytes, limit %d", size, limit)
	}
	if loaded := loadFresh(t, store.root, value.ID); !sameHistory(loaded.Messages, compacted) {
		t.Fatalf("compacted history = %d messages", len(loaded.Messages))
	}
	steps := [][]anthropic.EncodedMessage{
		{compacted[0], compacted[1], text(t, "a"), text(t, "b")},
		{compacted[0], compacted[1], text(t, "replacement")},
		{text(t, "second summary")},
		{text(t, "second summary"), text(t, "after")},
	}
	for index, step := range steps {
		saveMessages(t, store, value, step)
		if loaded := loadFresh(t, store.root, value.ID); !sameHistory(loaded.Messages, step) {
			t.Fatalf("step %d loaded %d messages, want %d", index, len(loaded.Messages), len(step))
		}
	}
}

func TestOneStoreSavesSessionsInTurn(t *testing.T) {
	store := NewStore(t.TempDir())
	first := newTestSession(t, t.TempDir())
	second := newTestSession(t, t.TempDir())
	a, b := texts(t, "first", 3, 10), texts(t, "second", 3, 10)
	for count := 1; count <= 3; count++ {
		saveMessages(t, store, first, a[:count])
		saveMessages(t, store, second, b[:count])
	}
	if loaded := loadFresh(t, store.root, first.ID); !sameHistory(loaded.Messages, a) {
		t.Fatalf("first session = %d messages", len(loaded.Messages))
	}
	if loaded := loadFresh(t, store.root, second.ID); !sameHistory(loaded.Messages, b) {
		t.Fatalf("second session = %d messages", len(loaded.Messages))
	}
}

func TestLastSaveWinsAcrossStores(t *testing.T) {
	for name, otherLoads := range map[string]bool{"other store rewrites": false, "other store appends": true} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			first, other := NewStore(root), NewStore(root)
			value := newTestSession(t, t.TempDir())
			history := texts(t, "turn", 2, 10)
			saveMessages(t, first, value, history)
			if otherLoads {
				if _, err := other.Load(value.ID); err != nil {
					t.Fatal(err)
				}
			}
			saveMessages(t, other, value, append(history[:2:2], texts(t, "other", 3, 1000)...))
			history = append(history, text(t, "last"))
			saveMessages(t, first, value, history)
			if loaded := loadFresh(t, root, value.ID); !sameHistory(loaded.Messages, history) {
				t.Fatalf("last save did not win: %d messages", len(loaded.Messages))
			}
			requireCompleteRecords(t, root, value.ID)
		})
	}
}

func TestTornFinalRecordIsIgnored(t *testing.T) {
	damages := map[string]func(data []byte, complete int) []byte{
		"partial header":  func(data []byte, complete int) []byte { return data[:complete+3] },
		"partial payload": func(data []byte, complete int) []byte { return data[:complete+(len(data)-complete)/2] },
		"bad checksum at end": func(data []byte, complete int) []byte {
			damaged := bytes.Clone(data)
			damaged[len(damaged)-1] ^= 0xff
			return damaged
		},
	}
	for name, damage := range damages {
		t.Run(name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			value := newTestSession(t, t.TempDir())
			history := texts(t, "turn", 3, 1000)
			saveMessages(t, store, value, history[:2])
			complete := logSize(t, store.root, value.ID)
			saveMessages(t, store, value, history)
			path := filepath.Join(store.root, value.ID+".log")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, damage(data, int(complete)), 0600); err != nil {
				t.Fatal(err)
			}
			resumed := NewStore(store.root)
			loaded, err := resumed.Load(value.ID)
			if err != nil || !sameHistory(loaded.Messages, history[:2]) {
				t.Fatalf("torn record changed the history: %d messages, %v", len(loaded.Messages), err)
			}
			next := append(loaded.Messages, text(t, "after crash"))
			saveMessages(t, resumed, loaded, next)
			if reloaded := loadFresh(t, store.root, value.ID); !sameHistory(reloaded.Messages, next) {
				t.Fatalf("save after a torn record = %d messages", len(reloaded.Messages))
			}
			requireCompleteRecords(t, store.root, value.ID)
		})
	}
}

func TestZeroFilledTailIsIgnored(t *testing.T) {
	store := NewStore(t.TempDir())
	value := newTestSession(t, t.TempDir())
	history := texts(t, "turn", 3, 100)
	saveMessages(t, store, value, history[:1])
	saveMessages(t, store, value, history[:2])
	path := filepath.Join(store.root, value.ID+".log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(make([]byte, 5000)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	resumed := NewStore(store.root)
	loaded, err := resumed.Load(value.ID)
	if err != nil || !sameHistory(loaded.Messages, history[:2]) {
		t.Fatalf("zero-filled tail changed the history: %d messages, %v", len(loaded.Messages), err)
	}
	saveMessages(t, resumed, loaded, history)
	if reloaded := loadFresh(t, store.root, value.ID); !sameHistory(reloaded.Messages, history) {
		t.Fatalf("save after a zero-filled tail = %d messages", len(reloaded.Messages))
	}
	requireCompleteRecords(t, store.root, value.ID)
}

func TestCorruptRecordIsAnError(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	value := newTestSession(t, t.TempDir())
	history := texts(t, "turn", 3, 100)
	saveMessages(t, store, value, history[:1])
	firstEnd := int(logSize(t, root, value.ID))
	saveMessages(t, store, value, history[:2])
	saveMessages(t, store, value, history)
	path := filepath.Join(root, value.ID+".log")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(offset int) []byte {
		damaged := bytes.Clone(original)
		damaged[offset] ^= 0xff
		return damaged
	}
	invalidJSON := []byte("{")
	frame := binary.LittleEndian.AppendUint32(nil, uint32(len(invalidJSON)))
	frame = binary.LittleEndian.AppendUint32(frame, crc32.Checksum(invalidJSON, testCastagnoli))
	cases := map[string][]byte{
		"first record checksum":       flip(recordHeaderSize + 2),
		"middle record checksum":      flip(firstEnd + recordHeaderSize + 2),
		"valid checksum, bad JSON":    append(bytes.Clone(original), append(frame, invalidJSON...)...),
		"empty log":                   {},
		"only a partial first record": original[:firstEnd-1],
		"zero bytes before data":      append(append(bytes.Clone(original), make([]byte, 16)...), 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(root).Load(value.ID); err == nil {
				t.Fatal("Load accepted a corrupt log")
			}
		})
	}
}

func TestFailedSaveKeepsPreviousSession(t *testing.T) {
	store := NewStore(t.TempDir())
	value := newTestSession(t, t.TempDir())
	history := texts(t, "turn", 2, 10)
	saveMessages(t, store, value, history)
	files := map[string][]byte{}
	for _, name := range []string{value.ID + ".log", value.ID + ".meta.json"} {
		data, err := os.ReadFile(filepath.Join(store.root, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	invalid := value
	invalid.Messages = append(history[:2:2], anthropic.EncodedMessage{})
	if err := store.Save(invalid); err == nil {
		t.Fatal("Save accepted an empty message")
	}
	for name, before := range files {
		if after, err := os.ReadFile(filepath.Join(store.root, name)); err != nil || !bytes.Equal(before, after) {
			t.Fatalf("failed save changed %s: %v", name, err)
		}
	}
	if entries, err := os.ReadDir(store.root); err != nil || len(entries) != 2 {
		t.Fatalf("failed save left files: %v, %v", entries, err)
	}
	if loaded := loadFresh(t, store.root, value.ID); !sameHistory(loaded.Messages, history) {
		t.Fatalf("failed save changed the history: %d messages", len(loaded.Messages))
	}
	history = append(history, text(t, "valid"))
	saveMessages(t, store, value, history)
	if loaded := loadFresh(t, store.root, value.ID); !sameHistory(loaded.Messages, history) {
		t.Fatalf("save after a failed save = %d messages", len(loaded.Messages))
	}
}

func TestLegacySessionMigratesOnSave(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	value := newTestSession(t, t.TempDir())
	value.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	value.Messages = allBlockKinds(t)
	value.Usage = anthropic.Usage{InputTokens: 3, OutputTokens: 4}
	writeLegacy(t, root, value)
	legacyPath := filepath.Join(root, value.ID+".json")
	indexPath := filepath.Join(root, value.ID+".meta.json")
	listOne := func(messageCount int) {
		t.Helper()
		all, err := store.List("")
		if err != nil || len(all) != 1 || all[0].ID != value.ID || all[0].MessageCount != messageCount || all[0].WorkDir != value.WorkDir {
			t.Fatalf("List = %+v, %v", all, err)
		}
	}
	listOne(len(value.Messages))
	legacy, err := store.Load(value.ID)
	if err != nil || !reflect.DeepEqual(legacy, value) {
		t.Fatalf("legacy Load = %+v, %v", legacy, err)
	}

	// A failed index write must keep the legacy file.
	if err := os.Mkdir(indexPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(legacy); err == nil {
		t.Fatal("Save ignored a failed index write")
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy file removed before the index existed: %v", err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	listOne(len(value.Messages))

	if err := store.Save(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("legacy file after migration: %v", err)
	}
	migrated := loadFresh(t, root, value.ID)
	for index := range legacy.Messages {
		if !bytes.Equal(migrated.Messages[index].Wire(), legacy.Messages[index].Wire()) {
			t.Fatalf("message %d changed in migration:\n%s\n%s", index, legacy.Messages[index].Wire(), migrated.Messages[index].Wire())
		}
	}
	if len(migrated.Messages) != len(legacy.Messages) || migrated.Usage != value.Usage || migrated.WorkDir != value.WorkDir {
		t.Fatalf("migrated session = %+v", migrated)
	}

	// A crash between the index write and the removal leaves both formats.
	writeLegacy(t, root, Session{ID: value.ID, WorkDir: value.WorkDir})
	listOne(len(value.Messages))
	if latest, err := store.Latest(""); err != nil || !sameHistory(latest.Messages, legacy.Messages) {
		t.Fatalf("Latest read the stale legacy file: %+v, %v", latest, err)
	}
	if err := store.Save(migrated); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale legacy file after save: %v", err)
	}
}

// An export must open in an older build, and changes that build makes must
// survive the next start of this build instead of being hidden by the log.
func TestExportRoundTripsThroughAnOlderBuild(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	value := newTestSession(t, t.TempDir())
	history := allBlockKinds(t)
	saveMessages(t, store, value, history)
	path, err := store.Export(value.ID)
	if err != nil || path != filepath.Join(root, value.ID+".json") {
		t.Fatalf("Export = %q, %v", path, err)
	}
	var older struct {
		ID       string              `json:"id"`
		Messages []anthropic.Message `json:"messages"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &older); err != nil || older.ID != value.ID || len(older.Messages) != len(history) {
		t.Fatalf("older build cannot read the export: %+v, %v", older, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("export mode = %v, %v", info, err)
	}
	all, err := store.List("")
	if err != nil || len(all) != 1 || all[0].MessageCount != len(history) {
		t.Fatalf("List with export = %+v, %v", all, err)
	}
	if unchanged := loadFresh(t, root, value.ID); !sameHistory(unchanged.Messages, history) {
		t.Fatal("an unchanged export replaced the log history")
	}

	// The older build adds a turn and saves its own format.
	changed := loadFresh(t, root, value.ID)
	changed.Messages = append(slices.Clone(changed.Messages), text(t, "turn from the older build"))
	changed.UpdatedAt = changed.UpdatedAt.Add(time.Minute)
	writeLegacy(t, root, changed)
	all, err = store.List("")
	if err != nil || len(all) != 1 || all[0].MessageCount != len(history)+1 {
		t.Fatalf("List after older-build change = %+v, %v", all, err)
	}
	loaded, err := store.Load(value.ID)
	if err != nil || !sameHistory(loaded.Messages, changed.Messages) {
		t.Fatalf("Load lost the older-build turn: %d messages, %v", len(loaded.Messages), err)
	}
	if err := store.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("export after save: %v", err)
	}
	if final := loadFresh(t, root, value.ID); !sameHistory(final.Messages, changed.Messages) {
		t.Fatal("the log after the save does not hold the older-build turn")
	}
}

// Output files of a deleted or never-saved session must not stay forever, but
// outputs of a saved session, or of a recent one-shot run, must stay.
func TestRemoveStaleOutputsKeepsSavedAndRecentSessions(t *testing.T) {
	if err := NewStore(filepath.Join(t.TempDir(), "missing")).RemoveStaleOutputs(time.Now()); err != nil {
		t.Fatalf("missing store: %v", err)
	}
	root := t.TempDir()
	store := NewStore(root)
	saved := newTestSession(t, t.TempDir())
	saveMessages(t, store, saved, texts(t, "turn", 1, 10))
	legacy := newTestSession(t, t.TempDir())
	writeLegacy(t, root, legacy)
	stale, recent := newTestSession(t, t.TempDir()), newTestSession(t, t.TempDir())
	now := time.Now()
	old := now.Add(-StaleOutputAge - time.Hour)
	for _, value := range []Session{saved, legacy, stale, recent} {
		directory, err := store.OutputDirectory(value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "output.txt"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if value.ID != recent.ID {
			if err := os.Chtimes(directory, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.RemoveStaleOutputs(now); err != nil {
		t.Fatal(err)
	}
	for _, value := range []Session{saved, legacy, stale, recent} {
		directory, err := store.OutputDirectory(value.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, statErr := os.Stat(directory)
		if removed := errors.Is(statErr, fs.ErrNotExist); removed != (value.ID == stale.ID) {
			t.Fatalf("output directory of %s removed=%v, stat error %v", value.ID, removed, statErr)
		}
	}
}

func TestConcurrentUseKeepsOneCompleteHistory(t *testing.T) {
	store := NewStore(t.TempDir())
	value := newTestSession(t, t.TempDir())
	history := texts(t, "turn", 16, 100)
	errs := make(chan error, 3*len(history))
	var group sync.WaitGroup
	for count := 1; count <= len(history); count++ {
		group.Add(1)
		go func() {
			defer group.Done()
			saved := value
			saved.Messages = history[:count]
			errs <- store.Save(saved)
			_, err := store.Load(value.ID)
			errs <- err
			_, err = store.List("")
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded := loadFresh(t, store.root, value.ID)
	if len(loaded.Messages) == 0 || !sameHistory(loaded.Messages, history[:len(loaded.Messages)]) {
		t.Fatalf("concurrent saves mixed histories: %d messages", len(loaded.Messages))
	}
	all, err := store.List("")
	if err != nil || len(all) != 1 || all[0].MessageCount != len(loaded.Messages) {
		t.Fatalf("index disagrees with log: %+v, %v", all, err)
	}
}
