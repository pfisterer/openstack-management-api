package tree

import (
	"slices"
	"testing"
)

// What is worth testing about a migration that drops tables is not the dropping
// — that is HasTable followed by DropTable — but the list of names. A wrong name
// there destroys a live table, and it would do so silently, on the next start,
// in every environment at once.
//
// So this holds the list against the tables the model actually uses, derived
// from the models rather than typed out again, so that a table added later is
// covered without anyone remembering to come back here.
//
// The dropping itself is deliberately not exercised: this package has no test
// database (its tests run against the in-memory store), and pulling in a SQLite
// driver to watch GORM execute one DROP TABLE would test GORM, not us.
func TestLegacyTables_NeverNamesATableTheModelUses(t *testing.T) {
	inUse := []string{
		nodeRow{}.TableName(),
		identityRow{}.TableName(),
		// Not a model in this package — it belongs to the shared token library —
		// but it lives in the same database and would be just as gone.
		"tokens",
	}

	for _, table := range legacyTables {
		if slices.Contains(inUse, table) {
			t.Errorf("legacyTables names %q, which the model uses: this migration would drop live data", table)
		}
	}
}

// The list is the whole blast radius, so it should not grow by accident. If a
// fourth table genuinely has to go, this test is the place where somebody says
// so out loud.
func TestLegacyTables_IsExactlyTheThreeFromBeforeTheRewrite(t *testing.T) {
	want := []string{"delegations", "eligibility_rules", "projects"}

	got := slices.Clone(legacyTables)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("legacyTables is %v, want %v — if that change is intended, update this test with the reason", got, want)
	}
}
