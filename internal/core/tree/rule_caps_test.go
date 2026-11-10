package tree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// Caps (common.Cap, 10.11.2026) are the providers' upper bounds a budget sets for the projects below it. The tree knows only their kinds, so these tests use a catalogue of their own: a plain number, one that may be unbounded, a choice, and a pair that is a Grant and set together. They took over from the tests of the four former settings of the AI service (release times, local storage, batch priority, batch jobs).

var testCaps = []common.Cap{
	{ID: "scratch", Name: "Scratch", Provider: "p", Unit: "GB", Min: 1, Max: 200},
	{ID: "cpu-max", Name: "CPU upper bound", Provider: "p", Unit: "h", Min: 1, Max: 720},
	{ID: "gpu-max", Name: "GPU upper bound", Provider: "p", Unit: "h", Min: 1, Max: 720, Unbounded: true},
	{ID: "prio", Name: "Priority", Provider: "p", Choices: []string{"preemptible", "guaranteed"}},
	{ID: "jobs", Name: "Jobs", Provider: "p", Min: 1, Max: 1000, Grant: true, Together: "batch"},
	{ID: "hours", Name: "Job hours", Provider: "p", Unit: "h", Min: 1, Max: 24, Grant: true, Together: "batch"},
}

func newCapsFixture(t *testing.T) changeFixture {
	t.Helper()
	f := newChangeFixture(t, nil, nil)
	if err := f.svc.SetCaps(testCaps); err != nil {
		t.Fatal(err)
	}
	return f
}

func set(kv ...any) map[string]*int {
	out := map[string]*int{}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == nil {
			out[kv[i].(string)] = nil
			continue
		}
		v := kv[i+1].(int)
		out[kv[i].(string)] = &v
	}
	return out
}

func update(f changeFixture, id string, caps map[string]*int) (model.Node, error) {
	return f.svc.UpdateNode(id, tree.UpdateNodeRequest{Caps: caps}, rootActor(), rootTokens)
}

func TestCaps_NumberOnlyTightens(t *testing.T) {
	f := newCapsFixture(t)
	for name, bad := range map[string]map[string]*int{"too small": set("scratch", 0), "too large": set("scratch", 201), "unknown": set("nope", 1), "cpu unbounded": set("cpu-max", 0)} {
		if _, err := update(f, f.budget.ID, bad); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
	b, err := update(f, f.budget.ID, set("scratch", 20))
	if err != nil {
		t.Fatal(err)
	}
	if last := b.History[len(b.History)-1]; last.CapsFrom != nil || last.CapsTo["scratch"] != 20 || len(last.CapsTo) != 1 {
		t.Errorf("the history records the changed id only: %+v %+v", last.CapsFrom, last.CapsTo)
	}
	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1)})
	if sub.Caps != nil {
		t.Errorf("nothing is copied down: %+v", sub.Caps)
	}
	if got := mustGet(t, f.svc, sub.ID).InheritedCaps["scratch"]; got != 20 {
		t.Errorf("inherited %d, want 20", got)
	}
	if _, err := update(f, sub.ID, set("scratch", 21)); err == nil || !strings.Contains(err.Error(), "at most 20 GB") {
		t.Errorf("looser below should be refused, got %v", err)
	}
	if _, err := update(f, sub.ID, set("scratch", 5)); err != nil {
		t.Fatal(err)
	}
	p := f.approved(t, 1, nil)
	if got := mustGet(t, f.svc, p.ID).InheritedCaps["scratch"]; got != 20 {
		t.Errorf("the project below the budget gets %d, want 20", got)
	}
	// Removing one is always allowed, and a value left out stays.
	if _, err := update(f, sub.ID, set("cpu-max", 10)); err != nil {
		t.Fatal(err)
	}
	after, err := update(f, sub.ID, set("scratch", nil))
	if err != nil || len(after.Caps) != 1 || after.Caps["cpu-max"] != 10 {
		t.Errorf("removing one: %v, %+v", err, after.Caps)
	}
	last := after.History[len(after.History)-1]
	if last.CapsFrom["scratch"] != 5 || last.CapsTo != nil {
		t.Errorf("the history records the removal: %+v %+v", last.CapsFrom, last.CapsTo)
	}
	cleared, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{ClearCaps: true}, rootActor(), rootTokens)
	if err != nil || cleared.Caps != nil {
		t.Errorf("null removes all: %v, %+v", err, cleared.Caps)
	}
	// Lowering above holds below at once.
	if _, err := update(f, f.budget.ID, set("scratch", 3)); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, f.svc, p.ID).InheritedCaps["scratch"]; got != 3 {
		t.Errorf("after lowering above: %d, want 3", got)
	}
}

