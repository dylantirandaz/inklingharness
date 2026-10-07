package audit_test

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	audit "fixture"
)

type failedWriter struct { err error }
func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestUnchangedUtilityBehavior(t *testing.T) {
	input := []audit.Event{{ID: "b", Time: 2, Kind: "warn", Message: "secret, yes"}, {ID: "a", Time: 1, Kind: "info", Message: "line\n"}, {ID: "b", Time: 2, Kind: "info", Message: "last"}}
	before := slices.Clone(input)
	var buf bytes.Buffer
	if err := audit.WriteCSV(&buf, input); err != nil { t.Fatal(err) }
	roundTrip, err := audit.ReadCSV(&buf)
	if err != nil || !slices.Equal(roundTrip, input) { t.Fatalf("CSV: %v, %v", roundTrip, err) }
	if _, err := audit.ReadCSV(strings.NewReader("id,no,kind,text\n")); err == nil { t.Fatal("invalid CSV time") }
	if _, err := audit.ReadCSV(strings.NewReader("id,1,kind\n")); err == nil { t.Fatal("invalid CSV columns") }
	sorted := audit.SortByTime(input)
	if !slices.Equal(sorted, []audit.Event{input[1], input[0], input[2]}) { t.Fatalf("stable sort: %v", sorted) }
	if latest, ok := audit.Latest(input); !ok || latest != input[0] { t.Fatal("latest tie") }
	if _, ok := audit.Latest(nil); ok { t.Fatal("latest empty") }
	if !slices.Equal(audit.Kinds(input), []string{"info", "warn"}) { t.Fatal("kinds") }
	if !slices.Equal(audit.Group(input)["info"], []audit.Event{input[1], input[2]}) { t.Fatal("group") }
	if audit.Index(input)["b"] != input[2] { t.Fatal("index") }
	if !slices.Equal(audit.Unique(input), input[:2]) { t.Fatal("unique") }
	if !slices.Equal(audit.Search(input, "secret"), input[:1]) { t.Fatal("search") }
	redacted := audit.Redact(input, "secret", "hidden")
	if redacted[0].Message != "hidden, yes" { t.Fatal("redact") }
	if !slices.Equal(audit.MergeByTime(input[:1], input[1:]), sorted) { t.Fatal("merge") }
	for text, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "\n\n": 2} {
		if got := audit.MessageLines(text); got != want { t.Fatalf("lines %q: %d", text, got) }
	}
	if !reflect.DeepEqual(input, before) { t.Fatal("utility changed input") }
}

func TestWriterErrorIdentity(t *testing.T) {
	failure := errors.New("full")
	writer := failedWriter{failure}
	q := audit.Query{Since: 0, Until: 30}
	if !errors.Is(audit.Render(writer, sample(), q), failure) { t.Fatal("Render lost error") }
	if !errors.Is(audit.Export(writer, sample(), q), failure) { t.Fatal("Export lost error") }
	if !errors.Is(audit.WriteCSV(writer, sample()), failure) { t.Fatal("WriteCSV lost error") }
}
