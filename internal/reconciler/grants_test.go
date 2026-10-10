package reconciler

import (
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// What the reconciler does with availabilities, without a cloud to do it in.
// The interesting cases are the ones with a blast radius: touching a target the
// catalogue does not name, and writing anything at all during a dry run.

type fakeGrants struct {
	held    map[string]bool // "type/target/project" -> granted
	added   []common.Grant
	removed []common.Grant
	reads   []common.Grant
	failAdd bool
}

func newFakeGrants() *fakeGrants { return &fakeGrants{held: map[string]bool{}} }

func key(g common.Grant, project string) string { return g.Type + "/" + g.Target + "/" + project }

func (f *fakeGrants) HasGrant(g common.Grant, project string) (bool, error) {
	f.reads = append(f.reads, g)
	return f.held[key(g, project)], nil
}

func (f *fakeGrants) AddGrant(g common.Grant, project string) (bool, error) {
	if f.failAdd {
		return false, errors.New("nope")
	}
	f.added = append(f.added, g)
	already := f.held[key(g, project)]
	f.held[key(g, project)] = true
	return !already, nil
}

func (f *fakeGrants) RemoveGrant(g common.Grant, project string) (bool, error) {
	f.removed = append(f.removed, g)
	had := f.held[key(g, project)]
	delete(f.held, key(g, project))
	return had, nil
}

var (
	netGrant    = common.Grant{Type: common.GrantNetwork, Target: "net-1"}
	flavorGrant = common.Grant{Type: common.GrantFlavor, Target: "flavor-1"}

	grantCatalogue = []common.ManagedProject{
		{ID: "cores", Name: "Cores"},
		{ID: "dhbw-ipv4", Name: "IPv4", Kind: common.KindBool, Grant: &netGrant},
		{ID: "gpu-rtx6000", Name: "RTX 6000", Kind: common.KindBool, Grant: &flavorGrant},
	}
)

func leafWithLimit(limit common.ProjectQuota) tree.Node {
	return tree.Node{ID: "p_1", Limit: limit}
}

func TestSyncGrants_GrantsWhatTheLeafHoldsAndRevokesTheRest(t *testing.T) {
	f := newFakeGrants()
	leaf := leafWithLimit(common.ProjectQuota{"cores": 4, "dhbw-ipv4": 1, "gpu-rtx6000": 0})

	syncGrants(f, grantCatalogue, leaf, "os-project", false, zap.NewNop().Sugar())

	if len(f.added) != 1 || f.added[0] != netGrant {
		t.Errorf("added = %v, want just the network", f.added)
	}
	if len(f.removed) != 1 || f.removed[0] != flavorGrant {
		t.Errorf("removed = %v, want just the flavour", f.removed)
	}
}

// The safety property. Flavour access and image members carry no marker saying
// who created them, so a target outside the catalogue can never be distinguished
// from an operator's own grant — it must not be read or written at all.
func TestSyncGrants_NeverTouchesATargetOutsideTheCatalogue(t *testing.T) {
	f := newFakeGrants()
	// The leaf carries an availability the catalogue has since dropped.
	leaf := leafWithLimit(common.ProjectQuota{"dhbw-ipv4": 1, "retired-image": 1})

	syncGrants(f, grantCatalogue, leaf, "os-project", false, zap.NewNop().Sugar())

	for _, g := range append(append([]common.Grant{}, f.added...), f.removed...) {
		if g != netGrant && g != flavorGrant {
			t.Fatalf("touched %v, which the catalogue does not name", g)
		}
	}
}

// A quantity has a quota, not a grant. Reading one as an availability would set
// "granted" from a core count.
func TestSyncGrants_IgnoresQuantities(t *testing.T) {
	f := newFakeGrants()
	leaf := leafWithLimit(common.ProjectQuota{"cores": 1})

	syncGrants(f, grantCatalogue, leaf, "os-project", false, zap.NewNop().Sugar())

	for _, g := range f.added {
		if g.Target == "cores" {
			t.Fatal("a quantity was granted as an availability")
		}
	}
	if len(f.added) != 0 {
		t.Errorf("nothing was granted, yet added = %v", f.added)
	}
}

// The first sharp run on production is preceded by a dry run, and it is only
// worth anything if the dry run changes nothing.
func TestSyncGrants_DryRunReadsAndWritesNothing(t *testing.T) {
	f := newFakeGrants()
	leaf := leafWithLimit(common.ProjectQuota{"dhbw-ipv4": 1, "gpu-rtx6000": 1})

	syncGrants(f, grantCatalogue, leaf, "os-project", true, zap.NewNop().Sugar())

	if len(f.added) != 0 || len(f.removed) != 0 {
		t.Fatalf("dry run wrote: added=%v removed=%v", f.added, f.removed)
	}
	if len(f.reads) != 2 {
		t.Errorf("dry run read %d grants, want the 2 availabilities", len(f.reads))
	}
}

// One failing resource must not stop the others: the next run tries again, and
// twenty other projects still need reconciling.
func TestSyncGrants_KeepsGoingAfterAFailure(t *testing.T) {
	f := newFakeGrants()
	f.failAdd = true
	leaf := leafWithLimit(common.ProjectQuota{"dhbw-ipv4": 1, "gpu-rtx6000": 0})

	syncGrants(f, grantCatalogue, leaf, "os-project", false, zap.NewNop().Sugar())

	if len(f.removed) != 1 {
		t.Fatalf("a failed grant stopped the revoke that followed it: removed=%v", f.removed)
	}
}

// Changing who may reach a network is not a detail, and the reconciler runs
// against every project every few minutes — so a real change has to be
// distinguishable from the twenty passes that changed nothing.
func TestSyncGrants_ReportsOnlyRealChanges(t *testing.T) {
	f := newFakeGrants()
	leaf := leafWithLimit(common.ProjectQuota{"dhbw-ipv4": 1})
	log := zap.NewNop().Sugar()

	// First pass grants it.
	syncGrants(f, grantCatalogue, leaf, "os-project", false, log)
	if !f.held[key(netGrant, "os-project")] {
		t.Fatal("the first pass did not grant it")
	}

	// Second pass, same desired state: the client must report no change, or the
	// log would claim a grant on every tick.
	changed, err := f.AddGrant(netGrant, "os-project")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if changed {
		t.Error("granting what is already granted reported a change")
	}

	// And removing what was never there is not a change either.
	gone, err := f.RemoveGrant(flavorGrant, "os-project")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if gone {
		t.Error("removing an absent grant reported a change")
	}
}

// An availability a project has by allocation from a budget further up is
// granted like one from its own limit: OpenStack sees what the project holds.
func TestSyncGrants_GrantsWhatAnAllocationHolds(t *testing.T) {
	f := newFakeGrants()
	leaf := leafWithLimit(common.ProjectQuota{"cores": 4})
	leaf.Allocations = []tree.Allocation{{BudgetID: "b_uni", Limit: common.ProjectQuota{"gpu-rtx6000": 1}}}

	syncGrants(f, grantCatalogue, leaf, "os-project", false, zap.NewNop().Sugar())

	if len(f.added) != 1 || f.added[0] != flavorGrant {
		t.Errorf("added = %v, want the flavour from the allocation", f.added)
	}
}

// An import carries the availabilities it already has, so adopting it does not
// start by revoking them; one that cannot be read keeps the last known value.
func TestImportGrants(t *testing.T) {
	f := newFakeGrants()
	f.held[key(netGrant, "os-1")] = true
	limit := common.ProjectQuota{"cores": 8}

	importGrants(f, grantCatalogue, "os-1", limit, nil, zap.NewNop().Sugar())

	if limit["dhbw-ipv4"] != 1 || limit["gpu-rtx6000"] != 0 || limit["cores"] != 8 {
		t.Errorf("limit = %v, want the network held, the flavour not, cores untouched", limit)
	}

	failing := &failingGrants{}
	limit = common.ProjectQuota{}
	importGrants(failing, grantCatalogue, "os-1", limit, common.ProjectQuota{"dhbw-ipv4": 1}, zap.NewNop().Sugar())
	if v, ok := limit["dhbw-ipv4"]; !ok || v != 1 {
		t.Errorf("unreadable grant: limit = %v, want the last known value kept", limit)
	}
	if _, ok := limit["gpu-rtx6000"]; ok {
		t.Errorf("unreadable grant without a last value must stay unset, got %v", limit)
	}
}

type failingGrants struct{ fakeGrants }

func (f *failingGrants) HasGrant(common.Grant, string) (bool, error) {
	return false, errors.New("unreachable")
}

// Every availability of the catalogue gets a figure, 0 for one nothing uses;
// use that could not be read gives nil, never a row of zeros.
func TestProjectGrantUse(t *testing.T) {
	use := osclient.GrantUse{"os-1": {"net-1": 2}, "os-2": {"flavor-1": 1}}

	got := projectGrantUse(grantCatalogue, use, "os-1")
	if len(got) != 2 || got["dhbw-ipv4"] != 2 || got["gpu-rtx6000"] != 0 {
		t.Errorf("got %v, want the network used twice, the flavour not at all", got)
	}
	if _, ok := got["cores"]; ok {
		t.Error("cores is no availability")
	}
	if projectGrantUse(grantCatalogue, nil, "os-1") != nil {
		t.Error("unread use must stay nil")
	}
}

// An unread use keeps the stored one; a read one replaces it and counts as a change.
func TestApplyOSSyncState_GrantUse(t *testing.T) {
	leaf := tree.Node{OSProjectID: "os-1", OSGrantUse: common.ProjectQuota{"dhbw-ipv4": 1}}
	if applyOSSyncState(&leaf, "os-1", &osMeasurement{}) && leaf.OSGrantUse["dhbw-ipv4"] != 1 {
		t.Errorf("unread use overwrote the stored one: %v", leaf.OSGrantUse)
	}
	if !applyOSSyncState(&leaf, "os-1", &osMeasurement{grantUse: common.ProjectQuota{"dhbw-ipv4": 0}}) || leaf.OSGrantUse["dhbw-ipv4"] != 0 {
		t.Errorf("fresh use not applied: %v", leaf.OSGrantUse)
	}
}
