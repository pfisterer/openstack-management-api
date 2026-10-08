package catalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/catalog"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

var configured = []common.ManagedProject{
	{ID: "cores", Name: "Cores", ShowOnUI: true},
	{ID: "net", Name: "Net", Kind: common.KindBool, ShowOnUI: true, Grant: &common.Grant{Type: common.GrantNetwork, Target: "net-uuid"}},
}

type fakeOpenStack struct {
	granted   map[string][]string // target -> projects
	targetErr error
}

func (f *fakeOpenStack) CheckGrantTarget(common.Grant) error { return f.targetErr }
func (f *fakeOpenStack) GrantedProjects(g common.Grant) ([]string, error) {
	return f.granted[g.Target], nil
}

type fixture struct {
	ctx   context.Context
	store tree.Store
	svc   *tree.Service
	cat   *catalog.Catalog
	os    *fakeOpenStack
	admin *catalog.Admin
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	log := zap.NewNop().Sugar()
	cat, err := catalog.Load(ctx, configured, &catalog.MemoryStore{}, log)
	if err != nil {
		t.Fatal(err)
	}
	store := tree.NewInMemoryStore(log)
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), configured, common.TokenList{"group:root"},
		5*time.Second, common.DefaultMaxAuthorizedUsers, tree.Accounting{}, log)
	svc.UseCatalog(cat)
	if err := svc.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	root := tree.RootNodeID
	budget := "b1"
	for _, n := range []tree.Node{
		{ID: budget, Kind: tree.KindBudget, ParentID: &root, Status: tree.StatusApproved, Name: "Budget",
			Limit: common.ProjectQuota{"cores": 10, "net": 1}},
		{ID: "p1", Kind: tree.KindProject, ParentID: &budget, Status: tree.StatusApproved, Name: "P1", OSProjectID: "os-p1",
			Limit: common.ProjectQuota{"cores": 2, "net": 1}},
		{ID: "p2", Kind: tree.KindProject, ParentID: &budget, Status: tree.StatusApproved, Name: "P2", OSProjectID: "os-p2",
			Limit:       common.ProjectQuota{"cores": 1, "net": 0},
			Allocations: []tree.Allocation{{BudgetID: root, Limit: common.ProjectQuota{"net": 1}, Reason: "lab"}}},
	} {
		if err := store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	os := &fakeOpenStack{granted: map[string][]string{}}
	return fixture{ctx: ctx, store: store, svc: svc, cat: cat, os: os,
		admin: &catalog.Admin{Catalog: cat, Tree: svc, OpenStack: os}}
}

func (f fixture) node(t *testing.T, id string) tree.Node {
	t.Helper()
	n, err := f.store.GetNode(f.ctx, id)
	if err != nil || n == nil {
		t.Fatalf("node %s: %v", id, err)
	}
	return *n
}

func resource(cat *catalog.Catalog, id string) (common.ManagedProject, bool) {
	for _, r := range cat.Resources() {
		if r.ID == id {
			return r, true
		}
	}
	return common.ManagedProject{}, false
}

