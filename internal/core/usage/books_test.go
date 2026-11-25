package usage

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"go.uber.org/zap"
)

var catalog = []common.Resource{
	{ID: "cores", Name: "Cores", Accounts: common.AccountsCores},
	{ID: "storage", Name: "Storage", Accounts: common.AccountsStorage},
	{ID: "ipv4", Name: "IPv4", Kind: common.KindBool},
	{ID: "ports", Name: "Ports", Static: true},
}

// report is what the executing system reports for a day in these tests.
var report = DayReport{Usage: map[string]Measured{
	"os-run":     {ServerHours: 24, VCPUHours: 96, RAMGBHours: 24 * 8, DiskGBHours: 480},
	"os-foreign": {ServerHours: 5, VCPUHours: 5},
}}

func ptr(s string) *string { return &s }

// A tree of root → Uni → Students with three projects: one that ran, one that
// is approved but idle, one rejected and never in OpenStack.
func fixture(t *testing.T) (*Books, *MemoryStore) {
	t.Helper()
	ctx := context.Background()
	nodes := nodestore.NewInMemoryStore(zap.NewNop().Sugar())
	for _, n := range migrated(t, []model.Node{
		{ID: "root", Kind: model.KindBudget, Name: "Root", Status: model.StatusApproved},
		{ID: "uni", Kind: model.KindBudget, Name: "Uni", ParentID: ptr("root"), Status: model.StatusApproved,
			Attributes: model.Attributes{"billing": {"cost_center": "4711"}}},
		{ID: "stud", Kind: model.KindBudget, Name: "Students", ParentID: ptr("uni"), Status: model.StatusApproved},
		{ID: "p_run", Kind: model.KindProject, Name: "Thesis", ParentID: ptr("stud"), Status: model.StatusApproved,
			Owner:           "user:a@x",
			Limit:           common.ProjectQuota{"cores": 4, "storage": 50, "ipv4": 0, "ports": 10},
			Allocations:     []model.Allocation{{BudgetID: "uni", Limit: common.ProjectQuota{"cores": 8, "ipv4": 1}}},
			AdminScope:      common.TokenList{"user:b@x"},
			AuthorizedUsers: []common.AuthorizedUser{{Token: "user:a@x"}, {Token: "user:c@x"}, {Token: "group:course"}},
			Bindings:        map[string]model.Binding{"openstack": {ExternalID: "os-run", InUse: common.ProjectQuota{"storage": 30}}},
			Attributes:      model.Attributes{"funding": {"wbs": "D-1"}}},
		{ID: "p_idle", Kind: model.KindProject, Name: "Idle", ParentID: ptr("stud"), Status: model.StatusApproved,
			Owner: "user:d@x", Bindings: map[string]model.Binding{"openstack": {ExternalID: "os-idle"}}, Limit: common.ProjectQuota{"cores": 2}},
		{ID: "p_rej", Kind: model.KindProject, Name: "No", ParentID: ptr("stud"), Status: model.StatusRejected, Owner: "user:e@x"},
	}) {
		if err := nodes.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	store := NewMemoryStore()
	c := NewBooks(store, nodes, catalog, "openstack", zap.NewNop().Sugar())
	c.now = func() time.Time { return time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC) }
	return c, store
}

func TestRecordDay(t *testing.T) {
	c, store := fixture(t)
	ctx := context.Background()
	yesterday := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if err := c.RecordDay(ctx, yesterday, report); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.Days(ctx, nil, yesterday, yesterday.AddDate(0, 0, 1))
	// The project that ran is paid by two budgets (its share under Students, 8 cores from Uni): a row per allocation, its use split like their limits.
	if len(rows) != 3 {
		t.Fatalf("want the project that ran (two allocations) and the idle one, not the rejected or a foreign one; got %d rows", len(rows))
	}
	var parts []Day
	for _, r := range rows {
		if r.NodeID == "p_run" {
			parts = append(parts, r)
		}
	}
	if len(parts) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	var vcpu, ram, server, disk, storage float64
	reserved := 0
	for _, r := range parts {
		vcpu, ram, server, disk, storage = vcpu+r.VCPUHours, ram+r.RAMGBHours, server+r.ServerHours, disk+r.DiskGBHours, storage+r.StorageGB
		reserved += r.Reserved["cores"]
		if r.AllocationID == "" {
			t.Errorf("a row of a project with allocations names its allocation: %+v", r)
		}
	}
	if vcpu != 96 || ram != 192 || server != 24 || disk != 480 || storage != 30 || reserved != 12 {
		t.Errorf("the parts add up to the project's use: vcpu %v ram %v server %v disk %v storage %v reserved %d", vcpu, ram, server, disk, storage, reserved)
	}
	run := parts[0]
	if run.BudgetID != "stud" {
		run = parts[1]
	}
	if run.VCPUHours != 32 || run.StorageGB != 30 || run.Backfilled {
		t.Errorf("the share under Students: 4 of 12 cores, all of the storage: %+v", run)
	}
	if run.Reserved["cores"] != 4 || len(run.Reserved) != 2 {
		t.Errorf("reserved is the allocation's own, quantities only: %v", run.Reserved)
	}
	if len(run.BudgetPath) != 3 || run.BudgetPath[0].Name != "Students" || run.BudgetPath[2].ID != "root" {
		t.Errorf("budget path = %+v", run.BudgetPath)
	}
	if len(run.Attributes) != 2 || run.Attributes["billing"]["cost_center"] != "4711" || run.Attributes["funding"]["wbs"] != "D-1" {
		t.Errorf("attributes = %v, want the university's billing and the allocation's own funding", run.Attributes)
	}
	if run.People != 3 || run.Groups != 1 {
		t.Errorf("people/groups = %d/%d, want owner a, admin b, member c and one group", run.People, run.Groups)
	}
	if idle := rows[0]; idle.NodeID != "p_idle" || idle.VCPUHours != 0 || idle.Reserved["cores"] != 2 {
		t.Errorf("an idle reservation must still be recorded: %+v", idle)
	}
}

