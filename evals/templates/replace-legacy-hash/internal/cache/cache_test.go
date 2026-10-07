package cache

import "testing"

func TestKeyIsSHA256(t *testing.T) {
	if got := Key("GET", "/", 1); len(got) != 64 {
		t.Fatalf("Key = %q has length %d, want 64 (SHA-256 hex)", got, len(got))
	}
	if Key("GET", "/", 1) == Key("GET", "/", 2) {
		t.Fatal("different versions gave the same key")
	}
}