func TestCaps_UnboundedAndChoice(t *testing.T) {
	f := newCapsFixture(t)
	if _, err := update(f, f.budget.ID, set("gpu-max", 0, "prio", 0)); err != nil {
		t.Fatalf("no bound and the strictest choice: %v", err)
	}
	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1)})
	if _, err := update(f, sub.ID, set("gpu-max", 48)); err != nil {
		t.Errorf("any bound below no bound: %v", err)
	}
	below := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: sub.ID, Kind: model.KindBudget, Limit: cores(1)})
	if _, err := update(f, below.ID, set("gpu-max", 0)); err == nil || !strings.Contains(err.Error(), "at most 48 h, so this one cannot allow no bound") {
		t.Errorf("no bound below a bound should be refused, got %v", err)
	}
	if _, err := update(f, below.ID, set("gpu-max", 49)); err == nil {
		t.Error("a longer bound below a bound should be refused")
	}
	if _, err := update(f, sub.ID, set("prio", 1)); err == nil || !strings.Contains(err.Error(), "at most preemptible, so this one cannot allow guaranteed") {
		t.Errorf("a looser choice below should be refused, got %v", err)
	}
	if _, err := update(f, sub.ID, set("prio", 2)); err == nil {
		t.Error("an index beyond the choices should be refused")
	}
}

// A Grant cap is there or not, like a resource: the root sets the most, a budget below may switch it on only where the budget above has it; one without it has none, and neither have the projects below. The pair is set together.
func TestCaps_GrantFromRoot(t *testing.T) {
	f := newCapsFixture(t)
	if got := mustGet(t, f.svc, f.budget.ID).InheritedCaps; got["jobs"] != 0 || len(got) != 0 {
		t.Fatalf("without the root's: inherited %+v, want none", got)
	}
	if _, err := update(f, f.budget.ID, set("jobs", 1, "hours", 1)); err == nil || !strings.Contains(err.Error(), "has none") {
		t.Errorf("switching on below a root without them should be refused, got %v", err)
	}
	if _, err := update(f, model.RootNodeID, set("jobs", 4)); err == nil || !strings.Contains(err.Error(), "together") {
		t.Errorf("one of a pair should be refused, got %v", err)
	}
	if _, err := update(f, model.RootNodeID, set("jobs", 40, "hours", 12)); err != nil {
		t.Fatalf("root admins set the root's: %v", err)
	}
	p := f.approved(t, 1, nil)
	if got := mustGet(t, f.svc, p.ID).InheritedCaps; len(got) != 0 {
		t.Errorf("a project below a budget without them: %+v, want none", got)
	}
	if _, err := update(f, f.budget.ID, set("jobs", 2, "hours", 4)); err != nil {
		t.Fatalf("switching on within the root's: %v", err)
	}
	if got := mustGet(t, f.svc, p.ID).InheritedCaps; got["jobs"] != 2 || got["hours"] != 4 {
		t.Errorf("the project gets the budget's: %+v", got)
	}
	if _, err := update(f, f.budget.ID, set("jobs", nil)); err == nil {
		t.Error("removing one of a pair should be refused")
	}
	if _, err := update(f, f.budget.ID, set("jobs", nil, "hours", nil)); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, f.svc, p.ID).InheritedCaps; len(got) != 0 {
		t.Errorf("after switching off: %+v, want none", got)
	}
}

