package common

import "testing"

// A catalogue is deployment configuration now, so these are the errors an
// operator will meet at startup. Each case is one way a resource can look
// finished and do nothing.

func countRes(id string) ManagedProject {
	return ManagedProject{ID: id, Name: id}
}

func boolRes(id, grantType, target string) ManagedProject {
	return ManagedProject{
		ID: id, Name: id, Kind: KindBool,
		Grant: &Grant{Type: grantType, Target: target},
	}
}

func TestValidateManagedProjects_AcceptsAMixedCatalogue(t *testing.T) {
	defs := []ManagedProject{
		countRes("cores"),
		{ID: "ram", Name: "RAM", Kind: KindCount, OSQuotaField: "ram"},
		boolRes("dhbw-ipv4", GrantNetwork, "8f2c-net"),
		boolRes("gpu-rtx6000-mannheim", GrantFlavor, "f-17"),
	}

	if err := ValidateManagedProjects(defs); err != nil {
		t.Fatalf("valid catalogue rejected: %v", err)
	}
}

// An empty kind has to keep meaning "count" — a catalogue written before kinds
// existed must not start failing.
func TestValidateManagedProjects_EmptyKindIsACount(t *testing.T) {
	d := countRes("cores")

	if !d.IsCount() || d.IsBool() {
		t.Fatalf("a definition without a kind must count, got kind %q", d.Kind)
	}
}

func TestValidateManagedProjects_Rejects(t *testing.T) {
	cases := []struct {
		name string
		defs []ManagedProject
	}{
		{
			// Quota maps are keyed by id: the second entry would silently govern
			// the first one's stored values.
			"a duplicate id",
			[]ManagedProject{countRes("cores"), countRes("cores")},
		},
		{
			"an unknown kind",
			[]ManagedProject{{ID: "cores", Name: "Cores", Kind: "conut"}},
		},
		{
			// Reserved, not built. Accepting it would hand out a resource that
			// takes part in no arithmetic at all.
			"the reserved hours kind",
			[]ManagedProject{{ID: "gpu-hours", Name: "GPU hours", Kind: KindHours}},
		},
		{
			// Would be grantable in the portal and mean nothing in OpenStack.
			"an availability without a grant",
			[]ManagedProject{{ID: "dhbw-ipv4", Name: "IPv4", Kind: KindBool}},
		},
		{
			"an availability with an unknown grant type",
			[]ManagedProject{boolRes("dhbw-ipv4", "subnet", "8f2c-net")},
		},
		{
			"a grant without a target",
			[]ManagedProject{boolRes("dhbw-ipv4", GrantNetwork, "")},
		},
		{
			// The reconciler would write it as a number: 1 core, 1 gigabyte.
			"an availability that also claims a quota field",
			[]ManagedProject{{
				ID: "dhbw-ipv4", Name: "IPv4", Kind: KindBool,
				Grant:        &Grant{Type: GrantNetwork, Target: "8f2c-net"},
				OSQuotaField: "networks",
			}},
		},
		{
			"a grant on a quantity",
			[]ManagedProject{{
				ID: "cores", Name: "Cores",
				Grant: &Grant{Type: GrantFlavor, Target: "f-17"},
			}},
		},
		{
			"an empty catalogue",
			[]ManagedProject{},
		},
		{
			"a resource without an id",
			[]ManagedProject{{Name: "Nameless"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateManagedProjects(tc.defs); err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
		})
	}
}

// A half-written mapping is what a hand-authored catalogue produces, and it does
// nothing loudly: the resource is never written to OpenStack, and an ignored
// overcommit flag sends the accounting back to billing declared limits.
func TestValidateManagedProjects_RejectsAHalfWrittenMapping(t *testing.T) {
	cases := map[string]ManagedProject{
		"a multiplier with nothing to convert for": {ID: "ram", Name: "RAM", OSMultiplier: 1024},
		"measuring a field that is not named":      {ID: "ram", Name: "RAM", OSOvercommitCheck: true},
		"mirroring into nothing":                   {ID: "cores", Name: "Cores", OSLinkedField: "instances"},
	}

	for name, def := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateManagedProjects([]ManagedProject{def}); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}

	// A resource with no OpenStack side at all stays valid: GPUs have no quota
	// field, and refusing that would make the built-in catalogue invalid.
	if err := ValidateManagedProjects([]ManagedProject{{ID: "gpu", Name: "GPUs"}}); err != nil {
		t.Errorf("a resource without any mapping must remain valid: %v", err)
	}
}
