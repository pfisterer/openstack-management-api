package tree_test

import (
	"context"
	"errors"

	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/dev"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/notify"
	"go.uber.org/zap"
)

var allocationResources = []common.Resource{
	{ID: "cores", Name: "Cores"},
	{ID: "gpu", Name: "GPUs"},
	{ID: "ipv4", Name: "IPv4", Kind: common.KindBool},
}

var (
	uniTokens  = common.TokenList{"user:uni@x", "group:uni"}
	studMgr    = common.TokenList{"user:mgr@x", "group:stud"}
	ownerToken = common.TokenList{"user:stud@x"}
)

// allocationFixture: a university budget holding GPUs and IPv4, below it a
// student budget holding neither, and one student project of 4 cores in it (one allocation under the students' budget).
type allocationFixture struct {
	svc       *tree.Service
	store     *nodestore.InMemoryStore
	uni, stud model.Node
	project   model.Node
}

func newAllocationFixture(t *testing.T) allocationFixture {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := nodestore.NewInMemoryStore(log)
	svc := tree.NewService(store, dev.NewMockRoleProvider(), allocationResources,
		common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	create := func(req tree.CreateNodeRequest, email string, tokens common.TokenList) model.Node {
		t.Helper()
		req.Reason = "t"
		if req.Name == "" {
			req.Name = "n"
		}
		n, err := svc.CreateNode(req, model.UIActor(email), email, tokens)
		if err != nil {
			t.Fatalf("create %s: %v", req.Name, err)
		}
		return n
	}
	uni := create(tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Uni",
		Limit: common.ProjectQuota{"cores": 100, "gpu": 4, "ipv4": 1}, AdminScope: common.TokenList{"group:uni"}}, "root@x", rootTokens)
	stud := create(tree.CreateNodeRequest{ParentID: uni.ID, Kind: model.KindBudget, Name: "Students",
		Limit: common.ProjectQuota{"cores": 10}, AdminScope: common.TokenList{"group:stud"},
		EligibleRequesters: common.TokenList{"user:stud@x"},
		AutoApprove:        &model.AutoApprove{PerRequesterLimit: common.ProjectQuota{"cores": 4}}, AllowSubBudgetRequests: new(false)}, "uni@x", uniTokens)
	p := create(tree.CreateNodeRequest{ParentID: stud.ID, Kind: model.KindProject, Name: "Thesis",
		Limit: common.ProjectQuota{"cores": 4}}, "stud@x", ownerToken)
	if p.Status != model.StatusApproved {
		t.Fatalf("setup: the project should be auto-approved, got %q", p.Status)
	}
	return allocationFixture{svc: svc, store: store, uni: uni, stud: stud, project: p}
}

// ── allocation_leaves ───────────────────────────────────────────────────────

// recorder is a notifier that keeps what it is sent.
type recorder struct{ events []notify.Event }

func (r *recorder) Enabled() bool          { return true }
func (r *recorder) Notify(ev notify.Event) { r.events = append(r.events, ev) }
func (r *recorder) ofType(typ string) []notify.Event {
	var out []notify.Event
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

var (
	baseMgr   = common.TokenList{"user:base@x", "group:base"}
	courseMgr = common.TokenList{"user:course@x", "group:course"}
	studT     = common.TokenList{"user:stud@x", "group:students"}
)

// splitFixture: under the root a base budget (Grundversorgung: a pool every student may draw from, auto-approved) and a faculty with a course budget below it (4 cores per student, auto-approved, requests beyond allowed). The course is the more specific budget.
type splitFixture struct {
	svc                   *tree.Service
	store                 *nodestore.InMemoryStore
	base, faculty, course model.Node
	clock                 *time.Time
}

func newSplitFixture(t *testing.T) splitFixture {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := nodestore.NewInMemoryStore(log)
	svc := tree.NewService(store, dev.NewMockRoleProvider(), allocationResources,
		common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	clock := &now
	svc.SetClock(func() time.Time { return *clock })
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	budget := func(parent, name string, limit common.ProjectQuota, admins common.TokenList, aa *model.AutoApprove) model.Node {
		t.Helper()
		n, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: parent, Kind: model.KindBudget, Name: name, Reason: "t", Limit: limit,
			AdminScope: admins, EligibleRequesters: common.TokenList{"group:students"}, AutoApprove: aa, AllowSubBudgetRequests: new(false)},
			model.UIActor("root@x"), "root@x", rootTokens)
		if err != nil {
			t.Fatalf("budget %s: %v", name, err)
		}
		return n
	}
	base := budget(model.RootNodeID, "Grundversorgung", common.ProjectQuota{"cores": 20, "ipv4": 1}, common.TokenList{"group:base"}, &model.AutoApprove{PerRequesterLimit: common.ProjectQuota{"cores": 2}})
	faculty := budget(model.RootNodeID, "Fakultät", common.ProjectQuota{"cores": 50, "gpu": 2}, common.TokenList{"group:faculty"}, nil)
	course := budget(faculty.ID, "Kurs", common.ProjectQuota{"cores": 20, "gpu": 2}, common.TokenList{"group:course"}, &model.AutoApprove{PerRequesterLimit: common.ProjectQuota{"cores": 4}})
	return splitFixture{svc: svc, store: store, base: base, faculty: faculty, course: course, clock: clock}
}

