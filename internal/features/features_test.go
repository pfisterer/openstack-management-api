package features

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

func TestGates(t *testing.T) {
	g, err := Parse(`{"estimator": ["Group:Leiter-ZWR"], "assistant": ["*"], "closed": [], "solo": ["user:A@example.org"]}`)
	if err != nil {
		t.Fatal(err)
	}
	zwr := common.TokenList{"user:x@example.org", "group:leiter-zwr"}
	member := common.TokenList{"user:y@example.org", "group:students"}
	cases := []struct {
		gate   string
		tokens common.TokenList
		want   bool
	}{
		{"estimator", zwr, true},
		{"estimator", member, false},
		{"assistant", member, true},
		{"closed", zwr, false},
		{"solo", common.TokenList{"user:a@example.org"}, true},
		{"solo", member, false},
		{"unconfigured", member, true},
	}
	for _, c := range cases {
		if got := g.Open(c.gate, c.tokens); got != c.want {
			t.Errorf("%s for %v: got %v, want %v", c.gate, c.tokens, got, c.want)
		}
	}
	all := g.For(member)
	if !all["assistant"] || all["estimator"] || all["closed"] || len(all) != 4 {
		t.Errorf("For: %v", all)
	}
}

func TestParse(t *testing.T) {
	if g, err := Parse(""); err != nil || len(g) != 0 || !g.Open("estimator", nil) {
		t.Errorf("empty: %v %v", g, err)
	}
	if g, err := Parse(`{"estimator": null}`); err != nil || g.Open("estimator", common.TokenList{"group:x"}) {
		t.Errorf("null closes the gate like []: %v %v", g, err)
	}
	if _, err := Parse(`["estimator"]`); err == nil {
		t.Error("a list is not a configuration")
	}
	if _, err := Parse(`{"": ["*"]}`); err == nil {
		t.Error("a gate without a name")
	}
}
