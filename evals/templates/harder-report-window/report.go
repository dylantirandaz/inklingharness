package audit

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Select is shared by the text, JSON, and summary views.
func Select(events []Event, q Query) ([]Event, error) {
	if q.Since > q.Until || q.Offset < 0 || q.Limit < 0 {
		return nil, ErrQuery
	}
	start := min(q.Offset, len(events))
	end := len(events)
	if q.Limit > 0 {
		end = min(start+q.Limit, end)
	}
	selected := events[start:end]
	out := selected[:0]
	for _, event := range selected {
		if event.Time < q.Since || event.Time > q.Until {
			continue
		}
		if q.Kind != "" && event.Kind != q.Kind {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

// Render writes one tab-separated line for each selected event.
func Render(w io.Writer, events []Event, q Query) error {
	selected, err := Select(events, q)
	if err != nil {
		return err
	}
	for _, event := range selected {
		_, err := fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", event.ID, event.Time, event.Kind, event.Message)
		if err != nil {
			return err
		}
	}
	return nil
}

// Export writes the selected events as one JSON array.
func Export(w io.Writer, events []Event, q Query) error {
	selected, err := Select(events, q)
	if err != nil {
		return err
	}
	if selected == nil {
		selected = []Event{}
	}
	return json.NewEncoder(w).Encode(selected)
}

// Summarize computes statistics for the selected page, not the whole input.
func Summarize(events []Event, q Query) (Summary, error) {
	selected, err := Select(events, q)
	if err != nil {
		return Summary{}, err
	}
	out := Summary{Total: len(selected), ByKind: make(map[string]int)}
	for i, event := range selected {
		out.ByKind[event.Kind]++
		if i == 0 || event.Time < out.First {
			out.First = event.Time
		}
		if i == 0 || event.Time > out.Last {
			out.Last = event.Time
		}
	}
	return out, nil
}

// ReadCSV reads ID, time, kind, and message columns. There is no header.
func ReadCSV(r io.Reader) ([]Event, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = 4
	var events []Event
	for row := 1; ; row++ {
		record, err := reader.Read()
		if err == io.EOF {
			return events, nil
		}
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", row, err)
		}
		stamp, err := strconv.ParseInt(record[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("row %d time: %w", row, err)
		}
		events = append(events, Event{record[0], stamp, record[2], record[3]})
	}
}

func WriteCSV(w io.Writer, events []Event) error {
	writer := csv.NewWriter(w)
	for _, event := range events {
		record := []string{event.ID, strconv.FormatInt(event.Time, 10), event.Kind, event.Message}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

// SortByTime returns a stable sorted copy.
func SortByTime(events []Event) []Event {
	out := append([]Event(nil), events...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Time < out[j].Time
	})
	return out
}

// Latest returns the first event with the greatest time.
func Latest(events []Event) (Event, bool) {
	if len(events) == 0 {
		return Event{}, false
	}
	latest := events[0]
	for _, event := range events[1:] {
		if event.Time > latest.Time {
			latest = event
		}
	}
	return latest, true
}

func Kinds(events []Event) []string {
	seen := make(map[string]bool)
	for _, event := range events {
		seen[event.Kind] = true
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// Group keeps the input order within each kind.
func Group(events []Event) map[string][]Event {
	out := make(map[string][]Event)
	for _, event := range events {
		out[event.Kind] = append(out[event.Kind], event)
	}
	return out
}

// Index keeps the last event with a repeated ID.
func Index(events []Event) map[string]Event {
	out := make(map[string]Event, len(events))
	for _, event := range events {
		out[event.ID] = event
	}
	return out
}

// Unique keeps the first event with each ID.
func Unique(events []Event) []Event {
	seen := make(map[string]bool, len(events))
	out := make([]Event, 0, len(events))
	for _, event := range events {
		if seen[event.ID] {
			continue
		}
		seen[event.ID] = true
		out = append(out, event)
	}
	return out
}

// MessageLines counts logical lines. An empty message has no lines.
func MessageLines(message string) int {
	if message == "" {
		return 0
	}
	lines := strings.Count(message, "\n")
	if !strings.HasSuffix(message, "\n") {
		lines++
	}
	return lines
}

// Search matches literal, case-sensitive text in the message.
func Search(events []Event, text string) []Event {
	out := make([]Event, 0)
	for _, event := range events {
		if strings.Contains(event.Message, text) {
			out = append(out, event)
		}
	}
	return out
}

// Redact returns a copy with matching message text replaced.
func Redact(events []Event, old, replacement string) []Event {
	out := append([]Event(nil), events...)
	if old == "" {
		return out
	}
	for i := range out {
		out[i].Message = strings.ReplaceAll(out[i].Message, old, replacement)
	}
	return out
}

// MergeByTime combines two inputs without changing either one.
func MergeByTime(left, right []Event) []Event {
	out := make([]Event, 0, len(left)+len(right))
	out = append(out, left...)
	out = append(out, right...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Time < out[j].Time
	})
	return out
}
