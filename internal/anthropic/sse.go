package anthropic

import (
	"bufio"
	"bytes"
	"io"
)

type serverSentEvent struct {
	Name string
	Data []byte
}

// eventScanner splits a text/event-stream body into events.
type eventScanner struct {
	reader *bufio.Reader
}

func newEventScanner(stream io.Reader) *eventScanner {
	return &eventScanner{reader: bufio.NewReaderSize(stream, 64*1024)}
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
			name = string(value)
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