func TestLoad_SeedsOnceFromConfiguration(t *testing.T) {
	ctx := context.Background()
	log := zap.NewNop().Sugar()
	store := &catalog.MemoryStore{}
	cat, err := catalog.Load(ctx, configured, store, log)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := cat.Get("net"); !ok || e.State != catalog.StateActive || e.CreatedBy != "configuration" {
		t.Fatalf("net = %+v, want seeded and active", e)
	}

	// Configured again with another availability: the store is the source now.
	more := append(configured, common.ManagedProject{ID: "gpu", Name: "GPU", Kind: common.KindBool,
		Grant: &common.Grant{Type: common.GrantFlavor, Target: "f"}})
	cat, err = catalog.Load(ctx, more, store, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resource(cat, "gpu"); ok {
		t.Error("an availability configured after seeding must be ignored")
	}
	if _, ok := resource(cat, "cores"); !ok {
		t.Error("quantities always come from the configuration")
	}
}

func TestWithdrawThenRemove(t *testing.T) {
	f := setup(t)

	if err := f.admin.Remove(f.ctx, "net", "admin@x"); !errors.Is(err, catalog.ErrConflict) {
		t.Fatalf("remove while active = %v, want conflict", err)
	}
	st, err := f.admin.Status(f.ctx, "net")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Holders) != 3 {
		t.Fatalf("holders = %+v, want budget, project and allocation", st.Holders)
	}

	_, changed, err := f.admin.Withdraw(f.ctx, "net", "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 4 {
		t.Errorf("changed = %d, want root, budget and both projects", changed)
	}
	if r, _ := resource(f.cat, "net"); !r.Withdrawn {
		t.Error("withdrawn availability must stay known, marked withdrawn")
	}
	if got := f.node(t, tree.RootNodeID).Limit["net"]; got != 0 {
		t.Errorf("root net = %d, want 0", got)
	}
	if a := f.node(t, "p2").Allocations; len(a) != 0 {
		t.Errorf("allocation for net alone must go, got %+v", a)
	}
	if st, _ := f.admin.Status(f.ctx, "net"); len(st.Holders) != 0 {
		t.Errorf("holders after withdraw = %+v", st.Holders)
	}

	// OpenStack has not revoked yet for p1; a hand-made grant elsewhere does not count.
	f.os.granted["net-uuid"] = []string{"os-p1", "someone-else"}
	if err := f.admin.Remove(f.ctx, "net", "admin@x"); !errors.Is(err, catalog.ErrConflict) {
		t.Fatalf("remove while OpenStack still grants = %v, want conflict", err)
	}
	f.os.granted["net-uuid"] = []string{"someone-else"}
	if err := f.admin.Remove(f.ctx, "net", "admin@x"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := resource(f.cat, "net"); ok {
		t.Error("removed availability still in the catalogue")
	}
	for _, id := range []string{tree.RootNodeID, "b1", "p1", "p2"} {
		if _, ok := f.node(t, id).Limit["net"]; ok {
			t.Errorf("%s still stores net", id)
		}
	}

	// Added again under the same id: a clean start, held by the root only.
	if _, err := f.admin.Add(f.ctx, catalog.NewEntry{ID: "net", Name: "Net", Grant: common.Grant{Type: common.GrantNetwork, Target: "net-uuid"}}, "admin@x"); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := f.node(t, tree.RootNodeID).Limit["net"]; got != 1 {
		t.Errorf("root net after re-add = %d, want 1", got)
	}
	if _, ok := f.node(t, "p1").Limit["net"]; ok {
		t.Error("re-added availability must not come back where it was")
	}
}

func TestRestore(t *testing.T) {
	f := setup(t)
	if _, _, err := f.admin.Withdraw(f.ctx, "net", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Restore(f.ctx, "net", "a"); err != nil {
		t.Fatal(err)
	}
	if r, _ := resource(f.cat, "net"); r.Withdrawn {
		t.Error("restored availability still withdrawn")
	}
	if f.node(t, tree.RootNodeID).Limit["net"] != 1 || f.node(t, "p1").Limit["net"] != 0 {
		t.Error("restore gives the root the availability and nobody else")
	}
	if _, err := f.admin.Restore(f.ctx, "net", "a"); err != nil {
		t.Errorf("restoring an active one is a no-op, got %v", err)
	}
}

func TestAdd_Refusals(t *testing.T) {
	f := setup(t)
	g := common.Grant{Type: common.GrantFlavor, Target: "f"}
	cases := map[string]struct {
		in   catalog.NewEntry
		want error
	}{
		"bad id":       {catalog.NewEntry{ID: "GPU!", Name: "x", Grant: g}, catalog.ErrInvalid},
		"no name":      {catalog.NewEntry{ID: "gpu", Grant: g}, catalog.ErrInvalid},
		"quantity id":  {catalog.NewEntry{ID: "cores", Name: "x", Grant: g}, catalog.ErrConflict},
		"existing id":  {catalog.NewEntry{ID: "net", Name: "x", Grant: g}, catalog.ErrConflict},
		"unknown kind": {catalog.NewEntry{ID: "gpu", Name: "x", Grant: common.Grant{Type: "volume", Target: "v"}}, catalog.ErrInvalid},
	}
	for name, tc := range cases {
		if _, err := f.admin.Add(f.ctx, tc.in, "a"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	f.os.targetErr = errors.New("flavor f is public")
	if _, err := f.admin.Add(f.ctx, catalog.NewEntry{ID: "gpu", Name: "GPU", Grant: g}, "a"); !errors.Is(err, catalog.ErrInvalid) {
		t.Errorf("target refused by OpenStack: err = %v, want invalid", err)
	}
	f.os.targetErr = catalog.ErrUnavailable
	if _, err := f.admin.Add(f.ctx, catalog.NewEntry{ID: "gpu", Name: "GPU", Grant: g}, "a"); !errors.Is(err, catalog.ErrUnavailable) {
		t.Errorf("OpenStack not connected: err = %v, want unavailable", err)
	}
	if _, ok := f.cat.Get("gpu"); ok {
		t.Error("a refused availability must not be stored")
	}
}
