package tree_test

import (
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

func TestResolveAttributes_NearestGroupWinsWhole(t *testing.T) {
	chain := []tree.Node{
		{ID: "p", Name: "Thesis", Attributes: tree.Attributes{"billing": {"wbs": "D-1"}}},
		{ID: "stud", Name: "Students", Attributes: tree.Attributes{"contact": {"mail": "fin@x"}, "misc": {}}},
		{ID: "uni", Name: "Uni", Attributes: tree.Attributes{"billing": {"cost_center": "4711"}, "misc": {"a": "b"}}},
	}
	got := tree.ResolveAttributes(chain)

	// Whole group from the nearest node: no cost centre from Uni next to the
	// project's own WBS element.
	if b := got["billing"]; b.From.ID != "p" || len(b.Values) != 1 || b.Values["wbs"] != "D-1" {
		t.Errorf("billing = %+v", b)
	}
	if c := got["contact"]; c.From.ID != "stud" || c.From.Name != "Students" || c.Values["mail"] != "fin@x" {
		t.Errorf("contact = %+v", c)
	}
	// An empty group stops the inheritance.
	if _, ok := got["misc"]; ok {
		t.Errorf("misc should be cut off by Students' empty group: %+v", got["misc"])
	}
}

func TestSetAttributes_RightsAndInheritance(t *testing.T) {
	f := newAllocationFixture(t)
	set := func(id string, tokens common.TokenList, a tree.Attributes) error {
		_, err := f.svc.SetAttributes(id, tree.SetAttributesRequest{Attributes: a}, tree.UIActor("x@x"), tokens)
		return err
	}

	// The project's owner says nothing about who pays.
	if err := set(f.project.ID, ownerToken, tree.Attributes{"billing": {"cost_center": "1"}}); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("owner: want forbidden, got %v", err)
	}
	// A budget's own managers may set its attributes; the university's too.
	if err := set(f.stud.ID, studMgr, tree.Attributes{"billing": {"cost_center": "200"}}); err != nil {
		t.Fatalf("students' manager on their budget: %v", err)
	}
	if err := set(f.uni.ID, uniTokens, tree.Attributes{"billing": {"cost_center": "100"}, "contact": {"mail": "fin@x"}}); err != nil {
		t.Fatalf("uni manager: %v", err)
	}

	p := mustGet(t, f.svc, f.project.ID)
	if b := p.EffectiveAttributes["billing"]; b.From.ID != f.stud.ID || b.Values["cost_center"] != "200" {
		t.Errorf("project billing = %+v, want the students' budget's", b)
	}
	if c := p.EffectiveAttributes["contact"]; c.From.ID != f.uni.ID {
		t.Errorf("project contact = %+v, want the university's", c)
	}

	// The students' manager overrides it on the project.
	n, err := f.svc.SetAttributes(f.project.ID, tree.SetAttributesRequest{Attributes: tree.Attributes{"billing": {"wbs": "D-9"}}},
		tree.UIActor("mgr@x"), studMgr)
	if err != nil {
		t.Fatalf("students' manager on the project: %v", err)
	}
	if b := n.EffectiveAttributes["billing"]; b.From.ID != f.project.ID || b.Values["cost_center"] != "" {
		t.Errorf("after the override billing = %+v", b)
	}
	if last := n.History[len(n.History)-1]; last.Event != "attributes_changed" || last.AttributesTo["billing"]["wbs"] != "D-9" {
		t.Errorf("history = %+v", last)
	}

	// Clearing them inherits again.
	if err := set(f.project.ID, studMgr, nil); err != nil {
		t.Fatal(err)
	}
	if p := mustGet(t, f.svc, f.project.ID); p.Attributes != nil || p.EffectiveAttributes["billing"].From.ID != f.stud.ID {
		t.Errorf("after clearing: own %v, billing %+v", p.Attributes, p.EffectiveAttributes["billing"])
	}
}

func TestSetAttributes_Validation(t *testing.T) {
	f := newAllocationFixture(t)
	for name, a := range map[string]tree.Attributes{
		"upper-case group": {"Billing": {"a": "1"}},
		"space in key":     {"billing": {"cost center": "1"}},
		"long value":       {"billing": {"a": string(make([]byte, 600))}},
	} {
		if _, err := f.svc.SetAttributes(f.uni.ID, tree.SetAttributesRequest{Attributes: a}, tree.UIActor("root@x"), rootTokens); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Attributes go to those who look after a node: its managers and those above,
// and on a project its owner and admins — not to members or requesters.
func TestAttributes_VisibleToThoseWhoLookAfterTheNode(t *testing.T) {
	f := newAllocationFixture(t)
	if _, err := f.svc.SetAttributes(f.uni.ID, tree.SetAttributesRequest{Attributes: tree.Attributes{"billing": {"cost_center": "100"}}},
		tree.UIActor("uni@x"), uniTokens); err != nil {
		t.Fatal(err)
	}
	member := common.TokenList{"user:member@x"}
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.stud.ID, Kind: tree.KindProject, Name: "Shared", Reason: "t",
		Limit:           common.ProjectQuota{"cores": 0},
		AuthorizedUsers: []common.AuthorizedUser{{Token: "user:member@x", OpenstackRole: "member"}},
	}, tree.UIActor("stud@x"), "stud@x", ownerToken)
	if err != nil {
		t.Fatalf("create shared project: %v", err)
	}

	sees := func(id string, tokens common.TokenList) bool {
		t.Helper()
		n, err := f.svc.GetNode(id, tokens)
		if err != nil || n == nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return n.EffectiveAttributes["billing"].Values["cost_center"] == "100"
	}
	for name, c := range map[string]struct {
		id     string
		tokens common.TokenList
		want   bool
	}{
		"owner of the project":           {p.ID, ownerToken, true},
		"manager of the students budget": {p.ID, studMgr, true},
		"member of the project":          {p.ID, member, false},
		"requester under the budget":     {f.stud.ID, ownerToken, false},
		"manager above":                  {f.stud.ID, uniTokens, true},
	} {
		if got := sees(c.id, c.tokens); got != c.want {
			t.Errorf("%s: sees attributes = %v, want %v", name, got, c.want)
		}
	}
}
