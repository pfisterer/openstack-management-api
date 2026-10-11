package osclient

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// A catalogue is deployment configuration now, so these are the errors an
// operator will meet at startup. Each case is one way a resource can look
// finished and do nothing.

func countRes(id string) ManagedResource {
	return ManagedResource{ID: id, Name: id}
}

func boolRes(id, grantType, target string) ManagedResource {
	return ManagedResource{
		ID: id, Name: id, Kind: common.KindBool,
		Grant: &Grant{Type: grantType, Target: target},
	}
}

func TestValidateManagedResources_AcceptsAMixedCatalogue(t *testing.T) {
	defs := []ManagedResource{
		countRes("cores"),
		{ID: "ram", Name: "RAM", Kind: common.KindCount, OSQuotaField: "ram"},
		boolRes("dhbw-ipv4", GrantNetwork, "8f2c-net"),
		boolRes("gpu-rtx6000-mannheim", GrantFlavor, "f-17"),
	}

	if err := ValidateManagedResources(defs); err != nil {
		t.Fatalf("valid catalogue rejected: %v", err)
	}
}

// An empty kind has to keep meaning "count" — a catalogue written before kinds
// existed must not start failing.
func TestValidateManagedResources_EmptyKindIsACount(t *testing.T) {
	d := countRes("cores")

	if !d.IsCount() || d.IsBool() {
		t.Fatalf("a definition without a kind must count, got kind %q", d.Kind)
	}
}

func TestValidateManagedResources_Rejects(t *testing.T) {
	cases := []struct {
		name string
		defs []ManagedResource
	}{
		{
			// Quota maps are keyed by id: the second entry would silently govern
			// the first one's stored values.
			"a duplicate id",
			[]ManagedResource{countRes("cores"), countRes("cores")},
		},
		{
			"an unknown kind",
			[]ManagedResource{{ID: "cores", Name: "Cores", Kind: "conut"}},
		},
		{
			// Would be grantable in the portal and mean nothing in OpenStack.
			"an availability without a grant",
			[]ManagedResource{{ID: "dhbw-ipv4", Name: "IPv4", Kind: common.KindBool}},
		},
		{
			"an availability with an unknown grant type",
			[]ManagedResource{boolRes("dhbw-ipv4", "subnet", "8f2c-net")},
		},
		{
			"a grant without a target",
			[]ManagedResource{boolRes("dhbw-ipv4", GrantNetwork, "")},
		},
		{
			// The reconciler would write it as a number: 1 core, 1 gigabyte.
			"an availability that also claims a quota field",
			[]ManagedResource{{
				ID: "dhbw-ipv4", Name: "IPv4", Kind: common.KindBool,
				Grant:        &Grant{Type: GrantNetwork, Target: "8f2c-net"},
				OSQuotaField: "networks",
			}},
		},
		{
			"a grant on a quantity",
			[]ManagedResource{{
				ID: "cores", Name: "Cores",
				Grant: &Grant{Type: GrantFlavor, Target: "f-17"},
			}},
		},
		{
			"an empty catalogue",
			[]ManagedResource{},
		},
		{
			"a resource without an id",
			[]ManagedResource{{Name: "Nameless"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateManagedResources(tc.defs); err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
		})
	}
}

// A half-written mapping is what a hand-authored catalogue produces, and it does
// nothing loudly: the resource is never written to OpenStack, and an ignored
// overcommit flag sends the accounting back to billing declared limits.
func TestValidateManagedResources_RejectsAHalfWrittenMapping(t *testing.T) {
	cases := map[string]ManagedResource{
		"a multiplier with nothing to convert for": {ID: "ram", Name: "RAM", OSMultiplier: 1024},
		"measuring a field that is not named":      {ID: "ram", Name: "RAM", OSOvercommitCheck: true},
		"mirroring into nothing":                   {ID: "cores", Name: "Cores", OSLinkedField: "instances"},
	}

	for name, def := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateManagedResources([]ManagedResource{def}); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}

	// A resource with no OpenStack side at all stays valid: GPUs have no quota
	// field, and refusing that would make the built-in catalogue invalid.
	if err := ValidateManagedResources([]ManagedResource{{ID: "gpu", Name: "GPUs"}}); err != nil {
		t.Errorf("a resource without any mapping must remain valid: %v", err)
	}
}

// The tree is told what a resource means in the books, not which OpenStack
// field it is.
func TestCore_Accounts(t *testing.T) {
	cases := map[string]struct {
		in      ManagedResource
		want    string
		perGB   float64
		grantTy string
	}{
		"cores":        {ManagedResource{ID: "c", OSQuotaField: "cores"}, common.AccountsCores, 0, ""},
		"ram in GB":    {ManagedResource{ID: "r", OSQuotaField: "ram", OSMultiplier: 1024}, common.AccountsRAM, 1, ""},
		"ram in MB":    {ManagedResource{ID: "r", OSQuotaField: "ram"}, common.AccountsRAM, 1.0 / 1024, ""},
		"storage":      {ManagedResource{ID: "s", OSQuotaField: "gigabytes"}, common.AccountsStorage, 0, ""},
		"static ports": {ManagedResource{ID: "p", OSQuotaField: "ports", Static: true}, "", 0, ""},
		"network":      {boolRes("n", GrantNetwork, "net-1"), "", 0, GrantNetwork},
	}
	for name, tc := range cases {
		got := tc.in.Core()
		if got.Accounts != tc.want || got.AccountsPerGB != tc.perGB || got.GrantType != tc.grantTy || got.ID != tc.in.ID {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}
