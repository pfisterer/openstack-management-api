package model

import "testing"

func TestResolveBatchJobs(t *testing.T) {
	on := func(n, h int) *BatchJobs { return &BatchJobs{JobsPerPerson: n, MaxHours: h} }
	for _, tc := range []struct {
		name  string
		chain []Node
		want  *BatchJobs
	}{
		{"empty chain", nil, nil},
		{"root without", []Node{{}}, nil},
		{"root only", []Node{{BatchJobs: on(4, 24)}}, on(4, 24)},
		// Each bound on its own: the fewer jobs of one and the shorter run time of the other.
		{"several", []Node{{BatchJobs: on(2, 24)}, {BatchJobs: on(5, 4)}}, on(2, 4)},
		// A budget without them has none, wherever it is in the chain.
		{"one without", []Node{{BatchJobs: on(5, 4)}, {}}, nil},
		{"one without, other order", []Node{{}, {BatchJobs: on(5, 4)}}, nil},
		// A budget that inherits its limit passes on those above, whatever it holds.
		{"inheriting passes on", []Node{{BatchJobs: on(3, 8)}, {InheritsLimit: true}}, on(3, 8)},
		{"inheriting ignores its own", []Node{{BatchJobs: on(3, 8)}, {InheritsLimit: true, BatchJobs: on(1, 1)}}, on(3, 8)},
	} {
		got := ResolveBatchJobs(tc.chain)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestValidateBatchJobs(t *testing.T) {
	on := func(n, h int) *BatchJobs { return &BatchJobs{JobsPerPerson: n, MaxHours: h} }
	for _, tc := range []struct {
		name     string
		v, above *BatchJobs
		root     bool
		ok       bool
	}{
		{"none", nil, nil, false, true},
		{"root sets the most", on(500, MaxJobHours), nil, true, true},
		{"root: no jobs", on(0, 4), nil, true, false},
		{"root: no run time", on(2, 0), nil, true, false},
		{"root: too long", on(2, MaxJobHours+1), nil, true, false},
		{"below without above", on(1, 1), nil, false, false},
		{"tighter", on(1, 2), on(2, 4), false, true},
		{"same", on(2, 4), on(2, 4), false, true},
		{"more jobs", on(3, 4), on(2, 4), false, false},
		{"longer", on(2, 8), on(2, 4), false, false},
	} {
		if err := ValidateBatchJobs(tc.v, tc.above, tc.root); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}
