package usage

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

var testMapping = Mapping{Cores: "cores", RAM: "ram", Storage: "storage", RAMToGB: 1}

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

// Used and reserved are summed alike, so that dividing them is utilisation:
// a project that reserved 4 cores for two days and ran 2 vCPUs for 24 hours
// used a quarter of them.
func TestBuildReport_SumsAndUtilisation(t *testing.T) {
	reserved := common.ProjectQuota{"cores": 4, "ram": 8, "storage": 100}
	rows := []Day{
		{Day: day("2026-10-01"), NodeID: "p1", ProjectName: "old name", VCPUHours: 48, RAMGBHours: 96, ServerHours: 24, Reserved: reserved, Backfilled: true},
		{Day: day("2026-10-02"), NodeID: "p1", ProjectName: "new name", Reserved: reserved, StorageGB: 25},
		{Day: day("2026-10-02"), NodeID: "p2", VCPUHours: 1, Reserved: common.ProjectQuota{}},
	}
	r := BuildReport(rows, day("2026-10-01"), day("2026-10-02"), testMapping, nil)

	if r.From != "2026-10-01" || r.To != "2026-10-02" || len(r.Days) != 2 || r.BackfilledDays != 1 {
		t.Fatalf("period %s..%s, %d days, %d backfilled", r.From, r.To, len(r.Days), r.BackfilledDays)
	}
	if r.VCPUHours != 49 || r.ReservedCoreHours != 192 {
		t.Errorf("vcpu %v of %v reserved core hours", r.VCPUHours, r.ReservedCoreHours)
	}
	p1 := r.Projects[0]
	if p1.NodeID != "p1" || p1.ProjectName != "new name" || p1.Days != 2 || p1.LastActive != "2026-10-01" {
		t.Fatalf("first project %+v", p1)
	}
	if got := *p1.Utilization.Cores; got != 0.25 {
		t.Errorf("core utilisation %v, want 0.25", got)
	}
	if got := *p1.Utilization.RAM; got != 0.25 {
		t.Errorf("RAM utilisation %v, want 0.25", got)
	}
	// Storage only from the day with a sample: 25 of 100 GB.
	if got := *p1.Utilization.Storage; got != 0.25 || p1.SampledDays != 1 {
		t.Errorf("storage utilisation %v over %d days, want 0.25 over 1", got, p1.SampledDays)
	}
	if r.Projects[1].Utilization.Cores != nil {
		t.Error("a project that reserved nothing has a utilisation")
	}
	if r.ValueEUR != nil || r.Prices != nil || p1.ValueEUR != nil {
		t.Error("a value without prices")
	}
}

// A change of attributes within the period splits the project, so each part
// is billed where it belonged on the day.
func TestBuildReport_SplitsByAttributes(t *testing.T) {
	a := tree.Attributes{"billing": {"cost_center": "1"}}
	b := tree.Attributes{"billing": {"cost_center": "2"}}
	rows := []Day{
		{Day: day("2026-10-01"), NodeID: "p1", VCPUHours: 10, Attributes: a},
		{Day: day("2026-10-02"), NodeID: "p1", VCPUHours: 10, Attributes: a},
		{Day: day("2026-10-03"), NodeID: "p1", VCPUHours: 5, Attributes: b},
	}
	r := BuildReport(rows, day("2026-10-01"), day("2026-10-03"), testMapping, nil)
	if len(r.Projects) != 2 {
		t.Fatalf("want two parts, got %+v", r.Projects)
	}
	if p := r.Projects[0]; p.Attributes["billing"]["cost_center"] != "1" || p.VCPUHours != 20 || p.Days != 2 {
		t.Errorf("first part %+v", p)
	}
	if p := r.Projects[1]; p.Attributes["billing"]["cost_center"] != "2" || p.VCPUHours != 5 || p.Days != 1 {
		t.Errorf("second part %+v", p)
	}
	if r.VCPUHours != 25 {
		t.Errorf("total %v", r.VCPUHours)
	}
}

func TestBuildReport_PublicIPv4(t *testing.T) {
	two, zero := 2, 0
	rows := []Day{
		{Day: day("2026-10-01"), NodeID: "p1", PublicIPv4: &two},
		{Day: day("2026-10-02"), NodeID: "p1", PublicIPv4: &zero},
		{Day: day("2026-10-03"), NodeID: "p1"},
	}
	r := BuildReport(rows, day("2026-10-01"), day("2026-10-03"), testMapping, nil)
	if r.PublicIPv4Days != 2 || r.IPv4SampledDays != 2 || r.Projects[0].IPv4SampledDays != 2 {
		t.Errorf("%v IPv4 days over %d sampled days", r.PublicIPv4Days, r.IPv4SampledDays)
	}
}

func TestBuildReport_Value(t *testing.T) {
	rows := []Day{{Day: day("2026-10-02"), NodeID: "p1", VCPUHours: 10, RAMGBHours: 20, StorageGB: 30}}
	r := BuildReport(rows, day("2026-10-02"), day("2026-10-02"), testMapping, &Prices{VCPUHour: 1, RAMGBHour: 0.5, StorageGBDay: 0.1})
	if r.ValueEUR == nil || *r.ValueEUR != 23 || *r.Projects[0].ValueEUR != 23 {
		t.Fatalf("value %v", r.ValueEUR)
	}
	if r := BuildReport(rows, day("2026-10-02"), day("2026-10-02"), testMapping, &Prices{}); r.ValueEUR != nil {
		t.Error("zero prices give a value")
	}
}

// A catalogue in GB says so with a multiplier; RAM is reserved in its unit.
func TestMappingFrom(t *testing.T) {
	m := MappingFrom([]common.ManagedProject{
		{ID: "c", OSQuotaField: "cores"},
		{ID: "r", OSQuotaField: "ram", OSMultiplier: 1024},
		{ID: "s", OSQuotaField: "gigabytes"},
	})
	if m.Cores != "c" || m.RAM != "r" || m.Storage != "s" || m.RAMToGB != 1 {
		t.Fatalf("%+v", m)
	}
	if m := MappingFrom([]common.ManagedProject{{ID: "r", OSQuotaField: "ram"}}); m.RAMToGB != 1.0/1024 {
		t.Errorf("RAM in MB converts with %v", m.RAMToGB)
	}
}

// A subtree is what the rows recorded: a project counts under every budget on
// its path that day, and nowhere else.
func TestMemoryStore_DaysUnder(t *testing.T) {
	s := NewMemoryStore()
	path := []PathEntry{{ID: "faculty"}, {ID: "dept"}, {ID: "root"}}
	_ = s.ReplaceDay(context.Background(), day("2026-10-02"), []Day{
		{NodeID: "p1", BudgetPath: path},
		{NodeID: "p2", BudgetPath: []PathEntry{{ID: "bio"}, {ID: "root"}}},
	})
	for budget, want := range map[string]int{"faculty": 1, "dept": 1, "root": 2, "bio": 1, "other": 0} {
		rows, _ := s.DaysUnder(context.Background(), budget, day("2026-10-01"), day("2026-10-03"))
		if len(rows) != want {
			t.Errorf("%s: %d rows, want %d", budget, len(rows), want)
		}
	}
}
