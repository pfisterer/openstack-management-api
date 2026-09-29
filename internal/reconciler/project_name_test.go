package reconciler

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pfisterer/openstack-management-api/internal/tree"
)

func TestBuildProjectName(t *testing.T) {
	tests := []struct {
		name string
		leaf tree.Node
		want string
	}{
		{
			name: "name and id",
			leaf: tree.Node{ID: "p_001", Name: "Cloud Computing WS26/27"},
			want: "Cloud Computing WS26/27 [p_001]",
		},
		{
			name: "umlauts and punctuation survive",
			leaf: tree.Node{ID: "p_002", Name: "Prüfung Ökonomie & Recht (2026)"},
			want: "Prüfung Ökonomie & Recht (2026) [p_002]",
		},
		{
			name: "leaf without a name falls back to the id",
			leaf: tree.Node{ID: "p_003"},
			want: "p_003",
		},
		{
			name: "whitespace-only name falls back to the id",
			leaf: tree.Node{ID: "p_004", Name: "   \t "},
			want: "p_004",
		},
		{
			name: "whitespace is collapsed and trimmed",
			leaf: tree.Node{ID: "p_005", Name: "  Data   Science\nLab  "},
			want: "Data Science Lab [p_005]",
		},
		{
			name: "emoji and control characters are dropped",
			leaf: tree.Node{ID: "p_006", Name: "Rocket 🚀 Lab\x07"},
			want: "Rocket Lab [p_006]",
		},
		{
			name: "long name is truncated so the id suffix still fits",
			leaf: tree.Node{ID: "p_007", Name: strings.Repeat("a", 80)},
			want: strings.Repeat("a", 64-len(" [p_007]")) + " [p_007]",
		},
		{
			// A real node ID is a UUID; all of it would bury the purpose the
			// name exists to convey.
			name: "a uuid node id is shortened to its first block",
			leaf: tree.Node{ID: "p_7ad31c42-21e7-4fbd-aa3e-15a4660449be", Reason: "asfasf"},
			want: "asfasf [p_7ad31c42]",
		},
		{
			name: "a uuid node id without a name still identifies the project",
			leaf: tree.Node{ID: "p_7ad31c42-21e7-4fbd-aa3e-15a4660449be"},
			want: "p_7ad31c42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildProjectName(tc.leaf)
			if got != tc.want {
				t.Errorf("buildProjectName() = %q, want %q", got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n < 1 || n > keystoneProjectNameMaxLen {
				t.Errorf("name length %d out of Keystone's 1..%d range: %q", n, keystoneProjectNameMaxLen, got)
			}
		})
	}
}

// Keystone counts characters, not bytes: a multi-byte name must be truncated by
// rune count or a 64-character name is rejected as too long.
func TestBuildProjectNameTruncatesByRunes(t *testing.T) {
	leaf := tree.Node{ID: "p_008", Name: strings.Repeat("ä", 80)}
	got := buildProjectName(leaf)
	if n := utf8.RuneCountInString(got); n != keystoneProjectNameMaxLen {
		t.Errorf("rune count = %d, want %d (%q)", n, keystoneProjectNameMaxLen, got)
	}
	if !strings.HasSuffix(got, " [p_008]") {
		t.Errorf("id suffix lost during truncation: %q", got)
	}
}

// A cut must not leave a dangling space in front of the id suffix.
func TestBuildProjectNameTrimsAtCut(t *testing.T) {
	leaf := tree.Node{ID: "p_009", Name: strings.Repeat("a", 55) + " tail"}
	got := buildProjectName(leaf)
	if strings.Contains(got, "  [") {
		t.Errorf("dangling space before the id suffix: %q", got)
	}
}

func TestBuildDescription(t *testing.T) {
	leaf := tree.Node{
		ID:     "p_001",
		Reason: "Praktikum Cloud",
		Owner:  "user:max.muster@dhbw.de",
	}
	got := buildDescription(leaf, nil)
	if !strings.HasPrefix(got, "max.muster@dhbw.de: ") {
		t.Errorf("owner email missing from description: %q", got)
	}
	if !strings.HasSuffix(got, managedDescriptionSuffix) {
		t.Errorf("description not marked as managed: %q", got)
	}
	if !strings.Contains(got, "Praktikum Cloud") {
		t.Errorf("reason missing from description: %q", got)
	}

	// No owner, no reason: the fallback is still marked.
	bare := buildDescription(tree.Node{ID: "p_002"}, nil)
	if !strings.HasSuffix(bare, managedDescriptionSuffix) {
		t.Errorf("fallback description not marked as managed: %q", bare)
	}
}

// The description names the leaf's budgets, top-down and without the root, so an
// operator in Horizon can tell where the project belongs.
func TestBuildDescriptionWithBudgetPath(t *testing.T) {
	ptr := func(s string) *string { return &s }
	budgetByID := map[string]tree.Node{
		tree.RootNodeID: {ID: tree.RootNodeID, Name: "Organization Root"},
		"b_ma":          {ID: "b_ma", Name: "DHBW Mannheim", ParentID: ptr(tree.RootNodeID)},
		"b_tech":        {ID: "b_tech", Name: "Fakultät Technik", ParentID: ptr("b_ma")},
		"b_prof":        {ID: "b_prof", Name: "Prof-X", ParentID: ptr("b_tech")},
	}
	leaf := tree.Node{
		ID:       "p_001",
		ParentID: ptr("b_prof"),
		Reason:   "Linux-Übungen",
		Owner:    "user:max.muster@dhbw.de",
	}
	got := buildDescription(leaf, budgetPath(budgetByID, leaf))
	want := "DHBW Mannheim -> Fakultät Technik -> Prof-X -> max.muster@dhbw.de: Linux-Übungen (managed project)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Without an owner the path still leads the description.
	leaf.Owner = ""
	got = buildDescription(leaf, budgetPath(budgetByID, leaf))
	want = "DHBW Mannheim -> Fakultät Technik -> Prof-X: Linux-Übungen (managed project)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A budget that did not load ends the path rather than failing it.
	delete(budgetByID, "b_tech")
	if path := budgetPath(budgetByID, leaf); !slices.Equal(path, []string{"Prof-X"}) {
		t.Errorf("path with a missing budget = %q", path)
	}
}

// A requested project has no name — only a purpose. Naming it after its bare
// node ID is what a user then sees in Horizon, so the purpose has to carry it.
func TestBuildProjectNameFallsBackToReason(t *testing.T) {
	leaf := tree.Node{ID: "p_7ad31c42", Reason: "Lab exercises for Distributed Systems"}
	got := buildProjectName(leaf)
	want := "Lab exercises for Distributed Systems [p_7ad31c42]"
	if got != want {
		t.Errorf("buildProjectName() = %q, want %q", got, want)
	}

	// A real name still wins over the purpose.
	both := tree.Node{ID: "p_1", Name: "Cloud Computing", Reason: "some purpose"}
	if got := buildProjectName(both); got != "Cloud Computing [p_1]" {
		t.Errorf("name should win over reason, got %q", got)
	}
}

// The short form is only ever the front of the ID: everything that is not a
// "<kind>_<uuid>" must survive untouched, or a structural node would end up
// with a name that points nowhere.
func TestShortNodeID(t *testing.T) {
	tests := map[string]string{
		"p_7ad31c42-21e7-4fbd-aa3e-15a4660449be": "p_7ad31c42",
		"b_03c80ac1-9311-4825-9dd6-ba749f9a7a2b": "b_03c80ac1",
		"root":                                   "root",       // structural node, no UUID
		"unassigned":                             "unassigned", // structural node, no UUID
		"p_001":                                  "p_001",      // seeded/test IDs are not UUIDs
		"p_":                                     "p_",
	}
	for id, want := range tests {
		if got := shortNodeID(id); got != want {
			t.Errorf("shortNodeID(%q) = %q, want %q", id, got, want)
		}
	}
}
