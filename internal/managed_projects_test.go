package app

import "testing"

// Whether a resource is MEASURED in OpenStack is decided by one flag on its
// definition, and forgetting that flag fails silently: ProjectInUse skips the
// resource, it never reaches OSInUse, and the accounting goes on billing the
// declared limit as if nothing were running.
//
// That is exactly how storage stayed exploitable after the max(limit, in-use)
// rule was already in place — the rule was correct, the input was missing. The
// attack it leaves open: request 500 GB, fill it with volumes, shrink to 5.
// OpenStack accepts the smaller quota and keeps the volumes; the books say 5.
//
// So pin the measured set against the shipped defaults rather than trusting a
// reviewer to notice an absent field on a new resource.
func TestDefaultResources_MeasureEverythingOpenStackCounts(t *testing.T) {
	measured := map[string]bool{}
	fields := map[string]string{}
	for _, r := range defaultResourceCatalogue() {
		fields[r.ID] = r.OSQuotaField
		if r.OSOvercommitCheck && r.OSQuotaField != "" {
			measured[r.ID] = true
		}
	}

	for _, id := range []string{"cores", "ram", "storage"} {
		if !measured[id] {
			t.Errorf("%q maps to the OpenStack quota field %q but is not measured — "+
				"the shrink-after-filling loophole stays open for it", id, fields[id])
		}
	}

	// The other direction: claiming to measure something OpenStack cannot count
	// would invent a number. GPUs have no quota field at all.
	if measured["gpu"] {
		t.Error("gpu has no OpenStack quota field; measuring it would invent a number")
	}
}
