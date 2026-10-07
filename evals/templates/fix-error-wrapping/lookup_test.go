package fixture

import (
	"errors"
	"testing"
)

func TestLookup(t *testing.T) {
	s := NewStore(map[string]string{"a": "1"})
	if got, err := Lookup(s, "a"); err != nil || got != "1" {
		t.Fatalf("Lookup(a) = %q, %v", got, err)
	}
	_, err := Lookup(s, "b")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(%v, ErrNotFound) = false", err)
	}
	if want := `lookup "b": not found`; err.Error() != want {
		t.Fatalf("error text = %q, want %q", err.Error(), want)
	}
}

func TestDescribe(t *testing.T) {
	s := NewStore(map[string]string{"a": "1"})
	cases := map[string]string{"a": "a = 1", "b": "missing: b"}
	for key, want := range cases {
		if got := Describe(s, key); got != want {
			t.Errorf("Describe(%q) = %q, want %q", key, got, want)
		}
	}
}
