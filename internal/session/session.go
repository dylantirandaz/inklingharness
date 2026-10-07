// Package session persists conversations in private files. Each session has
// an append-only log of checksummed records and a small metadata index.
package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

// Files of session <id> in the store root. The log is the authority for the
// history and the metadata. The index lets List and Latest choose a session
// without a read of any log. Earlier releases wrote one legacy JSON file; the
// first Save of that session replaces it.
const (
	logSuffix     = ".log"
	indexSuffix   = ".meta.json"
	legacySuffix  = ".json"
	outputsSuffix = ".outputs"
)

const (
	formatVersion = 2
	// A record starts with the payload length and the CRC-32C of the payload.
	// Both are little-endian uint32 values.
	recordHeaderSize = 8
	// Save rewrites the log as one record when the log becomes larger than two
	// times the live history plus this slack. The slack prevents a rewrite of a
	// short log after each small change.
	rewriteSlack = 64 << 10
	ioBufferSize = 64 << 10
	// StaleOutputAge is how long the output directory of a session that has
	// no saved files stays on disk. A one-shot run never saves its session,
	// so its outputs stay available for inspection during this time.
	StaleOutputAge = 7 * 24 * time.Hour
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

type Session struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	WorkDir   string
	Model     string
	Effort    string
	Messages  []anthropic.EncodedMessage
	Usage     anthropic.Usage
}

type Summary struct {
	ID           string
	UpdatedAt    time.Time
	WorkDir      string
	Model        string
	MessageCount int
}

// Store saves and loads sessions in one directory. Its methods are safe for
// concurrent use.
type Store struct {
	root  string
	mutex sync.Mutex
	// current is the log of the session that this Store saved or loaded last.
	// Nil means that this Store knows no log. The Store keeps one session only,
	// so a history that the caller drops (for example after /clear) does not
	// stay in memory.
	current *logState
}

// logState is what a Store knows about one log file.
type logState struct {
	id string
	// messages is the history in the log. The values share their wire bytes
	// with the caller.
	messages []anthropic.EncodedMessage
	file     fs.FileInfo
	// size is the file size that the Store saw last. valid is the length of
	// the complete records. It is less than size after a torn write.
	size  int64
	valid int64
}

