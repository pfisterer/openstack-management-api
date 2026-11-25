package model

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Several paying budgets give the project the most generous of their values (decided 25.11.2026); a permission holds where any grants it. The rule is switchable in one place.
func TestMergeCaps(t *testing.T) {
	defs := []common.Cap{{ID: "jobs", Min: 1, Max: 10}, {ID: "gpu", Grant: true, Min: 1, Max: 8}}
	a := common.Caps{"jobs": 2}
	b := common.Caps{"jobs": 5, "gpu": 4}
	if got := MergeCaps([]common.Caps{a, b}, defs); got["jobs"] != 5 || got["gpu"] != 4 {
		t.Errorf("loosest = %v", got)
	}
	CapMerge = CapMergeStrictest
	defer func() { CapMerge = CapMergeLoosest }()
	if got := MergeCaps([]common.Caps{a, b}, defs); got["jobs"] != 2 || got["gpu"] != 0 {
		t.Errorf("strictest = %v", got)
	}
}
