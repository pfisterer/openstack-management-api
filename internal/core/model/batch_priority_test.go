package model

import "testing"

func TestResolveBatchPriority(t *testing.T) {
	sv := func(s string) *string { return &s }
	if got := ResolveBatchPriority(nil); got != nil {
		t.Errorf("empty chain = %v, want nil", *got)
	}
	if got := ResolveBatchPriority([]Node{{}, {BatchPriority: sv(BatchPriorityGuaranteed)}, {}}); got == nil || *got != BatchPriorityGuaranteed {
		t.Errorf("one value = %v, want guaranteed", got)
	}
	if got := ResolveBatchPriority([]Node{{BatchPriority: sv(BatchPriorityGuaranteed)}, {BatchPriority: sv(BatchPriorityPreemptible)}, {BatchPriority: sv(BatchPriorityGuaranteed)}}); got == nil || *got != BatchPriorityPreemptible {
		t.Errorf("several = %v, want the strictest, preemptible", got)
	}
	if got := ResolveBatchPriority([]Node{{BatchPriority: sv(BatchPriorityPreemptible)}, {BatchPriority: sv(BatchPriorityGuaranteed)}}); got == nil || *got != BatchPriorityPreemptible {
		t.Errorf("order must not matter, got %v", got)
	}
}

func TestValidateBatchPriority(t *testing.T) {
	sv := func(s string) *string { return &s }
	g, p := sv(BatchPriorityGuaranteed), sv(BatchPriorityPreemptible)
	for _, tc := range []struct {
		v, above *string
		ok       bool
	}{
		{nil, p, true},
		{g, nil, true},
		{p, nil, true},
		{sv(""), nil, false},
		{sv("Guaranteed"), nil, false},
		{sv("high"), nil, false},
		{g, g, true},
		{p, g, true},
		{p, p, true},
		{g, p, false},
	} {
		err := ValidateBatchPriority(tc.v, tc.above)
		if (err == nil) != tc.ok {
			t.Errorf("Validate(%v, %v) = %v, want ok=%v", derefString(tc.v), derefString(tc.above), err, tc.ok)
		}
	}
}

func derefString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