// recordHead is the metadata of one log record. The messages follow it in the
// same JSON object.
type recordHead struct {
	Version int `json:"version"`
	// Keep is the number of messages from the start of the history that stay.
	// The record messages replace all later messages.
	Keep      int             `json:"keep"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	WorkDir   string          `json:"work_dir"`
	Model     string          `json:"model"`
	Effort    string          `json:"effort"`
	Usage     anthropic.Usage `json:"usage"`
}

type recordBody struct {
	recordHead
	Messages []anthropic.EncodedMessage `json:"messages"`
}

// indexFile is the metadata that List and Latest read. The log stays the
// authority. After a crash, the index can be one save behind the log.
type indexFile struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	WorkDir      string    `json:"work_dir"`
	Model        string    `json:"model"`
	Effort       string    `json:"effort"`
	MessageCount int       `json:"message_count"`
}

// legacyFile is the session format of earlier releases: one JSON object that
// each save replaced.
type legacyFile struct {
	ID        string                     `json:"id"`
	CreatedAt time.Time                  `json:"created_at"`
	UpdatedAt time.Time                  `json:"updated_at"`
	WorkDir   string                     `json:"work_dir"`
	Model     string                     `json:"model"`
	Effort    string                     `json:"effort"`
	Messages  []anthropic.EncodedMessage `json:"messages"`
	Usage     anthropic.Usage            `json:"usage"`
}

// legacySummary reads only the fields that List needs from a legacy file.
// Empty structs count the messages without a copy of their content.
type legacySummary struct {
	ID        string     `json:"id"`
	UpdatedAt time.Time  `json:"updated_at"`
	WorkDir   string     `json:"work_dir"`
	Model     string     `json:"model"`
	Messages  []struct{} `json:"messages"`
}

func DefaultStore() (*Store, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" || !filepath.IsAbs(root) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("session: home directory: %w", err)
		}
		root = filepath.Join(home, ".local", "state")
	}
	return NewStore(filepath.Join(root, "inkling", "sessions")), nil
}

func NewStore(root string) *Store {
	return &Store{root: root}
}

func New(workDir, model, effort string) (Session, error) {
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return Session{}, fmt.Errorf("session: work directory: %w", err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Session{}, fmt.Errorf("session: generate ID: %w", err)
	}
	now := time.Now().UTC()
	return Session{
		ID: hex.EncodeToString(id[:]), CreatedAt: now, UpdatedAt: now,
		WorkDir: absolute, Model: model, Effort: effort,
	}, nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// base returns the path of session id in the store without a file suffix.
func (s *Store) base(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("session: invalid ID %q: expected 32 lowercase hexadecimal characters", id)
	}
	if s.root == "" {
		return "", errors.New("session: empty store directory")
	}
	return filepath.Join(s.root, id), nil
}

// OutputDirectory returns the absolute directory for the large tool outputs
// of session id. The tool that saves an output creates the directory.
func (s *Store) OutputDirectory(id string) (string, error) {
	base, err := s.base(id)
	if err != nil {
		return "", err
	}
	directory, err := filepath.Abs(base + outputsSuffix)
	if err != nil {
		return "", fmt.Errorf("session: output directory: %w", err)
	}
	return directory, nil
}

// Save writes only what changed since this Store last saved or loaded the same
// session, usually as one appended record. It rewrites the whole log when this
// Store does not know the log on disk, or when old records use too much space.
// Save refreshes the stored UpdatedAt and does not modify value.
func (s *Store) Save(value Session) error {
	base, err := s.base(value.ID)
	if err != nil {
		return err
	}
	for index, message := range value.Messages {
		if len(message.Wire()) == 0 {
			return fmt.Errorf("session: message %d is empty", index)
		}
	}
	if err := s.prepareRoot(); err != nil {
		return err
	}
	stored := value
	stored.UpdatedAt = time.Now().UTC()
	// The clone keeps the saved history correct if the caller changes its
	// slice in place later. The wire bytes stay shared.
	stored.Messages = slices.Clone(value.Messages)

	s.mutex.Lock()
	defer s.mutex.Unlock()
	state, err := s.writeLog(base+logSuffix, stored)
	if err != nil {
		// The log can end in a partial record now. Load ignores that record,
		// and the next Save rewrites the log.
		s.current = nil
		return err
	}
	s.current = &state
	if err := writeIndex(s.root, base+indexSuffix, stored); err != nil {
		return err
	}
	return removeLegacy(s.root, base+legacySuffix)
}

func (s *Store) prepareRoot() error {
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return fmt.Errorf("session: create store: %w", err)
	}
	if err := s.checkRoot(); err != nil {
		return err
	}
	if err := os.Chmod(s.root, 0700); err != nil {
		return fmt.Errorf("session: protect store: %w", err)
	}
	return nil
}

func (s *Store) checkRoot() error {
	if s.root == "" {
		return errors.New("session: empty store directory")
	}
	info, err := os.Lstat(s.root)
	if err != nil {
		return fmt.Errorf("session: inspect store: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("session: store must be a directory, not a symlink or other file: %q", s.root)
	}
	return nil
}

// writeLog appends the change from the known log to stored, or writes a new
// log. The caller holds the mutex.
func (s *Store) writeLog(path string, stored Session) (logState, error) {
	if previous := s.current; previous != nil && previous.id == stored.ID {
		keep := commonPrefix(previous.messages, stored.Messages)
		change, err := newRecord(stored, keep)
		if err != nil {
			return logState{}, err
		}
		end := previous.valid + change.size()
		if end <= 2*wireBytes(stored.Messages)+rewriteSlack {
			appended, err := appendRecord(path, *previous, change)
			if err != nil {
				return logState{}, err
			}
			if appended {
				return logState{id: stored.ID, messages: stored.Messages, file: previous.file, size: end, valid: end}, nil
			}
		}
	}
	snapshot, err := newRecord(stored, 0)
	if err != nil {
		return logState{}, err
	}
	file, err := writeAtomically(s.root, path, snapshot.write)
	if err != nil {
		return logState{}, err
	}
	return logState{id: stored.ID, messages: stored.Messages, file: file, size: snapshot.size(), valid: snapshot.size()}, nil
}

func commonPrefix(left, right []anthropic.EncodedMessage) int {
	limit := min(len(left), len(right))
	for index := range limit {
		if !left[index].Equal(right[index]) {
			return index
		}
	}
	return limit
}

func wireBytes(messages []anthropic.EncodedMessage) int64 {
	var total int64
	for _, message := range messages {
		total += int64(len(message.Wire()))
	}
	return total
}

// record is one log record that is ready to write. The writer sends the parts
// in sequence, so a large record does not need a second copy of the history
// in memory.
type record struct {
	// head is the JSON object of the metadata without its closing brace,
	// followed by the start of the messages array.
	head     []byte
	messages []anthropic.EncodedMessage
	length   uint32
	checksum uint32
}

var (
	messageSeparator = []byte(",")
	recordEnd        = []byte("]}")
)

func newRecord(value Session, keep int) (record, error) {
	head, err := json.Marshal(recordHead{
		Version: formatVersion, Keep: keep,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		WorkDir: value.WorkDir, Model: value.Model, Effort: value.Effort,
		Usage: value.Usage,
	})
	if err != nil {
		return record{}, fmt.Errorf("session: encode record: %w", err)
	}
	result := record{
		head:     append(head[:len(head)-1], `,"messages":[`...),
		messages: value.Messages[keep:],
	}
	var length uint64
	for part := range result.parts {
		length += uint64(len(part))
		result.checksum = crc32.Update(result.checksum, castagnoli, part)
	}
	if length > math.MaxUint32 {
		return record{}, fmt.Errorf("session: record of %d bytes is larger than the 4 GiB limit", length)
	}
	result.length = uint32(length)
	return result, nil
}

func (r record) parts(yield func([]byte) bool) {
	if !yield(r.head) {
		return
	}
	for index, message := range r.messages {
		if index > 0 && !yield(messageSeparator) {
			return
		}
		if !yield(message.Wire()) {
			return
		}
	}
	yield(recordEnd)
}

func (r record) size() int64 {
	return recordHeaderSize + int64(r.length)
}

func (r record) write(destination io.Writer) error {
	buffered := bufio.NewWriterSize(destination, int(min(r.size(), ioBufferSize)))
	var header [recordHeaderSize]byte
	binary.LittleEndian.PutUint32(header[:4], r.length)
	binary.LittleEndian.PutUint32(header[4:], r.checksum)
	if _, err := buffered.Write(header[:]); err != nil {
		return err
	}
	for part := range r.parts {
		if _, err := buffered.Write(part); err != nil {
			return err
		}
	}
	return buffered.Flush()
}

// appendRecord writes change after the complete records of the log that
// previous describes. It returns false and writes nothing when the file at
// path is not that log, for example after another process replaced it.
func appendRecord(path string, previous logState, change record) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("session: inspect %s: %w", filepath.Base(path), err)
	}
	if !os.SameFile(info, previous.file) || info.Size() != previous.size {
		return false, nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false, fmt.Errorf("session: open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("session: inspect %s: %w", filepath.Base(path), err)
	}
	// A symlink can replace the log between Lstat and OpenFile.
	if !os.SameFile(opened, previous.file) {
		return false, nil
	}
	if previous.size != previous.valid {
		if err := file.Truncate(previous.valid); err != nil {
			return false, fmt.Errorf("session: remove torn record from %s: %w", filepath.Base(path), err)
		}
	}
	if err := change.write(io.NewOffsetWriter(file, previous.valid)); err != nil {
		return false, fmt.Errorf("session: append to %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("session: sync %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("session: close %s: %w", filepath.Base(path), err)
	}
	return true, nil
}

// writeAtomically publishes a complete, synced file at path, or it leaves path
// unchanged. Rename replaces a symlink at path; it does not follow it.
func writeAtomically(root, path string, write func(io.Writer) error) (fs.FileInfo, error) {
	file, err := os.CreateTemp(root, ".session-*")
	if err != nil {
		return nil, fmt.Errorf("session: create temporary file: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if err := write(file); err != nil {
		return nil, fmt.Errorf("session: write %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("session: sync %s: %w", filepath.Base(path), err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("session: inspect %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("session: close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return nil, fmt.Errorf("session: replace %s: %w", filepath.Base(path), err)
	}
	return info, nil
}

func writeIndex(root, path string, stored Session) error {
	data, err := json.Marshal(indexFile{
		Version: formatVersion, ID: stored.ID,
		CreatedAt: stored.CreatedAt, UpdatedAt: stored.UpdatedAt,
		WorkDir: stored.WorkDir, Model: stored.Model, Effort: stored.Effort,
		MessageCount: len(stored.Messages),
	})
	if err != nil {
		return fmt.Errorf("session: encode index: %w", err)
	}
	_, err = writeAtomically(root, path, func(destination io.Writer) error {
		_, err := destination.Write(data)
		return err
	})
	return err
}

// removeLegacy deletes the legacy file of a session after its log and index
// are durable.
func removeLegacy(root, path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session: inspect %s: %w", filepath.Base(path), err)
	}
	// The renames of the log and the index must reach the disk before the
	// removal. Otherwise a crash can leave no complete file for the session.
	directory, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("session: open store: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("session: sync store: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("session: remove %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Load reads the log of session id. A session that has only a legacy file
// loads from that file, and its next Save migrates it. When both exist, the
// copy with the later UpdatedAt wins: an export for an older build can be
// changed by that build, and its changes must not be lost on the next start.
func (s *Store) Load(id string) (Session, error) {
	base, err := s.base(id)
	if err != nil {
		return Session{}, err
	}
	if err := s.checkRoot(); err != nil {
		return Session{}, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	path := base + logSuffix
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		value, err := loadLegacy(base+legacySuffix, id)
		if errors.Is(err, fs.ErrNotExist) {
			return Session{}, fmt.Errorf("session: no saved session %s: %w", id, fs.ErrNotExist)
		}
		if err != nil {
			return Session{}, err
		}
		s.current = nil
		return value, nil
	}
	if err != nil {
		return Session{}, fmt.Errorf("session: inspect %s: %w", filepath.Base(path), err)
	}
	value, state, err := loadLog(path, id, info)
	if err != nil {
		return Session{}, err
	}
	legacy, err := readLegacySummary(base+legacySuffix, id)
	if errors.Is(err, fs.ErrNotExist) {
		s.current = &state
		return value, nil
	}
	if err != nil {
		return Session{}, err
	}
	if !legacy.UpdatedAt.After(value.UpdatedAt) {
		// The legacy file is an unchanged export. The next Save removes it.
		s.current = &state
		return value, nil
	}
	value, err = loadLegacy(base+legacySuffix, id)
	if err != nil {
		return Session{}, err
	}
	// The next Save writes a new log from this history and removes the
	// legacy file.
	s.current = nil
	return value, nil
}

// Export writes session id in the legacy single-file format, so a build that
// predates the log format can open it. It returns the path of that file. The
// next Save by this build replaces the export with the log again; Load uses
// whichever copy changed last.
func (s *Store) Export(id string) (string, error) {
	value, err := s.Load(id)
	if err != nil {
		return "", err
	}
	base, err := s.base(id)
	if err != nil {
		return "", err
	}
	path := base + legacySuffix
	legacy := legacyFile{
		ID: value.ID, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		WorkDir: value.WorkDir, Model: value.Model, Effort: value.Effort,
		Messages: value.Messages, Usage: value.Usage,
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, err := writeAtomically(s.root, path, func(destination io.Writer) error {
		buffered := bufio.NewWriterSize(destination, ioBufferSize)
		if err := json.NewEncoder(buffered).Encode(legacy); err != nil {
			return err
		}
		return buffered.Flush()
	}); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveStaleOutputs deletes the output directory of each session that has
// neither a log nor a legacy file and that did not change for StaleOutputAge.
// Nothing can refer to such files any longer. A missing store has nothing to
// remove.
func (s *Store) RemoveStaleOutputs(now time.Time) error {
	if err := s.checkRoot(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("session: list: %w", err)
	}
	saved := make(map[string]bool)
	var outputs []fs.DirEntry
	for _, entry := range entries {
		name := entry.Name()
		if id, found := strings.CutSuffix(name, outputsSuffix); found && validID(id) && entry.IsDir() {
			outputs = append(outputs, entry)
			continue
		}
		for _, suffix := range [...]string{logSuffix, legacySuffix} {
			if id, found := strings.CutSuffix(name, suffix); found && validID(id) {
				saved[id] = true
			}
		}
	}
	var failures []error
	for _, entry := range outputs {
		id := strings.TrimSuffix(entry.Name(), outputsSuffix)
		if saved[id] {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("session: inspect %s: %w", entry.Name(), err))
			continue
		}
		if now.Sub(info.ModTime()) < StaleOutputAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, entry.Name())); err != nil {
			failures = append(failures, fmt.Errorf("session: remove %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(failures...)
}

func loadLog(path, id string, info fs.FileInfo) (Session, logState, error) {
	name := filepath.Base(path)
	if !info.Mode().IsRegular() {
		return Session{}, logState{}, fmt.Errorf("session: %s is not a regular file", name)
	}
	file, err := os.Open(path)
	if err != nil {
		return Session{}, logState{}, fmt.Errorf("session: open %s: %w", name, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return Session{}, logState{}, fmt.Errorf("session: inspect %s: %w", name, err)
	}
	if !os.SameFile(info, opened) {
		return Session{}, logState{}, fmt.Errorf("session: %s changed while it was opened", name)
	}
	history, err := replay(bufio.NewReaderSize(file, ioBufferSize), opened.Size())
	if err != nil {
		return Session{}, logState{}, fmt.Errorf("session: read %s: %w", name, err)
	}
	value := Session{
		ID: id, CreatedAt: history.head.CreatedAt, UpdatedAt: history.head.UpdatedAt,
		WorkDir: history.head.WorkDir, Model: history.head.Model, Effort: history.head.Effort,
		Messages: history.messages, Usage: history.head.Usage,
	}
	state := logState{
		id: id, messages: slices.Clone(history.messages),
		file: opened, size: opened.Size(), valid: history.valid,
	}
	return value, state, nil
}

// replayed is the result of a log read: the metadata of the last complete
// record, the history, and the length of the complete records.
type replayed struct {
	head     recordHead
	messages []anthropic.EncodedMessage
	valid    int64
}

// replay applies the records of a log in order. A crash during an append can
// leave a torn final record: its length goes past the end of the file, or its
// checksum fails and it ends at the end of the file. Some file systems (for
// example ext4 with data=writeback, or XFS) can also extend the file after a
// crash but keep the new bytes as zeros. Replay ignores such a record, and it
// ignores a tail that has only zero bytes. Any other bad record is corruption.
func replay(reader io.Reader, size int64) (replayed, error) {
	var result replayed
	var header [recordHeaderSize]byte
	var payload []byte
	for result.valid < size {
		remaining := size - result.valid
		if remaining < recordHeaderSize {
			break
		}
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return replayed{}, err
		}
		// A zero header is an empty record with a valid CRC. It is not a
		// record that Save writes, so it can only start a zero-filled tail.
		if header == ([recordHeaderSize]byte{}) {
			zero, err := onlyZeros(reader, remaining-recordHeaderSize)
			if err != nil {
				return replayed{}, err
			}
			if zero {
				break
			}
			return replayed{}, fmt.Errorf("zero bytes at offset %d come before more data", result.valid)
		}
		length := int64(binary.LittleEndian.Uint32(header[:4]))
		if length > remaining-recordHeaderSize {
			break
		}
		payload = slices.Grow(payload[:0], int(length))[:length]
		if _, err := io.ReadFull(reader, payload); err != nil {
			return replayed{}, err
		}
		end := result.valid + recordHeaderSize + length
		if crc32.Checksum(payload, castagnoli) != binary.LittleEndian.Uint32(header[4:]) {
			if end == size {
				break
			}
			return replayed{}, fmt.Errorf("record at offset %d fails its checksum", result.valid)
		}
		// EncodedMessage.UnmarshalJSON copies each message, so the history
		// does not keep the payload buffer.
		var body recordBody
		if err := json.Unmarshal(payload, &body); err != nil {
			return replayed{}, fmt.Errorf("record at offset %d: %w", result.valid, err)
		}
		if body.Version != formatVersion {
			return replayed{}, fmt.Errorf("record at offset %d has unsupported version %d", result.valid, body.Version)
		}
		if body.Keep < 0 || body.Keep > len(result.messages) {
			return replayed{}, fmt.Errorf("record at offset %d keeps %d messages of %d", result.valid, body.Keep, len(result.messages))
		}
		// Clear the replaced messages so that their wire bytes can be freed.
		clear(result.messages[body.Keep:])
		result.messages = append(result.messages[:body.Keep], body.Messages...)
		result.head = body.recordHead
		result.valid = end
	}
	if result.valid == 0 {
		return replayed{}, errors.New("no complete record")
	}
	return result, nil
}

// onlyZeros reports whether the next count bytes from reader are all zero.
func onlyZeros(reader io.Reader, count int64) (bool, error) {
	var chunk, zeros [4096]byte
	for count > 0 {
		part := chunk[:min(count, int64(len(chunk)))]
		if _, err := io.ReadFull(reader, part); err != nil {
			return false, err
		}
		if !bytes.Equal(part, zeros[:len(part)]) {
			return false, nil
		}
		count -= int64(len(part))
	}
	return true, nil
}

func loadLegacy(path, id string) (Session, error) {
	data, err := readRegular(path)
	if err != nil {
		return Session{}, err
	}
	// EncodedMessage.UnmarshalJSON copies each message, so the history does
	// not keep the file buffer.
	var legacy legacyFile
	if err := json.Unmarshal(data, &legacy); err != nil {
		return Session{}, fmt.Errorf("session: decode %s: %w", filepath.Base(path), err)
	}
	if legacy.ID != id {
		return Session{}, fmt.Errorf("session: stored ID %q does not match filename %q", legacy.ID, id)
	}
	return Session{
		ID: legacy.ID, CreatedAt: legacy.CreatedAt, UpdatedAt: legacy.UpdatedAt,
		WorkDir: legacy.WorkDir, Model: legacy.Model, Effort: legacy.Effort,
		Messages: legacy.Messages, Usage: legacy.Usage,
	}, nil
}

// readRegular reads a file that is not a symlink or another special file.
func readRegular(path string) ([]byte, error) {
	name := filepath.Base(path)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("session: inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("session: %s is not a regular file", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session: read %s: %w", name, err)
	}
	return data, nil
}

// List returns newest first, breaking timestamp ties by ID. A missing store is empty.
func (s *Store) List(workDir string) ([]Summary, error) {
	result, err := s.summaries(workDir)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(result, newestFirst)
	return result, nil
}

// Latest loads the newest session in workDir. It reads only the metadata of
// the other sessions.
func (s *Store) Latest(workDir string) (Session, error) {
	summaries, err := s.summaries(workDir)
	if err != nil {
		return Session{}, err
	}
	if len(summaries) == 0 {
		return Session{}, fmt.Errorf("session: no saved sessions: %w", fs.ErrNotExist)
	}
	return s.Load(slices.MinFunc(summaries, newestFirst).ID)
}

func newestFirst(left, right Summary) int {
	if order := right.UpdatedAt.Compare(left.UpdatedAt); order != 0 {
		return order
	}
	return strings.Compare(left.ID, right.ID)
}

// summaries reads the index of each session in workDir, or of all sessions
// when workDir is empty. A session with both an index and a legacy file is
// listed once, with the metadata of the copy that changed last, as Load
// chooses it.
func (s *Store) summaries(workDir string) ([]Summary, error) {
	if workDir != "" {
		absolute, err := filepath.Abs(workDir)
		if err != nil {
			return nil, fmt.Errorf("session: work directory: %w", err)
		}
		workDir = absolute
	}
	result := make([]Summary, 0)
	if err := s.checkRoot(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return result, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	indexed := make(map[string]Summary)
	var legacy []string
	for _, entry := range entries {
		name := entry.Name()
		if id, found := strings.CutSuffix(name, indexSuffix); found {
			if !validID(id) {
				continue
			}
			summary, err := readIndex(filepath.Join(s.root, name), id)
			if err != nil {
				return nil, err
			}
			indexed[id] = summary
			continue
		}
		if id, found := strings.CutSuffix(name, legacySuffix); found && validID(id) {
			legacy = append(legacy, id)
		}
	}
	for _, id := range legacy {
		summary, err := readLegacySummary(filepath.Join(s.root, id+legacySuffix), id)
		if err != nil {
			return nil, err
		}
		if index, found := indexed[id]; !found || summary.UpdatedAt.After(index.UpdatedAt) {
			indexed[id] = summary
		}
	}
	for _, summary := range indexed {
		if matchesWorkDir(summary, workDir) {
			result = append(result, summary)
		}
	}
	return result, nil
}

func matchesWorkDir(summary Summary, workDir string) bool {
	return workDir == "" || filepath.Clean(summary.WorkDir) == workDir
}

func readIndex(path, id string) (Summary, error) {
	data, err := readRegular(path)
	if err != nil {
		return Summary{}, err
	}
	var index indexFile
	if err := json.Unmarshal(data, &index); err != nil {
		return Summary{}, fmt.Errorf("session: decode %s: %w", filepath.Base(path), err)
	}
	if index.Version != formatVersion {
		return Summary{}, fmt.Errorf("session: %s has unsupported version %d", filepath.Base(path), index.Version)
	}
	if index.ID != id {
		return Summary{}, fmt.Errorf("session: stored ID %q does not match filename %q", index.ID, id)
	}
	return Summary{
		ID: index.ID, UpdatedAt: index.UpdatedAt, WorkDir: index.WorkDir,
		Model: index.Model, MessageCount: index.MessageCount,
	}, nil
}

func readLegacySummary(path, id string) (Summary, error) {
	data, err := readRegular(path)
	if err != nil {
		return Summary{}, err
	}
	var legacy legacySummary
	if err := json.Unmarshal(data, &legacy); err != nil {
		return Summary{}, fmt.Errorf("session: decode %s: %w", filepath.Base(path), err)
	}
	if legacy.ID != id {
		return Summary{}, fmt.Errorf("session: stored ID %q does not match filename %q", legacy.ID, id)
	}
	return Summary{
		ID: legacy.ID, UpdatedAt: legacy.UpdatedAt, WorkDir: legacy.WorkDir,
		Model: legacy.Model, MessageCount: len(legacy.Messages),
	}, nil
}
