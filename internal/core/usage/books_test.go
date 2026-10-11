package usage

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
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
	nodes := tree.NewInMemoryStore(zap.NewNop().Sugar())
	for _, n := range []tree.Node{
		{ID: "root", Kind: tree.KindBudget, Name: "Root", Status: tree.StatusApproved},
		{ID: "uni", Kind: tree.KindBudget, Name: "Uni", ParentID: ptr("root"), Status: tree.StatusApproved,
			Attributes: tree.Attributes{"billing": {"cost_center": "4711"}}},
		{ID: "stud", Kind: tree.KindBudget, Name: "Students", ParentID: ptr("uni"), Status: tree.StatusApproved},
		{ID: "p_run", Kind: tree.KindProject, Name: "Thesis", ParentID: ptr("stud"), Status: tree.StatusApproved,
			Owner: "user:a@x", OSProjectID: "os-run",
			Limit:           common.ProjectQuota{"cores": 4, "storage": 50, "ipv4": 0, "ports": 10},
			Allocations:     []tree.Allocation{{BudgetID: "uni", Limit: common.ProjectQuota{"cores": 8, "ipv4": 1}}},
			AdminScope:      common.TokenList{"user:b@x"},
			AuthorizedUsers: []common.AuthorizedUser{{Token: "user:a@x"}, {Token: "user:c@x"}, {Token: "group:course"}},
			OSInUse:         common.ProjectQuota{"storage": 30},
			Attributes:      tree.Attributes{"funding": {"wbs": "D-1"}}},
		{ID: "p_idle", Kind: tree.KindProject, Name: "Idle", ParentID: ptr("stud"), Status: tree.StatusApproved,
			Owner: "user:d@x", OSProjectID: "os-idle", Limit: common.ProjectQuota{"cores": 2}},
		{ID: "p_rej", Kind: tree.KindProject, Name: "No", ParentID: ptr("stud"), Status: tree.StatusRejected, Owner: "user:e@x"},
	} {
		if err := nodes.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	store := NewMemoryStore()
	c := NewBooks(store, nodes, catalog, zap.NewNop().Sugar())
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
	if len(run.Attributes) != 2 || run.Attributes["billing"]["cost_center"] != "4711" || run.Attributes["funding"]["wbs"] != "D-1" {
		t.Errorf("attributes = %v, want the university's billing and the project's own funding", run.Attributes)
	}
	if run.People != 3 || run.Groups != 1 {
		t.Errorf("people/groups = %d/%d, want owner a, admin b, member c and one group", run.People, run.Groups)
	}
	if idle := rows[0]; idle.VCPUHours != 0 || idle.Reserved["cores"] != 2 {
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
	if len(rows) != 40 || !rows[0].Backfilled || rows[0].StorageGB != 0 {
		t.Errorf("older days are backfilled without a storage sample: %d rows, first %+v", len(rows), rows[0])
	}

	// Recording a day again replaces it.
	if err := c.RecordDay(ctx, last, report); err != nil {
		t.Fatal(err)
	}
	again, _ := store.Days(ctx, nil, last, last.AddDate(0, 0, 1))
	if len(again) != 2 {
		t.Errorf("a day recorded twice holds %d rows, want 2", len(again))
	}
}
