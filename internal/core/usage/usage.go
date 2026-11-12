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

	"github.com/pfisterer/cloud-self-service-golib/day"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// PathEntry is one budget on the way from a project up to the root.
type PathEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Day is one project's consumption on one calendar day, with what was true of the
// project that day.
//
// The day is a day in Europe/Berlin (DayOf), decided on 12.11.2026 like the AI service (aiaas-management-api, docs/architektur.md "Tage der Nutzung"); rows written before the first release after 0.8.81 carry UTC days and stay so, without migration (ARCHITECTURE.md §8.4a).
type Day struct {
	Day    time.Time // the day's label: midnight UTC of its date, the form of the date column
	NodeID string

	// Snapshot. For days collected after the fact (Backfilled) these are the
	// values at collection time — the tree keeps no history of them.
	ExternalID  string
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
	Attributes model.Attributes

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

// DayOf is the day t falls on in Europe/Berlin, as stored: midnight UTC of that date (day.Date.AsUTC), the form of the date column. Before the first release after 0.8.81 it was the UTC date of t, so between midnight in Berlin and midnight UTC (one hour in winter, two in summer) a moment now counts for the next day.
func DayOf(t time.Time) time.Time { return day.Of(t).AsUTC() }

// BerlinSince is the first day counted in Europe/Berlin in production: 0.8.82 went live on 12.11.2026 around 11:00 in Berlin, after the old release had stored 11.11.2026 as a UTC day (up to midnight UTC). Earlier days in project_usage_daily are UTC days.
var BerlinSince = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

// Bounds is the stretch of time a stored day covers: from midnight to midnight in Europe/Berlin, 23 hours on the day the clocks go forward and 25 on the day they go back. stored is a day as stored (DayOf).
//
// The switch day (BerlinSince) starts where the last UTC day ended, at midnight UTC instead of midnight in Berlin, so the hour between them is not counted in both rows; it covers 23 hours. A stored row does not know the span it was measured over, so the seam is a fixed date rather than read from the row before; a fresh installation backfilling across that date leaves that hour out instead (ARCHITECTURE.md §8.4a, Days).
func Bounds(stored time.Time) (start, end time.Time) {
	d := day.FromUTC(stored)
	start, end = d.Start(), d.End()
	if d.AsUTC().Equal(BerlinSince) {
		start = BerlinSince
	}
	return start, end
}

// label is a day as stored, normalised to midnight UTC of its date. It reads the date of a label, not the day of an instant: a label is already a date and must not move to the next day in Berlin.
func label(t time.Time) time.Time { return day.FromUTC(t).AsUTC() }
