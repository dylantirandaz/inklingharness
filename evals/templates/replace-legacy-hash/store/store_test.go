package store

import "testing"

func TestPutUsesSHA256(t *testing.T) {
	s := New()
	key := s.Put([]byte("hello"))
	if want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"; key != want {
		t.Fatalf("key = %s, want %s", key, want)
	}
	if data, ok := s.Get(key); !ok || string(data) != "hello" {
		t.Fatalf("Get = %q, %v", data, ok)
	}
}
