package anthropic

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestEventScannerFields(t *testing.T) {
	tests := []struct {
		name  string
		input string
		event string
		data  string
	}{
		{"single", "event: reply\ndata: value\n\n", "reply", "value"},
		{"empty data", "data:\n\n", "", ""},
		{"no colon", "data\n\n", "", ""},
		{"event only", "event: reply\n\n", "reply", ""},
		{"multiple data", "data: first\ndata:\ndata: third\n\n", "", "first\n\nthird"},
		{"empty first data", "data:\ndata: second\n\n", "", "\nsecond"},
		{"one space", "data:  value\n\n", "", " value"},
		{"carriage returns", "event: reply\r\n data: ignored\r\ndata: value\r\r\n\r\n", "reply", "value"},
		{"ignored fields", "\n: comment\nid: 1\nretry: 10\n\nevent: first\nevent: reply\ndata: value\n\n", "reply", "value"},
		{"empty event", "event: first\nevent:\n\ndata: value\n\n", "", "value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scanner := newEventScanner(iotest.OneByteReader(strings.NewReader(test.input)))
			event, err := scanner.next()
			if err != nil {
				t.Fatal(err)
			}
			if event.Name != test.event || string(event.Data) != test.data {
				t.Fatalf("event = %q, %q; want %q, %q", event.Name, event.Data, test.event, test.data)
			}
			if _, err := scanner.next(); err != io.EOF {
				t.Fatalf("end = %v, want EOF", err)
			}
		})
	}
}

func TestEventScannerLongLinesAndOwnedData(t *testing.T) {
	for _, length := range []int{64*1024 - 8, 64*1024 - 7, 64*1024 - 6, 64 * 1024, 3 * 64 * 1024} {
		value := strings.Repeat("x", length)
		input := "event: first\ndata: " + value + "\r\n\n" +
			"event: second\ndata: " + strings.Repeat("y", 2*64*1024) + "\n\n"
		scanner := newEventScanner(strings.NewReader(input))
		first, err := scanner.next()
		if err != nil {
			t.Fatalf("length %d: %v", length, err)
		}
		second, err := scanner.next()
		if err != nil || second.Name != "second" || len(second.Data) != 2*64*1024 {
			t.Fatalf("length %d: second event name = %q, size = %d, error = %v", length, second.Name, len(second.Data), err)
		}
		if first.Name != "first" || string(first.Data) != value {
			t.Fatalf("length %d: a later read changed the first event", length)
		}
	}
}

func TestEventScannerDropsPartialEvents(t *testing.T) {
	failure := errors.New("stream read failed")
	for _, partial := range []string{"data: partial", "data: partial\n", "data: " + strings.Repeat("x", 3*64*1024)} {
		for _, terminal := range []error{io.EOF, failure} {
			reader := io.MultiReader(strings.NewReader("data: complete\n\n"+partial), iotest.ErrReader(terminal))
			scanner := newEventScanner(reader)
			first, err := scanner.next()
			if err != nil || string(first.Data) != "complete" {
				t.Fatalf("complete event = %q, error = %v", first.Data, err)
			}
			event, err := scanner.next()
			if !errors.Is(err, terminal) || event.Name != "" || len(event.Data) != 0 {
				t.Fatalf("partial event = %q, %q, error = %v; want %v", event.Name, event.Data, err, terminal)
			}
		}
	}
}
