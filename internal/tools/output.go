package tools

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// outputHeadBytes and outputTailBytes set how much of a large command
	// output stays in the model context. The rest goes to a file.
	outputHeadBytes   = 16 * 1024
	outputTailBytes   = 16 * 1024
	outputInlineBytes = outputHeadBytes + outputTailBytes
	// maxSavedOutputBytes stops a runaway command from filling the disk.
	maxSavedOutputBytes = 64 * 1024 * 1024
	// maxOutputDirectoryBytes bounds the saved outputs of one session. A new
	// file always fits, because one file holds at most maxSavedOutputBytes.
	maxOutputDirectoryBytes = 256 * 1024 * 1024
	outputWriteBuffer       = 32 * 1024
)

// outputCollector receives the combined stdout and stderr of one command.
// It keeps only the head and a tail ring in memory. When the output becomes
// larger than outputInlineBytes, it streams the full output to a new file.
// exec.Cmd calls Write from one goroutine at a time because stdout and
// stderr share this writer, so the collector needs no lock.
type outputCollector struct {
	directory string
	head      []byte
	// tail is a ring buffer. The write position follows from total, so the
	// ring needs no separate index.
	tail  []byte
	total int64
	// At most one of saved and saveErr is set. Both are nil while the output
	// fits in memory.
	saved   *outputFile
	saveErr error
}

type outputFile struct {
	file    *os.File
	buffer  *bufio.Writer
	written int64
}

func newOutputCollector(directory string) *outputCollector {
	return &outputCollector{directory: directory}
}

func (c *outputCollector) Write(chunk []byte) (int, error) {
	if c.saved == nil && c.saveErr == nil && c.total+int64(len(chunk)) > outputInlineBytes {
		c.startFile()
	}
	if c.saved != nil {
		c.save(chunk)
	}
	c.keep(chunk)
	return len(chunk), nil
}

// startFile creates the output file and writes all bytes received so far.
// Before this call the head and the ring hold the complete output, because
// the total is not larger than outputInlineBytes.
func (c *outputCollector) startFile() {
	file, err := createOutputFile(c.directory)
	if err != nil {
		c.saveErr = err
		return
	}
	c.saved = &outputFile{file: file, buffer: bufio.NewWriterSize(file, outputWriteBuffer)}
	first, second := c.orderedTail()
	for _, part := range [][]byte{c.head, first, second} {
		if c.saved == nil {
			return
		}
		c.save(part)
	}
}

