package inventory_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	inventory "fixture"
)

func seeded(t *testing.T) *inventory.Store {
	t.Helper()
	s, err := inventory.New(map[string]inventory.Item{"a": {Count: 4, Labels: []string{"red"}}, "b": {Count: 2}})
	if err != nil { t.Fatal(err) }
	return s
}

func TestEveryInvalidTailRollsBack(t *testing.T) {
	cases := []struct { change inventory.Change; err error }{
		{inventory.Change{Kind: "set", Key: "", Item: inventory.Item{Count: 1}}, inventory.ErrInvalid},
		{inventory.Change{Kind: "set", Key: "c", Item: inventory.Item{Count: -1}}, inventory.ErrInvalid},
		{inventory.Change{Kind: "unknown", Key: "a"}, inventory.ErrInvalid},
		{inventory.Change{Kind: "add", Key: "missing", Delta: 1}, inventory.ErrMissing},
		{inventory.Change{Kind: "delete", Key: "missing"}, inventory.ErrMissing},
		{inventory.Change{Kind: "add", Key: "a", Delta: -9}, inventory.ErrInvalid},
		{inventory.Change{Kind: "add", Key: "a", Delta: math.MaxInt64}, inventory.ErrInvalid},
		{inventory.Change{Kind: "add", Key: "a", Delta: math.MinInt64}, inventory.ErrInvalid},
	}
	for _, tc := range cases {
		s := seeded(t)
		before := s.Snapshot()
		changes := []inventory.Change{
			{Kind: "set", Key: "a", Item: inventory.Item{Count: 8, Labels: []string{"blue"}}},
			{Kind: "delete", Key: "b"},
			{Kind: "set", Key: "new", Item: inventory.Item{Count: 1}},
			tc.change,
		}
		if err := s.Apply(changes); !errors.Is(err, tc.err) { t.Fatalf("%+v: %v", tc.change, err) }
		if got := s.Snapshot(); !reflect.DeepEqual(got, before) { t.Fatalf("failed batch changed state: %#v", got) }
		changes[0].Item.Labels[0] = "outside"
		if !reflect.DeepEqual(s.Snapshot(), before) { t.Fatal("failed input leaked") }
	}
}

func TestOrderedChangesAndIntegerBounds(t *testing.T) {
	s := seeded(t)
	changes := []inventory.Change{
		{Kind: "delete", Key: "a"},
		{Kind: "set", Key: "a", Item: inventory.Item{Count: math.MaxInt64}},
		{Kind: "add", Key: "a", Delta: -math.MaxInt64},
		{Kind: "add", Key: "a", Delta: math.MaxInt64},
		{Kind: "add", Key: "a", Delta: 0},
		{Kind: "set", Key: "c", Item: inventory.Item{Count: 0}},
		{Kind: "delete", Key: "c"},
	}
	copyOfChanges := append([]inventory.Change(nil), changes...)
	if err := s.Apply(changes); err != nil { t.Fatal(err) }
	if !reflect.DeepEqual(changes, copyOfChanges) { t.Fatal("Apply changed input") }
	want := map[string]inventory.Item{"a": {Count: math.MaxInt64}, "b": {Count: 2}}
	if got := s.Snapshot(); !reflect.DeepEqual(got, want) { t.Fatalf("ordered batch: %#v", got) }
	if err := s.Apply(nil); err != nil { t.Fatal(err) }
	if !reflect.DeepEqual(s.Snapshot(), want) { t.Fatal("empty batch changed state") }
	if err := s.Apply([]inventory.Change{{Kind: "delete", Key: "a"}, {Kind: "add", Key: "a"}}); !errors.Is(err, inventory.ErrMissing) {
		t.Fatalf("add after delete: %v", err)
	}
	if !reflect.DeepEqual(s.Snapshot(), want) { t.Fatal("dependent error was not atomic") }
}

func TestNoMutableAliasesAtAnyBoundary(t *testing.T) {
	seed := map[string]inventory.Item{"a": {Count: 4, Labels: []string{"seed"}}}
	s, err := inventory.New(seed)
	if err != nil { t.Fatal(err) }
	seed["a"].Labels[0] = "changed"
	delete(seed, "a")
	if got := s.Snapshot()["a"]; got.Count != 4 || got.Labels[0] != "seed" { t.Fatalf("seed alias: %+v", got) }
	changes := []inventory.Change{{Kind: "set", Key: "b", Item: inventory.Item{Count: 3, Labels: []string{"set"}}}}
	if err := s.Apply(changes); err != nil { t.Fatal(err) }
	if changes[0].Item.Labels[0] != "set" { t.Fatal("input changed") }
	changes[0].Item.Labels[0] = "changed"
	if got := s.Snapshot()["b"].Labels[0]; got != "set" { t.Fatalf("batch alias: %q", got) }
	first := s.Snapshot()
	second := s.Snapshot()
	first["b"].Labels[0] = "snapshot"
	delete(first, "a")
	if second["b"].Labels[0] != "set" || s.Snapshot()["b"].Labels[0] != "set" { t.Fatal("snapshot alias") }
	if _, ok := s.Snapshot()["a"]; !ok { t.Fatal("snapshot map alias") }
}

func TestSeedValidationAndEmptyStore(t *testing.T) {
	for _, seed := range []map[string]inventory.Item{{"": {Count: 1}}, {"x": {Count: -1}}} {
		if s, err := inventory.New(seed); s != nil || !errors.Is(err, inventory.ErrInvalid) { t.Fatalf("invalid seed: %v, %v", s, err) }
	}
	s, err := inventory.New(nil)
	if err != nil { t.Fatal(err) }
	if err := s.Apply([]inventory.Change{{Kind: "set", Key: "x", Item: inventory.Item{Labels: []string{}}}}); err != nil { t.Fatal(err) }
	if labels := s.Snapshot()["x"].Labels; labels == nil || len(labels) != 0 { t.Fatalf("empty labels not preserved: %#v", labels) }
}
