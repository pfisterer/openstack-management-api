package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/usage"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// Nova reports memory in MB-hours; the books keep GB-hours.
func TestMeasuredFrom(t *testing.T) {
	got := measuredFrom([]osclient.TenantUsage{{ProjectID: "os-1", ServerHours: 24, VCPUHours: 96, MemoryMBHours: 24 * 8192, LocalGBHours: 480}})
	m := got["os-1"]
	if m.ServerHours != 24 || m.VCPUHours != 96 || m.RAMGBHours != 192 || m.DiskGBHours != 480 {
		t.Errorf("got %+v", m)
	}
}

type fakeBooks struct {
	days     []time.Time
	sampled  time.Time
	recorded map[time.Time]usage.DayReport
}

func (f *fakeBooks) DaysToRecord(context.Context) ([]time.Time, error) { return f.days, nil }
func (f *fakeBooks) Sampled(day time.Time) bool                        { return day.Equal(f.sampled) }
func (f *fakeBooks) RecordDay(_ context.Context, day time.Time, r usage.DayReport) error {
	f.recorded[day] = r
	return nil
}

type fakeMeter struct {
	asked   [][2]time.Time
	ipv4    map[string]int
	ipv4Err error
	ipv4N   int
}

func (f *fakeMeter) DayUsage(start, end time.Time) (map[string]usage.Measured, error) {
	f.asked = append(f.asked, [2]time.Time{start, end})
	return map[string]usage.Measured{"os-1": {VCPUHours: 1}}, nil
}

func (f *fakeMeter) PublicIPv4() (map[string]int, error) {
	f.ipv4N++
	return f.ipv4, f.ipv4Err
}

// Every missing day is measured once, from midnight to midnight in Europe/Berlin, and reported; addresses are counted only
// for the sampled day, and a failed count reports none instead of failing.
func TestRecordUsage(t *testing.T) {
	d1 := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(0, 0, 1)
	books := &fakeBooks{days: []time.Time{d1, d2}, sampled: d2, recorded: map[time.Time]usage.DayReport{}}
	meter := &fakeMeter{ipv4: map[string]int{"os-1": 2}}
	r := &Reconciler{log: zap.NewNop().Sugar()}
	r.SetUsage(books, meter)

	if n, err := r.recordUsage(context.Background()); err != nil || n != 2 {
		t.Fatalf("recorded %d, %v", n, err)
	}
	if len(meter.asked) != 2 || meter.ipv4N != 1 {
		t.Fatalf("asked %v, addresses counted %d times", meter.asked, meter.ipv4N)
	}
	if s, e := meter.asked[0][0], meter.asked[0][1]; !s.Equal(time.Date(2026, 10, 2, 22, 0, 0, 0, time.UTC)) || !e.Equal(time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("3 October measured %v – %v, want midnight to midnight in Berlin (summer time)", s, e)
	}
	if books.recorded[d1].PublicIPv4 != nil || books.recorded[d2].PublicIPv4["os-1"] != 2 || books.recorded[d1].Usage["os-1"].VCPUHours != 1 {
		t.Errorf("reports: %+v", books.recorded)
	}

	meter.ipv4Err = errors.New("neutron down")
	books.days = []time.Time{d2}
	if _, err := r.recordUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if books.recorded[d2].PublicIPv4 != nil {
		t.Error("a failed count was reported")
	}
}

// The night after the switch day the reconciler asks Nova from midnight UTC, where the last UTC row ended, to midnight in Berlin; the day after is a whole Berlin day.
func TestRecordUsage_SwitchDay(t *testing.T) {
	d1 := usage.BerlinSince
	d2 := d1.AddDate(0, 0, 1)
	books := &fakeBooks{days: []time.Time{d1, d2}, sampled: d2, recorded: map[time.Time]usage.DayReport{}}
	meter := &fakeMeter{ipv4: map[string]int{}}
	r := &Reconciler{log: zap.NewNop().Sugar()}
	r.SetUsage(books, meter)

	if n, err := r.recordUsage(context.Background()); err != nil || n != 2 {
		t.Fatalf("recorded %d, %v", n, err)
	}
	want := [][2]time.Time{
		{time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC), time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC)},
	}
	for i, w := range want {
		if s, e := meter.asked[i][0], meter.asked[i][1]; !s.Equal(w[0]) || !e.Equal(w[1]) {
			t.Errorf("day %d measured %v – %v, want %v – %v", i, s, e, w[0], w[1])
		}
	}
}
