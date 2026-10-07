package fixture

import "testing"

func TestStack(t *testing.T) {
	var s Stack[string]
	if _, ok := s.Pop(); ok {
		t.Fatal("Pop on empty stack returned ok")
	}
	if v, ok := s.Peek(); ok || v != "" {
		t.Fatalf("Peek on empty stack = %q, %v", v, ok)
	}
	s.Push("a")
	s.Push("b")
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	if v, ok := s.Peek(); !ok || v != "b" || s.Len() != 2 {
		t.Fatalf("Peek = %q, %v, Len %d", v, ok, s.Len())
	}
	for _, want := range []string{"b", "a"} {
		if v, ok := s.Pop(); !ok || v != want {
			t.Fatalf("Pop = %q, %v; want %q", v, ok, want)
		}
	}
	if s.Len() != 0 {
		t.Fatalf("Len after pops = %d", s.Len())
	}
	var numbers Stack[int]
	numbers.Push(7)
	if v, ok := numbers.Pop(); !ok || v != 7 {
		t.Fatalf("int Pop = %d, %v", v, ok)
	}
}
