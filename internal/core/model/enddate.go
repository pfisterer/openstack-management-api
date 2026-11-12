package model

import (
	"strings"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/day"
)

// End dates (Node.TerminationDate, Pending.TerminationDate) are days, compared as days in Europe/Berlin and stored as "2006-01-02" (decided 10.11.2026, docs/refactoring-audit-budget-baum.md). Before, they were stored as a bare day, as the portal's picker sent them ("2026-12-30T23:00:00.000Z" for 31 December), and as the end of a term ("2026-12-31T23:59:59Z"), and compared as instants or strings; a mere change of members could then look like an extension. Every reading and writing of an end goes through EndDay and NormalizeEnd.

// EndDay reads an end date as its day: a bare day as it is, a timestamp as its day in Berlin (the portal's picker sends local midnight). Exception: a timestamp at 23:59:59 UTC is the former end of a term, which named its UTC date (in Berlin it would already be the next day).
func EndDay(s string) (day.Date, error) {
	v := strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		if u := t.UTC(); u.Hour() == 23 && u.Minute() == 59 && u.Second() == 59 {
			return day.ParseUTCDate(v)
		}
	}
	d, err := day.Parse(v)
	if err != nil {
		return day.Date{}, invalid("termination_date %q ist weder ein Tag (2006-01-02) noch ein Zeitpunkt nach RFC 3339.", "termination_date %q is neither a day (2006-01-02) nor an RFC 3339 timestamp.", s)
	}
	return d, nil
}

// NormalizeEnd is an end date from a request or the store as it is stored: "2006-01-02"; nil stays nil, an unreadable one is an error.
func NormalizeEnd(s *string) (*string, error) {
	if s == nil {
		return nil, nil
	}
	d, err := EndDay(*s)
	if err != nil {
		return nil, err
	}
	v := d.String()
	return &v, nil
}

// EndLabel is an end date for a message: its day, or the text itself when it cannot be read.
func EndLabel(s string) string {
	if d, err := EndDay(s); err == nil {
		return d.String()
	}
	return s
}

// SameEnd reports whether two optional end dates name the same day (both nil is the same; unreadable ones only when written alike).
func SameEnd(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	da, errA := EndDay(*a)
	db, errB := EndDay(*b)
	if errA != nil || errB != nil {
		return *a == *b
	}
	return da == db
}

// EndsNoLater reports whether the end next is not after bound; a nil bound means "no end" and bounds nothing. An end that cannot be read counts as later (decided 10.11.2026): it takes the path through a human rather than past one.
func EndsNoLater(next string, bound *string) bool {
	if bound == nil {
		return true
	}
	n, err := EndDay(next)
	if err != nil {
		return false
	}
	b, err := EndDay(*bound)
	if err != nil {
		return true // an unreadable bound is later than anything, so it bounds nothing
	}
	return !n.After(b)
}
