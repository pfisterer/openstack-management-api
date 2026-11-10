package model

import "testing"

func TestNormalizeEnd(t *testing.T) {
	for in, want := range map[string]string{
		"2026-12-31":                "2026-12-31",
		" 2026-12-31 ":              "2026-12-31",
		"2026-12-30T23:00:00.000Z":  "2026-12-31", // the picker: 31 December at local midnight (winter)
		"2026-06-29T22:00:00.000Z":  "2026-06-30", // the same in summer time
		"2026-12-31T00:00:00.000Z":  "2026-12-31", // a stored bare day sent back by the form
		"2026-12-31T00:00:00Z":      "2026-12-31",
		"2026-12-31T23:59:59Z":      "2026-12-31", // the former end of a term: its UTC date
		"2026-06-30T23:59:59Z":      "2026-06-30",
		"2026-06-30T23:30:00Z":      "2026-07-01", // 01:30 in Berlin
		"2027-06-30T01:00:00+02:00": "2027-06-30",
		"2027-02-08T09:13:27.123Z":  "2027-02-08", // a date from the duration field
	} {
		s := in
		got, err := NormalizeEnd(&s)
		if err != nil || *got != want {
			t.Errorf("NormalizeEnd(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	if got, err := NormalizeEnd(nil); got != nil || err != nil {
		t.Error("nil must stay nil")
	}
	bad := "31.12.2026"
	if _, err := NormalizeEnd(&bad); err == nil {
		t.Error("31.12.2026 accepted")
	}
}

func TestSameEndAcrossForms(t *testing.T) {
	ptr := func(s string) *string { return &s }
	same := [][2]string{
		{"2026-12-31", "2026-12-31T00:00:00.000Z"},
		{"2026-12-31", "2026-12-30T23:00:00.000Z"},
		{"2026-12-31", "2026-12-31T23:59:59Z"},
		{"2026-12-30T23:00:00.000Z", "2026-12-31T23:59:59Z"},
	}
	for _, p := range same {
		if !SameEnd(ptr(p[0]), ptr(p[1])) || !EndsNoLater(p[0], ptr(p[1])) || !EndsNoLater(p[1], ptr(p[0])) {
			t.Errorf("%s and %s must be the same end", p[0], p[1])
		}
	}
	if SameEnd(ptr("2026-12-31"), nil) || !SameEnd(nil, nil) || SameEnd(ptr("2026-12-31"), ptr("2027-01-01")) {
		t.Error("SameEnd")
	}
	if !SameEnd(ptr("kaputt"), ptr("kaputt")) || SameEnd(ptr("kaputt"), ptr("2026-12-31")) {
		t.Error("SameEnd with unreadable")
	}
}

func TestEndsNoLater(t *testing.T) {
	ptr := func(s string) *string { return &s }
	if !EndsNoLater("2026-12-31", nil) || !EndsNoLater("2026-12-30", ptr("2026-12-31")) || EndsNoLater("2027-01-01", ptr("2026-12-31T23:59:59Z")) {
		t.Error("order")
	}
	if EndsNoLater("kaputt", ptr("2026-12-31")) {
		t.Error("an unreadable end must count as later")
	}
	if !EndsNoLater("2026-12-31", ptr("kaputt")) {
		t.Error("an unreadable bound must bound nothing")
	}
}

func TestEndLabel(t *testing.T) {
	if EndLabel("2026-12-30T23:00:00.000Z") != "2026-12-31" || EndLabel("kaputt") != "kaputt" {
		t.Error("EndLabel")
	}
}
