package anthropic

import (
	"bufio"
	"bytes"
	"io"
	"sync"
)

type serverSentEvent struct {
	Name string
	Data []byte
}

// eventScanner splits a text/event-stream body into events.
type eventScanner struct {
	reader *bufio.Reader
}

// readers keeps the 64 KiB read buffers of ended streams. Each turn streams
// one reply, so without the pool each turn allocates a new buffer.
var readers = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 64*1024) }}

// newEventScanner borrows a read buffer. Call release when the stream ends.
func newEventScanner(stream io.Reader) *eventScanner {
	reader := readers.Get().(*bufio.Reader)
	reader.Reset(stream)
	return &eventScanner{reader: reader}
}

// release gives the read buffer back. Events that next returned stay valid,
// because each event owns its data. The scanner must not be used after it.
func (s *eventScanner) release() {
	s.reader.Reset(nil)
	readers.Put(s.reader)
	s.reader = nil
}

// next returns the next complete event. It returns io.EOF when the stream
// ends. An event that the stream cuts before its blank line is dropped, as the
// SSE specification requires.
func (s *eventScanner) next() (serverSentEvent, error) {
	var name string
	var data []byte
	var hasData bool
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF {
				return serverSentEvent{}, io.EOF
			}
			return serverSentEvent{}, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if name == "" && !hasData {
				continue
			}
			return serverSentEvent{Name: name, Data: data}, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			name = eventName(value)
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			// The reader can reuse line on its next read. Each event owns
			// its data, and the JSON decoder can use the bytes directly.
			data = append(data, value...)
			hasData = true
		default:
			// The SSE specification tells clients to ignore fields they do not
			// know, for example "id" and "retry".
		}
	}
}

// readLine borrows the reader buffer for a short line. A long line needs a
// copy, but it has no size limit. The next read can overwrite the result.
func (s *eventScanner) readLine() ([]byte, error) {
	line, err := s.reader.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return line, err
	}
	full := append([]byte(nil), line...)
	for err == bufio.ErrBufferFull {
		line, err = s.reader.ReadSlice('\n')
		full = append(full, line...)
	}
	return full, err
}

// eventName returns a shared string for the event names of the Messages API,
// so the common events do not allocate a name.
func eventName(value []byte) string {
	switch string(value) {
	case "content_block_delta":
		return "content_block_delta"
	case "content_block_start":
		return "content_block_start"
	case "content_block_stop":
		return "content_block_stop"
	case "message_start":
		return "message_start"
	case "message_delta":
		return "message_delta"
	case "message_stop":
		return "message_stop"
	case "ping":
		return "ping"
	}
	return string(value)
}
