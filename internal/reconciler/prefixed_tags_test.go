package reconciler

import (
	"slices"
	"testing"
)

const (
	statusPrefix = "status:"
	termPrefix   = "termination:"
)

func TestApplyPrefixedTags_AddsMissingTag(t *testing.T) {
	got, changed := applyPrefixedTags([]string{"managed"},
		prefixedTag{statusPrefix, "approved"})

	if !changed {
		t.Error("adding a tag that was not there must count as a change")
	}
	if !slices.Contains(got, "status:approved") || !slices.Contains(got, "managed") {
		t.Errorf("got %v, want the new tag alongside the existing one", got)
	}
}

// The case the status tag exists for: approved → released has to REPLACE, not
// accumulate, or a workflow selecting on status finds the project under both.
func TestApplyPrefixedTags_ReplacesRatherThanAccumulates(t *testing.T) {
	got, changed := applyPrefixedTags(
		[]string{"managed", "status:approved", "contact:a@b.c"},
		prefixedTag{statusPrefix, "released"})

	if !changed {
		t.Error("a replaced value must count as a change")
	}
	if slices.Contains(got, "status:approved") {
		t.Errorf("got %v, still carries the old status", got)
	}
	if !slices.Contains(got, "status:released") {
		t.Errorf("got %v, want status:released", got)
	}
	if !slices.Contains(got, "contact:a@b.c") || !slices.Contains(got, "managed") {
		t.Errorf("got %v, foreign tags must survive untouched", got)
	}
}

// This runs for every managed leaf on every tick. Reporting "changed" for an
// unchanged value would be one Keystone write per project per interval, forever.
func TestApplyPrefixedTags_UnchangedValueIsNotAChange(t *testing.T) {
	tags := []string{"managed", "status:approved", "termination:2026-11-03T00:00:00Z"}
	got, changed := applyPrefixedTags(tags,
		prefixedTag{statusPrefix, "approved"},
		prefixedTag{termPrefix, "2026-11-03T00:00:00Z"})

	if changed {
		t.Errorf("got %v marked as changed, but every value is already correct", got)
	}
}

// Order is not content. The owned tags are re-appended at the end, so a check
// that compared the lists positionally would fire on every run — the bug this
// test exists to keep out.
func TestApplyPrefixedTags_ReorderingIsNotAChange(t *testing.T) {
	tags := []string{"status:approved", "managed", "termination:2026-11-03T00:00:00Z"}
	if _, changed := applyPrefixedTags(tags,
		prefixedTag{statusPrefix, "approved"},
		prefixedTag{termPrefix, "2026-11-03T00:00:00Z"},
	); changed {
		t.Error("the same tags in a different order must not count as a change")
	}
}

func TestApplyPrefixedTags_EmptyValueRemovesTheTag(t *testing.T) {
	got, changed := applyPrefixedTags(
		[]string{"managed", "termination:2026-11-03T00:00:00Z"},
		prefixedTag{termPrefix, ""})

	if !changed {
		t.Error("removing a tag must count as a change")
	}
	if slices.Contains(got, "termination:2026-11-03T00:00:00Z") {
		t.Errorf("got %v, want the termination tag gone", got)
	}
	if !slices.Contains(got, "managed") {
		t.Errorf("got %v, want the foreign tag kept", got)
	}
}

// An empty prefix is how a tag is switched off by configuration. It must not
// match every tag and wipe the list.
func TestApplyPrefixedTags_EmptyPrefixTouchesNothing(t *testing.T) {
	tags := []string{"managed", "status:approved"}
	got, changed := applyPrefixedTags(tags, prefixedTag{"", "released"})

	if changed || !slices.Equal(got, tags) {
		t.Errorf("got %v (changed=%v), want the input untouched", got, changed)
	}
}

// Both prefixes in one pass: two separate rebuilds from the same snapshot would
// each carry the other's old value back, so the second write undoes the first.
func TestApplyPrefixedTags_BothTagsChangeInOnePass(t *testing.T) {
	got, changed := applyPrefixedTags(
		[]string{"managed", "status:approved", "termination:2026-01-01T00:00:00Z"},
		prefixedTag{statusPrefix, "released"},
		prefixedTag{termPrefix, "2026-11-03T00:00:00Z"})

	if !changed {
		t.Error("two changed values must count as a change")
	}
	for _, want := range []string{"managed", "status:released", "termination:2026-11-03T00:00:00Z"} {
		if !slices.Contains(got, want) {
			t.Errorf("got %v, missing %q", got, want)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %v, want exactly three tags — no leftovers", got)
	}
}