func (f splitFixture) node(t *testing.T, id string) model.Node {
	t.Helper()
	n, err := f.store.GetNode(context.Background(), id)
	if err != nil || n == nil {
		t.Fatalf("node %s: %v", id, err)
	}
	return *n
}

func (f splitFixture) allocations(t *testing.T, projectID string) map[string]model.Node {
	t.Helper()
	list, err := f.store.ListNodes(context.Background(), nodestore.NodeQuery{Kinds: []string{model.KindAllocation}, ProjectIDs: []string{projectID}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.Node{}
	for _, a := range list {
		out[*a.ParentID] = a
	}
	return out
}

// (1) The split: what needs no decision first, the most specific budget first, the rest as a request; shown before, and the project is made of the parts.
func TestSplit_AutoFirstMostSpecificFirstRestAsRequest(t *testing.T) {
	f := newSplitFixture(t)
	plan, err := f.svc.PlanSplit(tree.SplitRequest{Limit: common.ProjectQuota{"cores": 9}}, "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Parts) != 2 || len(plan.Unplaced) != 0 {
		t.Fatalf("plan = %+v, want two parts and nothing unplaced", plan)
	}
	course, base := plan.Parts[0], plan.Parts[1]
	if course.BudgetID != f.course.ID || course.AutoApproved["cores"] != 4 || course.Limit["cores"] != 7 {
		t.Errorf("course part = %+v, want 4 cores granted at once and the 3 left over asked from it (the most specific budget that takes requests)", course)
	}
	if base.BudgetID != f.base.ID || base.AutoApproved["cores"] != 2 || base.Limit["cores"] != 2 {
		t.Errorf("base part = %+v, want the 2 cores of the person's share there", base)
	}
	if len(course.BudgetPath) != 2 || course.BudgetPath[0].Name != "Fakultät" {
		t.Errorf("the part names its budget's path: %+v", course.BudgetPath)
	}

	// Sent as shown: approved parts at once, the rest waits as a change on top.
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Thesis", Reason: "t", Allocations: []tree.AllocationSpec{
		{BudgetID: course.BudgetID, Limit: course.Limit}, {BudgetID: base.BudgetID, Limit: base.Limit},
	}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.StatusApproved || p.Limit["cores"] != 6 {
		t.Fatalf("project = %q %v, want started with the 6 cores granted", p.Status, p.Limit)
	}
	a := f.allocations(t, p.ID)
	if c := a[f.course.ID]; c.Status != model.StatusChangePending || c.Limit["cores"] != 4 || (*c.Pending.Limit)["cores"] != 7 {
		t.Errorf("course allocation = %q %v %+v, want 4 approved and 7 asked for", c.Status, c.Limit, c.Pending)
	}
	if b := a[f.base.ID]; b.Status != model.StatusApproved || b.Limit["cores"] != 2 {
		t.Errorf("base allocation = %q %v", b.Status, b.Limit)
	}
	if len(p.Allocations) != 2 || p.HomeBudgetID == nil {
		t.Errorf("the project lists its allocations and has a home: %+v / %v", p.Allocations, p.HomeBudgetID)
	}

	// The course's manager decides on the rest; the project grows by it.
	c := a[f.course.ID]
	if _, err := f.svc.ApproveNode(c.ID, tree.ApproveNodeRequest{ExpectedRevision: new(c.CurrentRevision())}, model.UIActor("course@x"), courseMgr); err != nil {
		t.Fatal(err)
	}
	if got := f.node(t, p.ID); got.Limit["cores"] != 9 {
		t.Errorf("after the approval the project holds %v, want 9 cores", got.Limit)
	}
}

// Without allocations named, the request is split by itself; a part no budget would even take is refused.
func TestSplit_AutomaticOnCreateAndUnplaced(t *testing.T) {
	f := newSplitFixture(t)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Auto", Reason: "t", Limit: common.ProjectQuota{"cores": 6}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit["cores"] != 6 || len(p.Allocations) != 2 {
		t.Errorf("6 cores without a request: 4 from the course, 2 from the base; got %v in %d allocations", p.Limit, len(p.Allocations))
	}
	plan, err := f.svc.PlanSplit(tree.SplitRequest{Limit: common.ProjectQuota{"gpu": 1, "ipv4": 1}}, "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Unplaced["ipv4"] != 0 || plan.Unplaced["gpu"] != 0 {
		t.Errorf("the course takes a request for the GPU, the base grants the network: %+v", plan)
	}
}