// A budget that inherits its limit passes on the Grant caps above and sets none of its own; switched on it drops its own, switched off it keeps what it passed on. Its other caps are its own as before.
func TestCaps_GrantOnInheritingBudget(t *testing.T) {
	f := newCapsFixture(t)
	if _, err := update(f, model.RootNodeID, set("jobs", 4, "hours", 12)); err != nil {
		t.Fatal(err)
	}
	if _, err := update(f, f.budget.ID, set("jobs", 3, "hours", 8)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Name: "s", Reason: "t", InheritsLimit: true,
		AdminScope: common.TokenList{"group:root"}, Caps: common.Caps{"jobs": 1, "hours": 1}}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("an inheriting budget with Grant caps of its own should be refused")
	}
	structure := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, InheritsLimit: true})
	course := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: structure.ID, Kind: model.KindBudget, Limit: cores(1)})
	if got := mustGet(t, f.svc, course.ID).InheritedCaps; got["jobs"] != 3 || got["hours"] != 8 {
		t.Fatalf("below an inheriting budget: %+v, want the budget's above it", got)
	}
	if _, err := update(f, structure.ID, set("jobs", 1, "hours", 1)); err == nil {
		t.Error("Grant caps of its own on an inheriting budget should be refused")
	}
	if _, err := update(f, structure.ID, set("scratch", 7)); err != nil {
		t.Errorf("other caps on an inheriting budget are allowed: %v", err)
	}
	off, err := f.svc.UpdateNode(structure.ID, tree.UpdateNodeRequest{InheritsLimit: new(false)}, rootActor(), rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	if off.Caps["jobs"] != 3 || off.Caps["hours"] != 8 || off.Caps["scratch"] != 7 {
		t.Errorf("switched off inheriting: own %+v, want what it passed on and its own", off.Caps)
	}
	again, err := f.svc.UpdateNode(structure.ID, tree.UpdateNodeRequest{InheritsLimit: new(true)}, rootActor(), rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Caps) != 1 || again.Caps["scratch"] != 7 {
		t.Errorf("switched on inheriting: own %+v, want only its other caps", again.Caps)
	}
	// Switched off together with another cap, it still keeps what it passed on.
	both, err := f.svc.UpdateNode(structure.ID, tree.UpdateNodeRequest{InheritsLimit: new(false), Caps: set("scratch", 6)}, rootActor(), rootTokens)
	if err != nil || both.Caps["jobs"] != 3 || both.Caps["hours"] != 8 || both.Caps["scratch"] != 6 {
		t.Errorf("switched off with another cap: %v, own %+v", err, both.Caps)
	}
	if _, err := f.svc.UpdateNode(structure.ID, tree.UpdateNodeRequest{InheritsLimit: new(true)}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	again = mustGet(t, f.svc, structure.ID)
	if len(again.Caps) != 1 || again.Caps["scratch"] != 6 {
		t.Errorf("switched on inheriting: own %+v, want only its other caps", again.Caps)
	}
	if last := again.History[len(again.History)-1]; last.CapsFrom["jobs"] != 3 || last.CapsTo != nil {
		t.Errorf("the history records the dropped Grant caps: %+v %+v", last.CapsFrom, last.CapsTo)
	}
}

// Who may set them: the budget's managers and those above, like the other policy fields; not those who may only request, and not on a project.
func TestCaps_Authz(t *testing.T) {
	f := newCapsFixture(t)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Caps: set("scratch", 5)}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not set them, got %v", err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ClearCaps: true}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not clear them, got %v", err)
	}
	mgr := common.TokenList{"user:mgr@x"}
	own := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1), AdminScope: mgr})
	if _, err := f.svc.UpdateNode(own.ID, tree.UpdateNodeRequest{Caps: set("scratch", 5)}, model.UIActor("mgr@x"), mgr); err != nil {
		t.Errorf("a budget's own managers set them: %v", err)
	}
	p := f.approved(t, 1, nil)
	if _, err := update(f, p.ID, set("scratch", 5)); err == nil {
		t.Error("caps on a project should be refused")
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: "p", Reason: "t", Limit: cores(1), Caps: common.Caps{"scratch": 5}}, rootActor(), "root@x", rootTokens); err == nil || !strings.Contains(err.Error(), "caps is a budget setting") {
		t.Errorf("caps on a new project should be refused, got %v", err)
	}
}
