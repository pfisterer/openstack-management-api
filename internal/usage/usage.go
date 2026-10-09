// Package usage records what each project actually used, one row per project
// and day, so it can later be said which project, budget, faculty or location
// consumed what — the evidence for the infrastructure, not the allocation.
//
// The rows live in their own table, not in the node document: there are many
// per project, the past must not change when a project is renamed, moved or
// handed over (so every row carries a snapshot of those), and they must outlive
// the project — released projects and deleted budgets disappear from the tree,
// their consumption must not disappear from a yearly report. Hence no foreign
// key, only the node id as a reference.
package usage

import (
	"context"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// PathEntry is one budget on the way from a project up to the root.
type PathEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Day is one project's consumption on one UTC day, with what was true of the
// project that day.
type Day struct {
	Day    time.Time // UTC midnight
	NodeID string

	// Snapshot. For days collected after the fact (Backfilled) these are the
	// values at collection time — the tree keeps no history of them.
	OSProjectID string
	ProjectName string
	Owner       string
	Status      string
	BudgetID    string
	BudgetPath  []PathEntry // the project's own budget first, the root last
	// People counts the distinct persons named on the project — owner, admins
	// and members given as a person; Groups the members given as a group,
	// whose size is not known here.
	People int
	Groups int

	// Used, from Nova's own accounting: hours a server existed, weighted.
	ServerHours float64
	VCPUHours   float64
	RAMGBHours  float64
	DiskGBHours float64
	// StorageGB is the volume storage in use when the day was collected — a
	// daily sample, since Cinder keeps no such books. Zero on backfilled days.
	StorageGB float64
	// PublicIPv4 is the number of public IPv4 addresses the project held when
	// the day was collected — floating IPs and addresses on public networks,
	// a daily sample like StorageGB. nil where nothing was measured: backfilled
	// days, a failed count, a deployment that names no public networks and
	// has no floating IPs to count.
	PublicIPv4 *int

	// Reserved is what the project held that day: its own limit plus its
	// allocations, quantities only.
	Reserved common.ProjectQuota

	// Attributes are the attribute groups that applied to the project that
	// day, its own and inherited — what billing reads. A snapshot like the
	// budget path, so changing a cost centre changes the days from then on.
	Attributes tree.Attributes

	Backfilled  bool
	CollectedAt time.Time
}

// Store keeps the daily rows.
type Store interface {
	// ReplaceDay stores the rows of one day, replacing whatever that day held:
	// collecting a day twice gives the same rows, not twice as many.
	ReplaceDay(ctx context.Context, day time.Time, rows []Day) error
	// LastDay is the latest day stored; ok is false when nothing is.
	LastDay(ctx context.Context) (day time.Time, ok bool, err error)
	// Days returns the rows of the given nodes in [from, to), oldest first.
	// No nodes means all of them.
	Days(ctx context.Context, nodeIDs []string, from, to time.Time) ([]Day, error)
	// DaysUnder returns the rows in [from, to) of the projects that were below
	// the budget on the day — by the budget path the row recorded, so a project
	// moved away still counts where it was, and one released long ago too.
	DaysUnder(ctx context.Context, budgetID string, from, to time.Time) ([]Day, error)
}

// dayOf truncates a time to its UTC day.
func dayOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
