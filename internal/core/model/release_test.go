package model

import (
	"strings"
	"testing"
)

func iv(n int) *int { return &n }

// The strictest value wins field by field: the shorter idle time, the shorter upper bound, and an upper bound over none (0).
func TestResolveReleaseMax(t *testing.T) {
	root := Node{ID: "root", ReleaseMax: &ReleaseMax{CPU: ReleaseTimes{IdleOpenMinutes: iv(120), IdleClosedMinutes: iv(120), MaxHours: iv(12)}, GPU: ReleaseTimes{IdleOpenMinutes: iv(30), MaxHours: iv(0)}}}
	mid := Node{ID: "b1"}
	low := Node{ID: "b2", ReleaseMax: &ReleaseMax{CPU: ReleaseTimes{IdleOpenMinutes: iv(60), MaxHours: iv(24)}, GPU: ReleaseTimes{IdleClosedMinutes: iv(5), MaxHours: iv(8)}}}
	got := ResolveReleaseMax([]Node{low, mid, root})
	if *got.CPU.IdleOpenMinutes != 60 || *got.CPU.IdleClosedMinutes != 120 || *got.CPU.MaxHours != 12 {
		t.Errorf("cpu = %+v", got.CPU)
	}
	if *got.GPU.IdleOpenMinutes != 30 || *got.GPU.IdleClosedMinutes != 5 || *got.GPU.MaxHours != 8 {
		t.Errorf("gpu = %+v (an upper bound is stricter than none)", got.GPU)
	}
	if ResolveReleaseMax([]Node{mid, {ID: "x", ReleaseMax: &ReleaseMax{}}}) != nil {
		t.Error("nothing set anywhere should resolve to nil")
	}
	// Resolving does not alias the stored values.
	*got.GPU.MaxHours = 99
	if *low.ReleaseMax.GPU.MaxHours != 8 {
		t.Error("the result shares memory with a node")
	}
}

func TestValidateReleaseMax(t *testing.T) {
	above := &ReleaseMax{CPU: ReleaseTimes{IdleOpenMinutes: iv(120), MaxHours: iv(12)}, GPU: ReleaseTimes{MaxHours: iv(0)}}
	for _, c := range []struct {
		name string
		r    *ReleaseMax
		up   *ReleaseMax
		want string
	}{
		{"nil", nil, above, ""},
		{"tighter", &ReleaseMax{CPU: ReleaseTimes{IdleOpenMinutes: iv(60), IdleClosedMinutes: iv(10), MaxHours: iv(8)}, GPU: ReleaseTimes{MaxHours: iv(24)}}, above, ""},
		{"gpu unlimited where above allows it", &ReleaseMax{GPU: ReleaseTimes{MaxHours: iv(0)}}, above, ""},
		{"gpu unlimited at the root", &ReleaseMax{GPU: ReleaseTimes{MaxHours: iv(0)}}, nil, ""},
		{"longer idle", &ReleaseMax{CPU: ReleaseTimes{IdleOpenMinutes: iv(121)}}, above, "at most 120 minutes"},
		{"longer max", &ReleaseMax{CPU: ReleaseTimes{MaxHours: iv(13)}}, above, "at most 12 hours"},
		{"cpu unlimited", &ReleaseMax{CPU: ReleaseTimes{MaxHours: iv(0)}}, nil, "upper bound is required"},
		{"gpu unlimited under a bound", &ReleaseMax{GPU: ReleaseTimes{MaxHours: iv(0)}}, &ReleaseMax{GPU: ReleaseTimes{MaxHours: iv(4)}}, "cannot allow none"},
		{"idle zero", &ReleaseMax{GPU: ReleaseTimes{IdleClosedMinutes: iv(0)}}, nil, "between 1 and 1440"},
		{"idle too long", &ReleaseMax{GPU: ReleaseTimes{IdleOpenMinutes: iv(1441)}}, nil, "between 1 and 1440"},
		{"max too long", &ReleaseMax{GPU: ReleaseTimes{MaxHours: iv(721)}}, nil, "between 1 and 720"},
		{"max negative", &ReleaseMax{GPU: ReleaseTimes{MaxHours: iv(-1)}}, nil, "between 1 and 720"},
	} {
		err := ValidateReleaseMax(c.r, c.up)
		if c.want == "" && err != nil {
			t.Errorf("%s: unexpected %v", c.name, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
}

func TestReleaseMaxNormalized(t *testing.T) {
	if (&ReleaseMax{}).Normalized() != nil {
		t.Error("an empty set should be stored as nil")
	}
	if r := (&ReleaseMax{GPU: ReleaseTimes{MaxHours: iv(4)}}).Normalized(); r == nil || *r.GPU.MaxHours != 4 {
		t.Errorf("normalized = %+v", r)
	}
}