func createOutputFile(directory string) (*os.File, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	var name [16]byte
	// crypto/rand.Read never returns an error in Go 1.24.
	_, _ = rand.Read(name[:])
	return os.OpenFile(filepath.Join(directory, hex.EncodeToString(name[:])+".txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// save writes chunk to the output file up to maxSavedOutputBytes.
func (c *outputCollector) save(chunk []byte) {
	room := maxSavedOutputBytes - c.saved.written
	if room <= 0 {
		return
	}
	if int64(len(chunk)) > room {
		chunk = chunk[:room]
	}
	written, err := c.saved.buffer.Write(chunk)
	c.saved.written += int64(written)
	if err != nil {
		c.failFile(err)
	}
}

// failFile closes and deletes the incomplete file, so no partial output
// stays on disk without a reference in the result.
func (c *outputCollector) failFile(cause error) {
	c.dropFile(errors.Join(cause, c.saved.file.Close()))
}

// dropFile deletes the closed file and records why the output was not saved.
func (c *outputCollector) dropFile(cause error) {
	if err := os.Remove(c.saved.file.Name()); err != nil {
		cause = fmt.Errorf("%w; remove incomplete file: %w", cause, err)
	}
	c.saved = nil
	c.saveErr = cause
}

func (c *outputCollector) keep(chunk []byte) {
	if room := outputHeadBytes - len(c.head); room > 0 {
		taken := min(room, len(chunk))
		c.head = append(c.head, chunk[:taken]...)
		c.total += int64(taken)
		chunk = chunk[taken:]
	}
	if len(chunk) == 0 {
		return
	}
	if c.tail == nil {
		c.tail = make([]byte, outputTailBytes)
	}
	end := c.total - outputHeadBytes + int64(len(chunk))
	c.total += int64(len(chunk))
	if len(chunk) > outputTailBytes {
		chunk = chunk[len(chunk)-outputTailBytes:]
	}
	start := (end - int64(len(chunk))) % outputTailBytes
	copied := copy(c.tail[start:], chunk)
	copy(c.tail, chunk[copied:])
}

// orderedTail returns the ring content, oldest bytes first, as two parts.
func (c *outputCollector) orderedTail() (first, second []byte) {
	routed := c.total - int64(len(c.head))
	if routed <= outputTailBytes {
		return c.tail[:routed], nil
	}
	next := routed % outputTailBytes
	return c.tail[next:], c.tail[:next]
}

// finish closes the output file and returns the text for the model. An empty
// output gives an empty string.
func (c *outputCollector) finish() string {
	var pruneErr error
	if c.saved != nil {
		if err := errors.Join(c.saved.buffer.Flush(), c.saved.file.Close()); err != nil {
			c.dropFile(err)
		} else {
			pruneErr = pruneOutputDirectory(c.directory, c.saved.file.Name(), maxOutputDirectoryBytes)
		}
	}
	first, second := c.orderedTail()
	if c.total <= outputInlineBytes {
		return string(c.head) + string(first)
	}
	head := trimIncompleteRuneEnd(c.head)
	tail := trimIncompleteRuneStart(append(append(make([]byte, 0, outputTailBytes), first...), second...))
	omitted := c.total - int64(len(head)) - int64(len(tail))

	var text strings.Builder
	text.Grow(len(head) + len(tail) + 512)
	text.Write(head)
	if !bytes.HasSuffix(head, []byte("\n")) {
		text.WriteByte('\n')
	}
	fmt.Fprintf(&text, "[output truncated: %d bytes omitted; ", omitted)
	if c.saved != nil {
		fmt.Fprintf(&text, "full output (%d bytes) saved to %s", c.total, c.saved.file.Name())
		if c.total > maxSavedOutputBytes {
			fmt.Fprintf(&text, "; the file holds only the first %d bytes", maxSavedOutputBytes)
		}
		text.WriteString("; use read_file with offset and limit to read it")
		if pruneErr != nil {
			text.WriteString("; older output files were not removed: " + strings.ReplaceAll(pruneErr.Error(), "\n", "; "))
		}
	} else {
		text.WriteString("full output was not saved: " + strings.ReplaceAll(c.saveErr.Error(), "\n", "; "))
	}
	text.WriteString("]\n")
	text.Write(tail)
	return text.String()
}

// discard deletes the output file. The caller uses it when the result is not
// given to the model, so nothing refers to the file.
func (c *outputCollector) discard() error {
	if c.saved == nil {
		return nil
	}
	err := errors.Join(c.saved.file.Close(), os.Remove(c.saved.file.Name()))
	c.saved = nil
	return err
}

// trimIncompleteRuneEnd removes a multi-byte UTF-8 sequence that the cut
// split at the end of b. Invalid UTF-8 stays as it is.
func trimIncompleteRuneEnd(b []byte) []byte {
	for back := 1; back < utf8.UTFMax && back <= len(b); back++ {
		start := len(b) - back
		if utf8.RuneStart(b[start]) {
			if utf8.FullRune(b[start:]) {
				return b
			}
			return b[:start]
		}
	}
	return b
}

// trimIncompleteRuneStart removes the continuation bytes of a UTF-8
// sequence that the cut split at the start of b.
func trimIncompleteRuneStart(b []byte) []byte {
	skip := 0
	for skip < utf8.UTFMax-1 && skip < len(b) && !utf8.RuneStart(b[skip]) {
		skip++
	}
	return b[skip:]
}

// pruneOutputDirectory deletes the oldest saved outputs until directory holds
// at most limit bytes. It never deletes keep, the file that the current result
// names. A file that another process already removed is not an error.
func pruneOutputDirectory(directory, keep string, limit int64) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	type savedOutput struct {
		path     string
		size     int64
		modified time.Time
	}
	older := make([]savedOutput, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		total += info.Size()
		if path := filepath.Join(directory, entry.Name()); path != keep {
			older = append(older, savedOutput{path: path, size: info.Size(), modified: info.ModTime()})
		}
	}
	slices.SortFunc(older, func(left, right savedOutput) int {
		if order := left.modified.Compare(right.modified); order != 0 {
			return order
		}
		return strings.Compare(left.path, right.path)
	})
	var failures []error
	for _, file := range older {
		if total <= limit {
			break
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = append(failures, err)
			continue
		}
		total -= file.size
	}
	return errors.Join(failures...)
}
