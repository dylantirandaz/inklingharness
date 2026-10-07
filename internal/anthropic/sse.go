package anthropic

import (
	"bufio"
	"io"
	"strings"
)

type serverSentEvent struct {
	Name string
	Data string
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
	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return serverSentEvent{}, io.EOF
			}
			return serverSentEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if name == "" && len(data) == 0 {
				continue
			}
			return serverSentEvent{Name: name, Data: strings.Join(data, "\n")}, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			data = append(data, value)
		default:
			// The SSE specification tells clients to ignore fields they do not
			// know, for example "id" and "retry".
		}
	}
}