// Public IPv4 is a sample of now, like storage: booked for yesterday, a
// project holding none counts zero, nothing counted leaves the day unmeasured
// instead of claiming zero, and an older day never carries today's count.
func TestRecordDay_PublicIPv4(t *testing.T) {
	c, store := fixture(t)
	ctx := context.Background()
	yesterday := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	counted := report
	counted.PublicIPv4 = map[string]int{"os-run": 3}
	_ = c.RecordDay(ctx, yesterday, counted)
	rows, _ := store.Days(ctx, nil, yesterday, yesterday.AddDate(0, 0, 1))
	if rows[1].PublicIPv4 == nil || *rows[1].PublicIPv4 != 3 || rows[0].PublicIPv4 == nil || *rows[0].PublicIPv4 != 0 {
		t.Errorf("counts: run %v, idle %v", rows[1].PublicIPv4, rows[0].PublicIPv4)
	}

	_ = c.RecordDay(ctx, yesterday, report)
	rows, _ = store.Days(ctx, nil, yesterday, yesterday.AddDate(0, 0, 1))
	if rows[1].PublicIPv4 != nil {
		t.Errorf("a day without a count was stored as %d", *rows[1].PublicIPv4)
	}

	earlier := yesterday.AddDate(0, 0, -5)
	_ = c.RecordDay(ctx, earlier, counted)
	rows, _ = store.Days(ctx, nil, earlier, earlier.AddDate(0, 0, 1))
	if rows[0].PublicIPv4 != nil {
		t.Error("a backfilled day carries today's addresses")
	}
	if c.Sampled(earlier) || !c.Sampled(yesterday) {
		t.Error("only yesterday is sampled")
	}
}

// The first pass reaches back, a bounded number of days at a time; later
// passes only ask for what is missing, and no day is asked for twice.
func TestDaysToRecord(t *testing.T) {
	c, store := fixture(t)
	ctx := context.Background()
	c.BackfillDays = 40
	c.MaxDaysPerRun = 31
	pass := func() int {
		days, err := c.DaysToRecord(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range days {
			if err := c.RecordDay(ctx, d, report); err != nil {
				t.Fatal(err)
			}
		}
		return len(days)
	}

	if n := pass(); n != 31 {
		t.Fatalf("first pass: %d days; want 31", n)
	}
	if n := pass(); n != 9 {
		t.Fatalf("second pass: %d days; want the remaining 9", n)
	}
	if n := pass(); n != 0 {
		t.Fatalf("third pass: %d days; want none, yesterday is stored", n)
	}
	last, _, _ := store.LastDay(ctx)
	if !last.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last day = %v, want yesterday — today is not finished", last)
	}
	rows, _ := store.Days(ctx, []string{"p_run"}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), last.AddDate(0, 0, 1))
	if len(rows) != 80 || !rows[0].Backfilled || rows[0].StorageGB != 0 {
		t.Errorf("older days are backfilled without a storage sample (two allocations, two rows a day): %d rows, first %+v", len(rows), rows[0])
	}

	// Recording a day again replaces it.
	if err := c.RecordDay(ctx, last, report); err != nil {
		t.Fatal(err)
	}
	again, _ := store.Days(ctx, nil, last, last.AddDate(0, 0, 1))
	if len(again) != 3 {
		t.Errorf("a day recorded twice holds %d rows, want 3", len(again))
	}
}

// migrated is test data written as the former model (projects in the tree) as it is stored since allocation_leaves.
func migrated(t *testing.T, nodes []model.Node) []model.Node {
	t.Helper()
	out, err := nodestore.MigrateNodes(nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return out
}