// (3) The person chooses: start with what is granted, or wait for everything.
func TestStartWhen(t *testing.T) {
	f := newSplitFixture(t)
	create := func(start string) model.Node {
		t.Helper()
		p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "P " + start, Reason: "t", StartWhen: start, Allocations: []tree.AllocationSpec{
			{BudgetID: f.base.ID, Limit: cores(1)}, {BudgetID: f.course.ID, Limit: common.ProjectQuota{"gpu": 1}},
		}}, model.UIActor("stud@x"), "stud@x", studT)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	now := create(model.StartNow)
	if now.Status != model.StatusApproved || now.Limit["cores"] != 1 {
		t.Errorf("start now: %q %v, want started with the core", now.Status, now.Limit)
	}
	all := create(model.StartAll)
	if all.Status != model.StatusPending {
		t.Fatalf("start when all is decided: %q, want waiting", all.Status)
	}
	gpu := f.allocations(t, all.ID)[f.course.ID]
	if _, err := f.svc.RejectNode(gpu.ID, tree.RejectNodeRequest{Reason: new("no GPU this term")}, model.UIActor("course@x"), courseMgr); err != nil {
		t.Fatal(err)
	}
	if p := f.node(t, all.ID); p.Status != model.StatusApproved || p.Limit["cores"] != 1 {
		t.Errorf("once all is decided it starts with what was granted: %q %v", p.Status, p.Limit)
	}
}

