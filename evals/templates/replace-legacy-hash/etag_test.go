package fixture

import "testing"

func TestETag(t *testing.T) {
	if got, want := ETag([]byte("hello")), `"2cf24dba5fb0a30e"`; got != want {
		t.Fatalf("ETag = %s, want %s", got, want)
	}
}
