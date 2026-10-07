package fixture

import "testing"

func TestGetMarksRecent(t *testing.T) {
	c := New(2)
	c.Put("a", 1)
	c.Put("b", 2)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a missing")
	}
	c.Put("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Error("b should be evicted: it is the least recently used")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Errorf("Get(a) = %d, %v; want 1, true", v, ok)
	}
}

func TestPutReplaces(t *testing.T) {
	c := New(2)
	c.Put("a", 1)
	c.Put("a", 2)
	if c.Len() != 1 {
		t.Fatalf("Len() = %d after two puts of one key, want 1", c.Len())
	}
	if v, _ := c.Get("a"); v != 2 {
		t.Fatalf("Get(a) = %d, want 2", v)
	}
	c.Put("b", 3)
	c.Put("a", 4)
	c.Put("c", 5)
	if _, ok := c.Get("b"); ok {
		t.Error("b should be evicted after a was put again")
	}
	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2", c.Len())
	}
}