// (4) and (5): the owner manages the whole project; a budget's managers see it and act on their own allocation only; reducing or removing is for the paying budget's managers (and above) and the owner, never a co-funder.
func TestRights_PerAllocation(t *testing.T) {
	f := newSplitFixture(t)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Shared", Reason: "t", Allocations: []tree.AllocationSpec{
		{BudgetID: f.base.ID, Limit: cores(2)}, {BudgetID: f.course.ID, Limit: cores(4)},
	}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	a := f.allocations(t, p.ID)
	base, course := a[f.base.ID], a[f.course.ID]

	// Both budgets' managers see the project; a stranger does not.
	for _, tok := range []common.TokenList{baseMgr, courseMgr, studT} {
		if _, err := f.svc.GetNode(p.ID, tok); err != nil {
			t.Errorf("%v cannot read the project: %v", tok, err)
		}
	}
	if _, err := f.svc.GetNode(p.ID, common.TokenList{"user:other@x"}); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a stranger reads the project: %v", err)
	}
	// Each budget lists it, by its own allocation.
	page, err := f.svc.ListChildren(f.base.ID, tree.ChildFilter{Kind: model.KindProject}, baseMgr, 50, 0)
	if err != nil || page.Total != 1 || page.Items[0].ID != base.ID || page.Items[0].Project == nil || len(page.Items[0].Project.Allocations) != 2 {
		t.Fatalf("base table: %v %+v", err, page)
	}

	// The course's manager may not touch the base's allocation — neither change nor end it.
	if _, err := f.svc.RequestChange(base.ID, tree.ChangeNodeRequest{Limit: quota(cores(1))}, model.UIActor("course@x"), courseMgr); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a co-funder changed another budget's allocation: %v", err)
	}
	if _, err := f.svc.ReleaseNode(base.ID, tree.ReleaseNodeRequest{}, model.UIActor("course@x"), courseMgr); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a co-funder ended another budget's allocation: %v", err)
	}
	// Nor act on the project as a whole: transfer it, release it, rename it.
	if _, err := f.svc.TransferOwner(p.ID, tree.TransferOwnerRequest{NewOwner: "other@x"}, model.UIActor("course@x"), courseMgr); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a co-funder transferred the project: %v", err)
	}
	if _, err := f.svc.ReleaseNode(p.ID, tree.ReleaseNodeRequest{}, model.UIActor("course@x"), courseMgr); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a co-funder released the project: %v", err)
	}
	// Its own: a manager changes it at once, and withdraws it.
	if _, err := f.svc.RequestChange(course.ID, tree.ChangeNodeRequest{Limit: quota(cores(3))}, model.UIActor("course@x"), courseMgr); err != nil {
		t.Errorf("the course's manager lowers its own allocation: %v", err)
	}
	if _, err := f.svc.ReleaseNode(course.ID, tree.ReleaseNodeRequest{}, model.UIActor("course@x"), courseMgr); err != nil {
		t.Errorf("the course's manager withdraws its own allocation: %v", err)
	}
	if got := f.node(t, p.ID); got.Limit["cores"] != 2 {
		t.Errorf("after the withdrawal the project holds %v, want the base's 2 cores", got.Limit)
	}
	// The owner gives back what is left; the root manages every budget and so the whole project.
	if _, err := f.svc.ReleaseNode(base.ID, tree.ReleaseNodeRequest{}, model.UIActor("stud@x"), studT); err != nil {
		t.Errorf("the owner gives an allocation back: %v", err)
	}
	if _, err := f.svc.TransferOwner(p.ID, tree.TransferOwnerRequest{NewOwner: "other@x"}, rootActor(), rootTokens); err != nil {
		t.Errorf("a root admin transfers the project: %v", err)
	}
	if a := f.allocations(t, p.ID)[f.base.ID]; a.Owner != "user:other@x" {
		t.Errorf("the allocations follow the owner (a person's share is counted by them): %q", a.Owner)
	}
}

// A manager of a budget grants an allocation of their own accord, with a reason; the project's holder asks for one under the budget's rules; one live allocation per budget.
func TestAddAllocation(t *testing.T) {
	f := newSplitFixture(t)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Lab", Reason: "t", ParentID: f.base.ID, Limit: cores(2)}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	add := func(budget string, limit common.ProjectQuota, reason string, tokens common.TokenList) (model.Node, error) {
		return f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindAllocation, ProjectID: p.ID, ParentID: budget, Limit: limit, Reason: reason}, model.UIActor("x@x"), "x@x", tokens)
	}
	if _, err := add(f.faculty.ID, common.ProjectQuota{"gpu": 1}, "", common.TokenList{"group:faculty"}); err == nil {
		t.Error("a manager's grant without a reason was accepted")
	}
	g, err := add(f.faculty.ID, common.ProjectQuota{"gpu": 1}, "GPU for the thesis", common.TokenList{"group:faculty"})
	if err != nil || g.Status != model.StatusApproved {
		t.Fatalf("the faculty grants a GPU: %v %q", err, g.Status)
	}
	if got := f.node(t, p.ID); got.Limit["gpu"] != 1 || got.Limit["cores"] != 2 {
		t.Errorf("project = %v, want the sum", got.Limit)
	}
	if _, err := add(f.faculty.ID, common.ProjectQuota{"gpu": 1}, "again", common.TokenList{"group:faculty"}); !errors.Is(err, common.ErrConflict) {
		t.Errorf("a second allocation from the same budget: %v", err)
	}
	r, err := add(f.course.ID, cores(4), "more", studT)
	if err != nil || r.Status != model.StatusApproved {
		t.Errorf("the owner asks the course for 4 cores within its policy: %v %q", err, r.Status)
	}
	if _, err := add(f.course.ID, cores(1), "x", common.TokenList{"user:other@x"}); err == nil {
		t.Error("a stranger added an allocation")
	}
}

