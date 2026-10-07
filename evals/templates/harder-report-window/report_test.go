package audit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"testing"

	audit "fixture"
)

func sample() []audit.Event {
	return []audit.Event{
		{ID: "old", Time: 9, Kind: "ok"},
		{ID: "a", Time: 10, Kind: "ok", Message: "first"},
		{ID: "skip", Time: 11, Kind: "other"},
		{ID: "b", Time: 14, Kind: "ok", Message: "second"},
		{ID: "c", Time: 12, Kind: "ok", Message: "third"},
		{ID: "end", Time: 20, Kind: "ok"},
	}
}

func TestFilterBeforePageAndKeepInputOrder(t *testing.T) {
	input := sample()
	before := slices.Clone(input)
	q := audit.Query{Since: 10, Until: 20, Kind: "ok", Offset: 1, Limit: 2}
	out, err := audit.Select(input, q)
	want := []audit.Event{before[3], before[4]}
	if err != nil || !slices.Equal(out, want) { t.Fatalf("page: %v, %v", out, err) }
	if !slices.Equal(input, before) { t.Fatal("Select changed input") }
	out[0].Message = "changed"
	if !slices.Equal(input, before) { t.Fatal("Select returned an alias") }
}

func TestWindowEdgesAndPageBounds(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	cases := []struct { q audit.Query; ids []string }{
		{audit.Query{Since: 10, Until: 20}, []string{"a", "skip", "b", "c"}},
		{audit.Query{Since: 10, Until: 10}, nil},
		{audit.Query{Since: 10, Until: 20, Kind: "OK"}, nil},
		{audit.Query{Since: 10, Until: 20, Offset: 4}, nil},
		{audit.Query{Since: 10, Until: 20, Offset: maxInt, Limit: maxInt}, nil},
		{audit.Query{Since: 10, Until: 20, Offset: 1, Limit: maxInt}, []string{"skip", "b", "c"}},
		{audit.Query{Since: 10, Until: 20, Limit: 1}, []string{"a"}},
	}
	for _, tc := range cases {
		input := sample()
		before := slices.Clone(input)
		out, err := audit.Select(input, tc.q)
		if err != nil { t.Fatal(err) }
		var ids []string
		for _, event := range out { ids = append(ids, event.ID) }
		if !slices.Equal(ids, tc.ids) { t.Fatalf("%+v: %v, want %v", tc.q, ids, tc.ids) }
		if !slices.Equal(input, before) { t.Fatalf("input changed for %+v", tc.q) }
	}
	input := []audit.Event{{ID: "min", Time: math.MinInt64}, {ID: "max", Time: math.MaxInt64}}
	out, err := audit.Select(input, audit.Query{Since: math.MinInt64, Until: math.MaxInt64})
	if err != nil || len(out) != 1 || out[0].ID != "min" { t.Fatalf("time bounds: %v, %v", out, err) }
}

func TestAllConsumersUseTheSamePage(t *testing.T) {
	q := audit.Query{Since: 10, Until: 20, Kind: "ok", Offset: 1, Limit: 2}
	input := sample()
	before := slices.Clone(input)
	var text bytes.Buffer
	if err := audit.Render(&text, input, q); err != nil { t.Fatal(err) }
	if text.String() != "b\t14\tok\tsecond\nc\t12\tok\tthird\n" { t.Fatalf("text: %q", text.String()) }
	var data bytes.Buffer
	if err := audit.Export(&data, input, q); err != nil { t.Fatal(err) }
	var decoded []audit.Event
	if err := json.Unmarshal(data.Bytes(), &decoded); err != nil { t.Fatal(err) }
	if !slices.Equal(decoded, []audit.Event{before[3], before[4]}) { t.Fatalf("export: %v", decoded) }
	summary, err := audit.Summarize(input, q)
	want := audit.Summary{Total: 2, ByKind: map[string]int{"ok": 2}, First: 12, Last: 14}
	if err != nil || !reflect.DeepEqual(summary, want) { t.Fatalf("summary: %+v, %v", summary, err) }
	if !slices.Equal(input, before) { t.Fatal("a consumer changed input") }
}

func TestInvalidQueriesAndEmptyExport(t *testing.T) {
	for _, q := range []audit.Query{{Since: 2, Until: 1}, {Offset: -1}, {Limit: -1}} {
		input := sample()
		before := slices.Clone(input)
		out, err := audit.Select(input, q)
		if out != nil || !errors.Is(err, audit.ErrQuery) { t.Fatalf("invalid query: %v, %v", out, err) }
		var buf bytes.Buffer
		if !errors.Is(audit.Render(&buf, input, q), audit.ErrQuery) || buf.Len() != 0 { t.Fatal("Render wrote on invalid query") }
		if !errors.Is(audit.Export(&buf, input, q), audit.ErrQuery) || buf.Len() != 0 { t.Fatal("Export wrote on invalid query") }
		if _, err := audit.Summarize(input, q); !errors.Is(err, audit.ErrQuery) { t.Fatal("summary accepted invalid query") }
		if !slices.Equal(input, before) { t.Fatal("invalid query changed input") }
	}
	var buf bytes.Buffer
	if err := audit.Export(&buf, nil, audit.Query{}); err != nil || buf.String() != "[]\n" { t.Fatalf("empty export: %q, %v", buf.String(), err) }
}
