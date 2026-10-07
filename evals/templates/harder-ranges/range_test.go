package ranges_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	ranges "fixture"
)

func equal(a, b []ranges.Range) bool { return slices.Equal(a, b) }

func TestNormalizationCases(t *testing.T) {
	minInt, maxInt := -int(^uint(0)>>1)-1, int(^uint(0)>>1)
	cases := []struct { name string; in, want []ranges.Range }{
		{"empty", nil, nil},
		{"empty endpoints", []ranges.Range{{0, 0}, {maxInt, maxInt}, {minInt, minInt}}, nil},
		{"adjacent", []ranges.Range{{4, 8}, {0, 4}}, []ranges.Range{{0, 8}}},
		{"unit gap", []ranges.Range{{0, 1}, {2, 3}}, []ranges.Range{{0, 1}, {2, 3}}},
		{"nested", []ranges.Range{{0, 20}, {2, 8}, {9, 12}}, []ranges.Range{{0, 20}}},
		{"same start", []ranges.Range{{1, 9}, {1, 4}, {1, 9}}, []ranges.Range{{1, 9}}},
		{"negative", []ranges.Range{{-2, 1}, {-9, -4}, {-4, -2}}, []ranges.Range{{-9, 1}}},
		{"bounds", []ranges.Range{{maxInt-1, maxInt}, {minInt, minInt+1}}, []ranges.Range{{minInt, minInt+1}, {maxInt-1, maxInt}}},
		{"max nested", []ranges.Range{{0, maxInt}, {1, maxInt}}, []ranges.Range{{0, maxInt}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.in)
			got, err := ranges.Normalize(tc.in)
			if err != nil || !equal(got, tc.want) { t.Fatalf("Normalize = %v, %v; want %v", got, err, tc.want) }
			if !reflect.DeepEqual(tc.in, before) { t.Fatal("input changed") }
			if len(got) > 0 { got[0].Start = 100; if !reflect.DeepEqual(tc.in, before) { t.Fatal("output aliases input") } }
		})
	}
}

func TestReversedRangeDoesNotChangeInput(t *testing.T) {
	input := []ranges.Range{{8, 10}, {4, 3}, {-2, 1}}
	before := slices.Clone(input)
	if out, err := ranges.Normalize(input); out != nil || !errors.Is(err, ranges.ErrRange) { t.Fatalf("invalid: %v, %v", out, err) }
	if !reflect.DeepEqual(input, before) { t.Fatal("invalid input changed") }
	if _, err := ranges.Gaps(input, ranges.Range{0, 0}); !errors.Is(err, ranges.ErrRange) { t.Fatalf("empty window hides invalid input: %v", err) }
	if _, err := ranges.Gaps(nil, ranges.Range{2, 1}); !errors.Is(err, ranges.ErrRange) { t.Fatalf("reversed window: %v", err) }
}

func TestClippedGapsAtIntegerBounds(t *testing.T) {
	minInt, maxInt := -int(^uint(0)>>1)-1, int(^uint(0)>>1)
	input := []ranges.Range{{maxInt-2, maxInt}, {minInt, minInt+2}, {-3, 3}, {-1, 1}}
	before := slices.Clone(input)
	out, err := ranges.Gaps(input, ranges.Range{minInt+1, maxInt-1})
	want := []ranges.Range{{minInt+2, -3}, {3, maxInt-2}}
	if err != nil || !equal(out, want) { t.Fatalf("bounded gaps: %v, %v", out, err) }
	if !reflect.DeepEqual(input, before) { t.Fatal("Gaps changed input") }
	out, err = ranges.Gaps([]ranges.Range{{-100, 100}}, ranges.Range{-2, 2})
	if err != nil || len(out) != 0 { t.Fatalf("covered window: %v, %v", out, err) }
	out, err = ranges.Gaps(nil, ranges.Range{3, 3})
	if err != nil || len(out) != 0 { t.Fatalf("empty window: %v, %v", out, err) }
}

func contains(rs []ranges.Range, x int) bool {
	for _, r := range rs { if r.Start <= x && x < r.End { return true } }
	return false
}

func TestSmallDomainUnionAndComplement(t *testing.T) {
	var domain []ranges.Range
	for a := -3; a <= 3; a++ {
		for b := a; b <= 3; b++ { domain = append(domain, ranges.Range{a, b}) }
	}
	for _, a := range domain {
		for _, b := range domain {
			input := []ranges.Range{b, a, a}
			out, err := ranges.Normalize(input)
			if err != nil { t.Fatal(err) }
			for i, r := range out {
				if r.Start >= r.End || (i > 0 && out[i-1].End >= r.Start) { t.Fatalf("not canonical: %v", out) }
			}
			gaps, err := ranges.Gaps(input, ranges.Range{-2, 2})
			if err != nil { t.Fatal(err) }
			for x := -4; x <= 4; x++ {
				covered := contains([]ranges.Range{a, b}, x)
				if contains(out, x) != covered { t.Fatalf("union %v at %d: %v", input, x, out) }
				if contains(gaps, x) != (x >= -2 && x < 2 && !covered) { t.Fatalf("gaps %v at %d: %v", input, x, gaps) }
			}
		}
	}
}
