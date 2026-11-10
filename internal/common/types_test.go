package common

import "testing"

func TestProjectQuota_HoldsAndGrants(t *testing.T) {
	q := ProjectQuota{"zero": 0, "one": 1, "five": 5, "unlimited": UnlimitedQuota, "minus2": -2}
	for _, c := range []struct {
		id            string
		holds, grants bool
	}{
		{"zero", false, false},
		{"one", true, true},
		{"five", true, false},
		{"unlimited", true, false}, // -1 is no grant of an availability (decided 10.11.2026)
		{"minus2", false, false},
		{"missing", false, false},
	} {
		if got := q.Holds(c.id); got != c.holds {
			t.Errorf("Holds(%s) = %v, want %v", c.id, got, c.holds)
		}
		if got := q.Grants(c.id); got != c.grants {
			t.Errorf("Grants(%s) = %v, want %v", c.id, got, c.grants)
		}
	}
	var none ProjectQuota
	if none.Holds("x") || none.Grants("x") {
		t.Error("a nil quota holds and grants nothing")
	}
}
