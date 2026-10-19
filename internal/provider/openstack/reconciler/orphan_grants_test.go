package reconciler

import (
	"errors"
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// The orphan sweep deletes network shares, which takes a network away from a project if it gets it wrong: every test here is about a share it must NOT delete, bar one.

type fakeOrphans struct {
	shares   map[string][]osclient.NetworkShare // network -> shares
	projects map[string]bool                    // project -> exists
	failGet  map[string]bool                    // project -> Keystone refuses
	asked    []string
	deleted  []string
}

func (f *fakeOrphans) ListNetworkShares(networkID string) ([]osclient.NetworkShare, error) {
	return f.shares[networkID], nil
}

func (f *fakeOrphans) DeleteNetworkShare(policyID string) error {
	f.deleted = append(f.deleted, policyID)
	return nil
}

func (f *fakeOrphans) ProjectExists(id string) (bool, error) {
	f.asked = append(f.asked, id)
	if f.failGet[id] {
		return false, errors.New("403 forbidden")
	}
	return f.projects[id], nil
}

const ownerProject = "owner-parent"

func share(policy, network, target, owner string) osclient.NetworkShare {
	return osclient.NetworkShare{PolicyID: policy, NetworkID: network, TargetProject: target, OwnerProject: owner}
}

func sweepFixture() *fakeOrphans {
	return &fakeOrphans{
		shares: map[string][]osclient.NetworkShare{
			"net-1": {
				share("gone", "net-1", "deleted-project", ownerProject),
				share("alive", "net-1", "living-project", ownerProject),
				share("listed", "net-1", "listed-project", ownerProject),
				share("everyone", "net-1", "*", ownerProject),
				share("empty", "net-1", "", ownerProject),
				share("foreign-owner", "net-1", "deleted-elsewhere", "someone-else"),
				share("refused", "net-1", "invisible-project", ownerProject),
				share("self", "net-1", ownerProject, ownerProject),
			},
			// Not in the catalogue: never listed, never touched.
			"net-other": {share("not-ours", "net-other", "deleted-project", ownerProject)},
		},
		projects: map[string]bool{"living-project": true, "listed-project": true},
		failGet:  map[string]bool{"invisible-project": true},
	}
}

func TestSweepOrphanedGrants_DeletesOnlyTheShareOfAProjectKeystoneSaysIsGone(t *testing.T) {
	f := sweepFixture()
	known := map[string]osclient.ProjectInfo{"listed-project": {ID: "listed-project"}}

	n := sweepOrphanedGrants(f, grantCatalogue, []string{ownerProject}, known, false, zap.NewNop().Sugar())

	if n != 1 || !slices.Equal(f.deleted, []string{"gone"}) {
		t.Fatalf("deleted %v (%d), want just the share of the deleted project", f.deleted, n)
	}
	// Only the concrete targets of our own policies outside the listing are looked up.
	for _, p := range f.asked {
		if p == "listed-project" || p == "*" || p == "" || p == "deleted-elsewhere" || p == ownerProject {
			t.Errorf("asked Keystone about %q", p)
		}
	}
}

func TestSweepOrphanedGrants_DryRunDeletesNothing(t *testing.T) {
	f := sweepFixture()
	n := sweepOrphanedGrants(f, grantCatalogue, []string{ownerProject}, nil, true, zap.NewNop().Sugar())
	if n != 1 || len(f.deleted) != 0 {
		t.Fatalf("dry run: counted %d, deleted %v", n, f.deleted)
	}
}

func TestSweepOrphanedGrants_WithoutAnOwnerDoesNothing(t *testing.T) {
	f := sweepFixture()
	if n := sweepOrphanedGrants(f, grantCatalogue, nil, nil, false, zap.NewNop().Sugar()); n != 0 || len(f.deleted) != 0 || len(f.asked) != 0 {
		t.Fatalf("no owner: %d deleted %v asked %v", n, f.deleted, f.asked)
	}
}

// Flavours and images are not swept: they carry no owner, and they hold no quota.
func TestSweepOrphanedGrants_OnlyNetworks(t *testing.T) {
	f := sweepFixture()
	f.shares["flavor-1"] = []osclient.NetworkShare{share("flavor", "flavor-1", "deleted-project", ownerProject)}
	defs := []osclient.ManagedResource{{ID: "gpu", Name: "GPU", Kind: common.KindBool, Grant: &flavorGrant}}
	if n := sweepOrphanedGrants(f, defs, []string{ownerProject}, nil, false, zap.NewNop().Sugar()); n != 0 || len(f.deleted) != 0 {
		t.Fatalf("swept a flavour: %v", f.deleted)
	}
}

// One lookup per project, however many networks it holds.
func TestSweepOrphanedGrants_AsksOncePerProject(t *testing.T) {
	net2 := osclient.Grant{Type: osclient.GrantNetwork, Target: "net-2"}
	f := &fakeOrphans{shares: map[string][]osclient.NetworkShare{
		"net-1": {share("a", "net-1", "deleted-project", ownerProject)},
		"net-2": {share("b", "net-2", "deleted-project", ownerProject)},
	}}
	defs := []osclient.ManagedResource{
		{ID: "n1", Name: "N1", Kind: common.KindBool, Grant: &netGrant},
		{ID: "n2", Name: "N2", Kind: common.KindBool, Grant: &net2},
	}
	n := sweepOrphanedGrants(f, defs, []string{ownerProject}, nil, false, zap.NewNop().Sugar())
	if n != 2 || len(f.asked) != 1 {
		t.Fatalf("deleted %d, asked %v", n, f.asked)
	}
}
