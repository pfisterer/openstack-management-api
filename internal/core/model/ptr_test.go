package model

import "testing"

func TestMinPtrAndClonePtr(t *testing.T) {
	one, two := 1, 2
	for _, c := range []struct {
		a, b *int
		want *int
	}{{nil, nil, nil}, {&one, nil, &one}, {nil, &two, &two}, {&one, &two, &one}, {&two, &one, &one}} {
		got := MinPtr(c.a, c.b)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("MinPtr(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got != nil && (got == c.a || got == c.b) {
			t.Errorf("MinPtr must return a copy")
		}
	}
	if ClonePtr[int](nil) != nil {
		t.Error("ClonePtr(nil) must be nil")
	}
	if c := ClonePtr(&one); c == &one || *c != 1 {
		t.Error("ClonePtr must copy")
	}
}
