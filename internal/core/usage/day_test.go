package usage

import (
	"context"
	"testing"
	"time"
)

func utcDate(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// A moment counts for its day in Europe/Berlin: midnight there in winter (UTC+1) and summer (UTC+2), and around both changes of the clocks, 25.10.2026 and 28.03.2027.
func TestDayOf_Berlin(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   string
		want time.Time
	}{
		{"winter, just before midnight in Berlin", "2026-12-01T22:59:59Z", utcDate(2026, 12, 1)},
		{"winter, midnight in Berlin", "2026-12-01T23:00:00Z", utcDate(2026, 12, 2)},
		{"summer, just before midnight in Berlin", "2026-07-01T21:59:59Z", utcDate(2026, 7, 1)},
		{"summer, midnight in Berlin", "2026-07-01T22:00:00Z", utcDate(2026, 7, 2)},
		{"clocks go back: midnight in Berlin is still summer time", "2026-10-24T22:00:00Z", utcDate(2026, 10, 25)},
		{"clocks go back: the repeated hour stays on the day", "2026-10-25T01:30:00Z", utcDate(2026, 10, 25)},
		{"clocks go back: the day ends at midnight winter time", "2026-10-25T22:59:59Z", utcDate(2026, 10, 25)},
		{"clocks go back: the next day", "2026-10-25T23:00:00Z", utcDate(2026, 10, 26)},
		{"clocks go forward: midnight in Berlin is still winter time", "2027-03-27T23:00:00Z", utcDate(2027, 3, 28)},
		{"clocks go forward: the day ends at midnight summer time", "2027-03-28T21:59:59Z", utcDate(2027, 3, 28)},
		{"clocks go forward: the next day", "2027-03-28T22:00:00Z", utcDate(2027, 3, 29)},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		if got := DayOf(at); !got.Equal(tc.want) || got.Location() != time.UTC {
			t.Errorf("%s: DayOf(%s) = %v, want %v", tc.name, tc.at, got, tc.want)
		}
	}
}

// A stored day is measured from midnight to midnight in Berlin: 24 hours in winter and summer, 25 when the clocks go back and 23 when they go forward.
func TestBounds_Berlin(t *testing.T) {
	for _, tc := range []struct {
		day          time.Time
		start, hours string
	}{
		{utcDate(2026, 12, 2), "2026-12-01T23:00:00Z", "24h0m0s"},
		{utcDate(2026, 7, 2), "2026-07-01T22:00:00Z", "24h0m0s"},
		{utcDate(2026, 10, 25), "2026-10-24T22:00:00Z", "25h0m0s"},
		{utcDate(2027, 3, 28), "2027-03-27T23:00:00Z", "23h0m0s"},
	} {
		start, end := Bounds(tc.day)
		if got := start.UTC().Format(time.RFC3339); got != tc.start || end.Sub(start).String() != tc.hours {
			t.Errorf("Bounds(%s) = %s + %v, want %s + %s", tc.day.Format(time.DateOnly), got, end.Sub(start), tc.start, tc.hours)
		}
	}
}

// The switch day starts where the last UTC day (11.11.2026, up to midnight UTC) ended, so the hour before midnight UTC is not counted twice; it ends at midnight in Berlin like every later day, and the days around it are untouched.
func TestBounds_SwitchDay(t *testing.T) {
	for _, tc := range []struct {
		day        time.Time
		start, end string
	}{
		{utcDate(2026, 11, 12), "2026-11-12T00:00:00Z", "2026-11-12T23:00:00Z"},
		{utcDate(2026, 11, 13), "2026-11-12T23:00:00Z", "2026-11-13T23:00:00Z"},
		{utcDate(2026, 11, 14), "2026-11-13T23:00:00Z", "2026-11-14T23:00:00Z"},
		{utcDate(2026, 11, 11), "2026-11-10T23:00:00Z", "2026-11-11T23:00:00Z"},
		{utcDate(2027, 11, 12), "2027-11-11T23:00:00Z", "2027-11-12T23:00:00Z"},
	} {
		start, end := Bounds(tc.day)
		if s, e := start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339); s != tc.start || e != tc.end {
			t.Errorf("Bounds(%s) = %s – %s, want %s – %s", tc.day.Format(time.DateOnly), s, e, tc.start, tc.end)
		}
	}
	// Adjacent: the last UTC row ends at midnight UTC of 12.11., where the switch day starts.
	lastUTCEnd := utcDate(2026, 11, 11).AddDate(0, 0, 1)
	if start, _ := Bounds(BerlinSince); !start.Equal(lastUTCEnd) {
		t.Errorf("the switch day starts at %v, the last UTC day ended at %v", start, lastUTCEnd)
	}
}

// Right after midnight in Berlin, while it is still the day before in UTC, the day that just ended is finished and recorded, and it is the sampled one; in summer as in winter.
func TestDaysToRecord_BerlinMidnight(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 11, 12, 23, 30, 0, 0, time.UTC), // 13.11. 00:30 in Berlin
		time.Date(2027, 7, 12, 22, 30, 0, 0, time.UTC),  // 13.07. 00:30 in Berlin
	} {
		c, store := fixture(t)
		ctx := context.Background()
		c.now = func() time.Time { return now }
		last := utcDate(now.Year(), now.Month(), 11)
		if err := store.ReplaceDay(ctx, last, nil); err != nil {
			t.Fatal(err)
		}
		days, err := c.DaysToRecord(ctx)
		want := utcDate(now.Year(), now.Month(), 12)
		if err != nil || len(days) != 1 || !days[0].Equal(want) {
			t.Fatalf("at %v: days to record %v, %v; want %v", now, days, err, want)
		}
		if !c.Sampled(want) || c.Sampled(last) {
			t.Errorf("at %v: the day that just ended in Berlin is the sampled one", now)
		}
	}
}
