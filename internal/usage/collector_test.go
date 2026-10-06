package usage

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

var catalog = []common.ManagedProject{
	{ID: "cores", Name: "Cores", OSQuotaField: "cores"},
	{ID: "storage", Name: "Storage", OSQuotaField: "gigabytes"},
	{ID: "ipv4", Name: "IPv4", Kind: common.KindBool},
	{ID: "ports", Name: "Ports", Static: true, OSQuotaField: "ports"},
}

type fakeSource struct {
	calls []time.Time
	usage map[string]osclient.TenantUsage
}

func (f *fakeSource) TenantUsages(start, end time.Time) ([]osclient.TenantUsage, error) {
	f.calls = append(f.calls, start)
	var out []osclient.TenantUsage
	for _, u := range f.usage {
		out = append(out, u)
	}
	return out, nil
}

func ptr(s string) *string { return &s }

// A tree of root → Uni → Students with three projects: one that ran, one that
// is approved but idle, one rejected and never in OpenStack.
func fixture(t *testing.T) (*Collector, *MemoryStore, *fakeSource) {
	t.Helper()
	ctx := context.Background()
	nodes := tree.NewInMemoryStore(zap.NewNop().Sugar())
	for _, n := range []tree.Node{
		{ID: "root", Kind: tree.KindBudget, Name: "Root", Status: tree.StatusApproved},
		{ID: "uni", Kind: tree.KindBudget, Name: "Uni", ParentID: ptr("root"), Status: tree.StatusApproved},
		{ID: "stud", Kind: tree.KindBudget, Name: "Students", ParentID: ptr("uni"), Status: tree.StatusApproved},
		{ID: "p_run", Kind: tree.KindProject, Name: "Thesis", ParentID: ptr("stud"), Status: tree.StatusApproved,
			Owner: "user:a@x", OSProjectID: "os-run",
			Limit:           common.ProjectQuota{"cores": 4, "storage": 50, "ipv4": 0, "ports": 10},
			Allocations:     []tree.Allocation{{BudgetID: "uni", Limit: common.ProjectQuota{"cores": 8, "ipv4": 1}}},
			AdminScope:      common.TokenList{"user:b@x"},
			AuthorizedUsers: []common.AuthorizedUser{{Token: "user:a@x"}, {Token: "user:c@x"}, {Token: "group:course"}},
			OSInUse:         common.ProjectQuota{"storage": 30}},
		{ID: "p_idle", Kind: tree.KindProject, Name: "Idle", ParentID: ptr("stud"), Status: tree.StatusApproved,
			Owner: "user:d@x", OSProjectID: "os-idle", Limit: common.ProjectQuota{"cores": 2}},
		{ID: "p_rej", Kind: tree.KindProject, Name: "No", ParentID: ptr("stud"), Status: tree.StatusRejected, Owner: "user:e@x"},
	} {
		if err := nodes.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	src := &fakeSource{usage: map[string]osclient.TenantUsage{
		"os-run":     {ProjectID: "os-run", ServerHours: 24, VCPUHours: 96, MemoryMBHours: 24 * 8192, LocalGBHours: 480},
		"os-foreign": {ProjectID: "os-foreign", ServerHours: 5, VCPUHours: 5},
	}}
	store := NewMemoryStore()
	c := NewCollector(store, nodes, src, catalog, zap.NewNop().Sugar())
	c.now = func() time.Time { return time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC) }
	return c, store, src
}

func TestCollectDay(t *testing.T) {
	c, store, _ := fixture(t)
	ctx := context.Background()
	yesterday := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if err := c.CollectDay(ctx, yesterday); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.Days(ctx, nil, yesterday, yesterday.AddDate(0, 0, 1))
	if len(rows) != 2 {
		t.Fatalf("want the project that ran and the idle one, not the rejected or a foreign one; got %d rows", len(rows))
	}
	run := rows[1]
	if run.NodeID != "p_run" {
		t.Fatalf("unexpected order: %+v", rows)
	}
	if run.VCPUHours != 96 || run.RAMGBHours != 192 || run.ServerHours != 24 || run.DiskGBHours != 480 {
		t.Errorf("used amounts = %+v", run)
	}
	if run.StorageGB != 30 || run.Backfilled {
		t.Errorf("yesterday is sampled, not backfilled: storage %v, backfilled %v", run.StorageGB, run.Backfilled)
	}
	if run.Reserved["cores"] != 12 || len(run.Reserved) != 2 {
		t.Errorf("reserved should be quantities incl. allocations, without availabilities or static quotas: %v", run.Reserved)
	}
	if len(run.BudgetPath) != 3 || run.BudgetPath[0].Name != "Students" || run.BudgetPath[2].ID != "root" {
		t.Errorf("budget path = %+v", run.BudgetPath)
	}
	if run.People != 3 || run.Groups != 1 {
		t.Errorf("people/groups = %d/%d, want owner a, admin b, member c and one group", run.People, run.Groups)
	}
	if idle := rows[0]; idle.VCPUHours != 0 || idle.Reserved["cores"] != 2 {
		t.Errorf("an idle reservation must still be recorded: %+v", idle)
	}
}

// The first run reaches back, a bounded number of days at a time; later runs
// only add what is missing, and nothing is collected twice.
func TestCatchUp(t *testing.T) {
	c, store, src := fixture(t)
	ctx := context.Background()
	c.BackfillDays = 40
	c.MaxDaysPerRun = 31

	if n, err := c.CatchUp(ctx); err != nil || n != 31 {
		t.Fatalf("first run: %d days, %v; want 31", n, err)
	}
	if n, _ := c.CatchUp(ctx); n != 9 {
		t.Fatalf("second run: %d days; want the remaining 9", n)
	}
	if n, _ := c.CatchUp(ctx); n != 0 {
		t.Fatalf("third run: %d days; want none, yesterday is stored", n)
	}
	if len(src.calls) != 40 {
		t.Errorf("Nova asked %d times, want once per day", len(src.calls))
	}
	last, _, _ := store.LastDay(ctx)
	if !last.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last day = %v, want yesterday — today is not finished", last)
	}
	rows, _ := store.Days(ctx, []string{"p_run"}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), last.AddDate(0, 0, 1))
	if len(rows) != 40 || !rows[0].Backfilled || rows[0].StorageGB != 0 {
		t.Errorf("older days are backfilled without a storage sample: %d rows, first %+v", len(rows), rows[0])
	}

	// Collecting a day again replaces it.
	if err := c.CollectDay(ctx, last); err != nil {
		t.Fatal(err)
	}
	again, _ := store.Days(ctx, nil, last, last.AddDate(0, 0, 1))
	if len(again) != 2 {
		t.Errorf("a day collected twice holds %d rows, want 2", len(again))
	}
}
