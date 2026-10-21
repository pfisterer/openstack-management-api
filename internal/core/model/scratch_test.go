package model

import "testing"

func TestResolveWorkspaceScratchGB(t *testing.T) {
	iv := func(n int) *int { return &n }
	if got := ResolveWorkspaceScratchGB(nil); got != nil {
		t.Errorf("empty chain = %v, want nil", *got)
	}
	if got := ResolveWorkspaceScratchGB([]Node{{}, {WorkspaceScratchGB: iv(20)}, {}}); got == nil || *got != 20 {
		t.Errorf("one value = %v, want 20", got)
	}
	if got := ResolveWorkspaceScratchGB([]Node{{WorkspaceScratchGB: iv(5)}, {WorkspaceScratchGB: iv(20)}, {WorkspaceScratchGB: iv(10)}}); got == nil || *got != 5 {
		t.Errorf("several = %v, want the smallest, 5", got)
	}
}

func TestValidateWorkspaceScratchGB(t *testing.T) {
	iv := func(n int) *int { return &n }
	for _, tc := range []struct {
		v, above *int
		ok       bool
	}{
		{nil, iv(1), true},
		{iv(1), nil, true},
		{iv(MaxWorkspaceScratchGB), nil, true},
		{iv(0), nil, false},
		{iv(-1), nil, false},
		{iv(MaxWorkspaceScratchGB + 1), nil, false},
		{iv(10), iv(10), true},
		{iv(5), iv(10), true},
		{iv(11), iv(10), false},
	} {
		err := ValidateWorkspaceScratchGB(tc.v, tc.above)
		if (err == nil) != tc.ok {
			t.Errorf("Validate(%v, %v) = %v, want ok=%v", deref(tc.v), deref(tc.above), err, tc.ok)
		}
	}
}

func deref(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
