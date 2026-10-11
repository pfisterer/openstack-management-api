package reconciler

import (
	"context"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/usage"
)

// UsageBooks is where the tree keeps what projects used per day (package
// usage, *usage.Books): which days are still missing, whether a day is recent
// enough for what can only be counted now, and the report for a day.
type UsageBooks interface {
	DaysToRecord(ctx context.Context) ([]time.Time, error)
	Sampled(day time.Time) bool
	RecordDay(ctx context.Context, day time.Time, report usage.DayReport) error
}

// UsageMeter reads what projects used from OpenStack (*UsageSource).
type UsageMeter interface {
	DayUsage(start, end time.Time) (map[string]usage.Measured, error)
	PublicIPv4() (map[string]int, error)
}

// SetUsage makes every pass also report the days not recorded yet. It reads
// from OpenStack only, so it runs in dry-run mode as well.
func (r *Reconciler) SetUsage(books UsageBooks, meter UsageMeter) {
	r.usageBooks, r.usageMeter = books, meter
}

// recordUsage measures each day the books are missing and reports it, oldest
// first. Returns how many days it recorded. A failed count of addresses leaves
// the day without them rather than failing it.
func (r *Reconciler) recordUsage(ctx context.Context) (int, error) {
	days, err := r.usageBooks.DaysToRecord(ctx)
	if err != nil {
		return 0, err
	}
	for i, day := range days {
		measured, err := r.usageMeter.DayUsage(day, day.AddDate(0, 0, 1))
		if err != nil {
			return i, err
		}
		report := usage.DayReport{Usage: measured}
		if r.usageBooks.Sampled(day) {
			if report.PublicIPv4, err = r.usageMeter.PublicIPv4(); err != nil {
				r.log.Warnw("Usage: could not count public IPv4 addresses, the day goes without", "error", err)
				report.PublicIPv4 = nil
			}
		}
		if err := r.usageBooks.RecordDay(ctx, day, report); err != nil {
			return i, err
		}
	}
	return len(days), nil
}