// Each allocation is charged to its own budget and those above; a person's share counts per budget; what the project uses beyond its limit goes to its home allocation.
func TestAccounting_PerAllocation(t *testing.T) {
	f := newSplitFixture(t)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Both", Reason: "t", Allocations: []tree.AllocationSpec{
		{BudgetID: f.base.ID, Limit: cores(2)}, {BudgetID: f.course.ID, Limit: cores(4)},
	}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	usage := func(id string, tokens common.TokenList) int {
		t.Helper()
		n, err := f.svc.GetNode(id, tokens)
		if err != nil {
			t.Fatal(err)
		}
		return n.Usage.Total([]string{"cores"})["cores"]
	}
	if b, c, fac := usage(f.base.ID, rootTokens), usage(f.course.ID, rootTokens), usage(f.faculty.ID, rootTokens); b != 2 || c != 4 || fac != 4 {
		t.Errorf("charged base %d, course %d, faculty %d; want 2, 4, 4", b, c, fac)
	}
	// The person's share of the base is used up there, not by what the course gives.
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Second", Reason: "t", ParentID: f.base.ID, Limit: cores(1)}, model.UIActor("stud@x"), "stud@x", studT); err != nil {
		t.Fatal(err)
	}
	second := f.allocations(t, mustFind(t, f.store, "Second").ID)[f.base.ID]
	if second.Status != model.StatusPending {
		t.Errorf("a third core from the base is beyond the person's 2 there: %q", second.Status)
	}
	// Measured beyond the limit: charged where the project lives.
	if _, err := f.store.UpdateNode(context.Background(), p.ID, func(n *model.Node) error {
		n.EditBinding("openstack", func(b *model.Binding) { b.InUse = common.ProjectQuota{"cores": 9} })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	home := f.node(t, p.ID).HomeBudgetID
	other := f.course.ID
	if *home == f.course.ID {
		other = f.base.ID
	}
	homeAlloc := f.allocations(t, p.ID)[*home]
	if got := usage(*home, rootTokens); got != homeAlloc.Limit["cores"]+3 {
		t.Errorf("home budget charged %d, want its allocation plus the 3 cores beyond the project's 6", got)
	}
	if got := usage(other, rootTokens); got != f.allocations(t, p.ID)[other].Limit["cores"] {
		t.Errorf("the other budget is charged %d, want only its allocation", got)
	}
}

func mustFind(t *testing.T, store *nodestore.InMemoryStore, name string) model.Node {
	t.Helper()
	list, err := store.ListNodes(context.Background(), nodestore.NodeQuery{Kinds: []string{model.KindProject}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no project %q", name)
	return model.Node{}
}

// (2) and (6): an allocation whose end passed is only marked; without notifications nothing is cut; with them it is cut after the period, the project shrinks, and the people are told. Moving the end later lifts the mark.
func TestExpiry_MarkThenCutOnlyWithNotifications(t *testing.T) {
	f := newSplitFixture(t)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Ending", Reason: "t", StartWhen: model.StartNow, Allocations: []tree.AllocationSpec{
		{BudgetID: f.base.ID, Limit: cores(2), TerminationDate: ptr("2026-12-31")}, {BudgetID: f.course.ID, Limit: cores(4), TerminationDate: ptr("2026-12-10")},
	}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	course := f.allocations(t, p.ID)[f.course.ID]
	if got := f.node(t, p.ID); endOf(got) != "2026-12-31" {
		t.Errorf("the project ends with its latest allocation, got %s", endOf(got))
	}
	ctx := context.Background()

	// Without a notification service: marked, never cut.
	*f.clock = time.Date(2026, 12, 11, 10, 0, 0, 0, time.UTC)
	if res, err := f.svc.SweepExpiry(ctx); err != nil || res.Marked != 1 {
		t.Fatalf("sweep: %v %+v", err, res)
	}
	marked := f.node(t, course.ID)
	if marked.Expired == nil || marked.Expired.Cause != model.ExpiryEnded || marked.Expired.Due != nil || marked.Status != model.StatusApproved {
		t.Fatalf("marked = %+v %q, want expired without a day to cut, still paying", marked.Expired, marked.Status)
	}
	*f.clock = time.Date(2027, 2, 1, 10, 0, 0, 0, time.UTC)
	if _, err := f.svc.SweepExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.node(t, p.ID); got.Limit["cores"] != 6 {
		t.Errorf("without notifications nothing is cut: %v", got.Limit)
	}
	// The project shows which allocation ran out.
	view, _ := f.svc.GetNode(p.ID, studT)
	expired := 0
	for _, a := range view.Allocations {
		if a.Expired != nil {
			expired++
		}
	}
	if expired != 2 {
		t.Errorf("both allocations have run out by February, %d are marked", expired)
	}

	// With one: the mark gets its day and is sent; after it the allocation is cut.
	rec := &recorder{}
	f.svc.SetNotifier(rec)
	*f.clock = time.Date(2027, 2, 2, 10, 0, 0, 0, time.UTC)
	if res, err := f.svc.SweepExpiry(ctx); err != nil || res.Dated != 2 {
		t.Fatalf("sweep with notifications: %v %+v", err, res)
	}
	sent := rec.ofType(notify.TypeAllocationExpired)
	if len(sent) != 2 || sent[0].Class != notify.ClassAction || sent[0].Due == "" || sent[0].Actor != "" || sent[0].Object.ID != p.ID {
		t.Fatalf("allocation.expired = %+v", sent)
	}
	recips := map[string]bool{}
	for _, r := range sent[0].Recipients {
		recips[r.User+r.Group] = true
	}
	if !recips["stud@x"] {
		t.Errorf("the owner is told: %+v", sent[0].Recipients)
	}
	*f.clock = time.Date(2027, 2, 17, 10, 0, 0, 0, time.UTC)
	if res, err := f.svc.SweepExpiry(ctx); err != nil || res.Cut != 2 {
		t.Fatalf("sweep after the period: %v %+v", err, res)
	}
	if got := f.node(t, p.ID); got.Limit["cores"] != 0 {
		t.Errorf("after the period the project shrinks by both: %v", got.Limit)
	}
	if cut := f.node(t, course.ID); cut.Status != model.StatusEnded {
		t.Errorf("the cut allocation ended: %q", cut.Status)
	}
	if len(rec.ofType(notify.TypeAllocationShortened)) != 2 {
		t.Errorf("allocation.shortened = %+v", rec.ofType(notify.TypeAllocationShortened))
	}
}

func TestExpiry_ExtensionLiftsTheMark(t *testing.T) {
	f := newSplitFixture(t)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Ext", Reason: "t", ParentID: f.base.ID, Limit: cores(1), TerminationDate: ptr("2026-12-10")}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	*f.clock = time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC)
	if _, err := f.svc.SweepExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := f.allocations(t, p.ID)[f.base.ID]
	if a.Expired == nil {
		t.Fatal("not marked")
	}
	if _, err := f.svc.RequestChange(a.ID, tree.ChangeNodeRequest{TerminationDate: ptr("2027-03-31")}, model.UIActor("base@x"), baseMgr); err != nil {
		t.Fatal(err)
	}
	if got := f.node(t, a.ID); got.Expired != nil || endOf(got) != "2027-03-31" {
		t.Errorf("extended: %+v %s", got.Expired, endOf(got))
	}
}

// (6) A budget is deleted while projects of others draw from it: overbooking is accepted, the allocation moves to the budget above, marked; a request still waiting is withdrawn.
func TestDeleteBudget_WithForeignAllocations(t *testing.T) {
	f := newSplitFixture(t)
	rec := &recorder{}
	f.svc.SetNotifier(rec)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Funded", Reason: "t", Allocations: []tree.AllocationSpec{
		{BudgetID: f.base.ID, Limit: cores(2)}, {BudgetID: f.course.ID, Limit: cores(6)},
	}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "Waiting", Reason: "t", ParentID: f.course.ID, Limit: common.ProjectQuota{"gpu": 1}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteNode(f.course.ID, model.UIActor("course@x"), courseMgr); err != nil {
		t.Fatalf("delete a budget others draw from: %v", err)
	}
	moved := f.allocations(t, p.ID)[f.faculty.ID]
	if moved.ID == "" || moved.Expired == nil || moved.Expired.Cause != model.ExpiryBudgetDeleted || moved.Expired.BudgetName != "Kurs" || moved.Status != model.StatusApproved {
		t.Fatalf("the allocation should hang under the faculty, marked: %+v", moved)
	}
	if moved.Limit["cores"] != 4 || moved.Pending != nil {
		t.Errorf("it keeps what was approved, the waiting rest is dropped: %v %+v", moved.Limit, moved.Pending)
	}
	if got := f.node(t, p.ID); got.Limit["cores"] != 6 {
		t.Errorf("the project keeps its resources: %v", got.Limit)
	}
	if fac, _ := f.svc.GetNode(f.faculty.ID, rootTokens); fac.Usage.Total([]string{"cores"})["cores"] != 4 {
		t.Errorf("the faculty carries it now: %v", fac.Usage)
	}
	if w := f.node(t, waiting.ID); w.Status != model.StatusRejected {
		t.Errorf("a project waiting only for the deleted budget is not started: %q", w.Status)
	}
	if len(rec.ofType(notify.TypeAllocationExpired)) != 1 || rec.ofType(notify.TypeAllocationExpired)[0].Due == "" {
		t.Errorf("allocation.expired with its day: %+v", rec.ofType(notify.TypeAllocationExpired))
	}
	// Moved to a budget that exists, it is no longer marked.
	if _, err := f.svc.ReparentNode(moved.ID, tree.ReparentNodeRequest{NewParentID: f.base.ID}, rootActor(), rootTokens); !errors.Is(err, common.ErrConflict) {
		t.Errorf("one allocation per budget: %v", err)
	}
	other := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Neu", Limit: cores(10)})
	if _, err := f.svc.ReparentNode(moved.ID, tree.ReparentNodeRequest{NewParentID: other.ID}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	if got := f.node(t, moved.ID); got.Expired != nil {
		t.Errorf("moved, the mark is gone: %+v", got.Expired)
	}
}

// approval.waiting goes to the budget's managers and is resolved by the decision; approval.decided to whoever asked.
func TestNotify_ApprovalWaitingAndDecided(t *testing.T) {
	f := newSplitFixture(t)
	rec := &recorder{}
	f.svc.SetNotifier(rec)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{Kind: model.KindProject, Name: "GPU", Reason: "t", ParentID: f.course.ID, Limit: common.ProjectQuota{"gpu": 1}}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil {
		t.Fatal(err)
	}
	w := rec.ofType(notify.TypeApprovalWaiting)
	if len(w) != 1 || w[0].Resolve || len(w[0].Recipients) != 1 || w[0].Recipients[0].Group != "group:course" || w[0].Actor != "stud@x" {
		t.Fatalf("approval.waiting = %+v", w)
	}
	a := f.allocations(t, p.ID)[f.course.ID]
	if _, err := f.svc.ApproveNode(a.ID, tree.ApproveNodeRequest{ExpectedRevision: new(a.CurrentRevision())}, model.UIActor("course@x"), courseMgr); err != nil {
		t.Fatal(err)
	}
	w = rec.ofType(notify.TypeApprovalWaiting)
	if len(w) != 2 || !w[1].Resolve || w[1].Key != w[0].Key {
		t.Errorf("the decision resolves it under the same key: %+v", w)
	}
	d := rec.ofType(notify.TypeApprovalDecided)
	if len(d) != 1 || d[0].Class != notify.ClassInfo || d[0].Recipients[0].User != "stud@x" || d[0].Data["decision"] != "approved" {
		t.Errorf("approval.decided = %+v", d)
	}
}
