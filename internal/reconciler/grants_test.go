package reconciler

import (
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
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
