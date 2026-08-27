package tree

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// An availability is not a quantity, and every one of these tests pins a way the
// two could be confused. Held in the same map[string]int as everything else, so
// the arithmetic has to be told the difference — nothing about the value says it.

var availabilityCatalogue = []common.ManagedProject{
	{ID: "cores", Name: "Cores"},
	{ID: "dhbw-ipv4", Name: "DHBW IPv4", Kind: common.KindBool,
		Grant: &common.Grant{Type: common.GrantNetwork, Target: "net-uuid"}},
}

func availabilityService(t *testing.T) *Service {
	t.Helper()
	log := zap.NewNop().Sugar()
	return NewService(NewInMemoryStore(log), noRoles{}, availabilityCatalogue,
		common.TokenList{"group:root-admin"}, 5*time.Second,
		common.DefaultMaxAuthorizedUsers,
		Accounting{ChargeOSInUse: true, ChargeReleased: true}, log)
}

// The one that would be invisible in production: three projects with a network
// do not add up to three networks, so an availability must never reach a sum.
func TestAvailability_DoesNotSumAcrossSiblings(t *testing.T) {
	svc := availabilityService(t)

	parent := "budget"
	leaves := []Node{
		{ID: "a", ParentID: &parent, Status: StatusApproved, Limit: common.ProjectQuota{"cores": 2, "dhbw-ipv4": 1}},
		{ID: "b", ParentID: &parent, Status: StatusApproved, Limit: common.ProjectQuota{"cores": 3, "dhbw-ipv4": 1}},
		{ID: "c", ParentID: &parent, Status: StatusApproved, Limit: common.ProjectQuota{"cores": 1, "dhbw-ipv4": 1}},
	}
	// The budget is the tracked root of this rollup: present as a key, with no
	// parent of its own, so the climb stops there.
	parents := map[string]*string{parent: nil}

	usage := buildRolledUpUsage(leaves, parents, svc.countIDs, false)
	total := usage[parent].Total(svc.countIDs)

	if total["cores"] != 6 {
		t.Errorf("cores must still sum: got %d, want 6", total["cores"])
	}
	if got, ok := total["dhbw-ipv4"]; ok && got != 0 {
		t.Errorf("the availability was summed to %d; it must not appear in the rollup at all", got)
	}
}

func TestAvailability_RejectsAnythingButZeroOrOne(t *testing.T) {
	svc := availabilityService(t)

	for _, val := range []int{-1, 2, 7} {
		q := common.ProjectQuota{"dhbw-ipv4": val}

		if err := svc.validateBudgetLimit(q); err == nil {
			t.Errorf("budget limit accepted %d for an availability", val)
		}
		if err := svc.validateLeafLimit(q); err == nil {
			t.Errorf("leaf limit accepted %d for an availability", val)
		}
	}

	for _, val := range []int{0, 1} {
		if err := svc.validateBudgetLimit(common.ProjectQuota{"dhbw-ipv4": val}); err != nil {
			t.Errorf("budget limit rejected %d for an availability: %v", val, err)
		}
	}
}

// -1 deserves its own case: it is the "no cap" sentinel, and the parent-child
// check reads it as "anything goes". On an availability that would be a wildcard
// handing the resource to every descendant.
func TestAvailability_UnlimitedIsRefusedByName(t *testing.T) {
	svc := availabilityService(t)

	err := svc.validateBudgetLimit(common.ProjectQuota{"dhbw-ipv4": common.UnlimitedQuota})
	if err == nil {
		t.Fatal("unlimited accepted on an availability")
	}
	if !strings.Contains(err.Error(), "dhbw-ipv4") {
		t.Errorf("the error must name the resource, got %q", err)
	}
	// The same value stays legal on a quantity.
	if err := svc.validateBudgetLimit(common.ProjectQuota{"cores": common.UnlimitedQuota}); err != nil {
		t.Errorf("unlimited must remain valid for a quantity: %v", err)
	}
}

// Delegation: a child cannot hold what its parent does not.
func TestAvailability_CannotBeGrantedBeyondTheParent(t *testing.T) {
	svc := availabilityService(t)
	withhold := &Node{ID: "p", Limit: common.ProjectQuota{"cores": 10, "dhbw-ipv4": 0}}
	grant := &Node{ID: "p", Limit: common.ProjectQuota{"cores": 10, "dhbw-ipv4": 1}}

	if err := svc.validateChildBudgetLimit(withhold, common.ProjectQuota{"dhbw-ipv4": 1}); err == nil {
		t.Error("a child was granted an availability its parent does not hold")
	}
	if err := svc.validateChildBudgetLimit(grant, common.ProjectQuota{"dhbw-ipv4": 1}); err != nil {
		t.Errorf("a parent that holds it must be able to pass it on: %v", err)
	}
}

// An availability does not consume capacity, so a budget that grants one to
// every child is not over-allocated.
func TestAvailability_DoesNotConsumeCapacity(t *testing.T) {
	svc := availabilityService(t)
	ctx := context.Background()
	budget := Node{ID: "b", Kind: KindBudget, Status: StatusApproved,
		Limit: common.ProjectQuota{"cores": 4, "dhbw-ipv4": 1}}
	if err := svc.store.UpsertNode(ctx, budget); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := svc.checkCapacity(ctx, []Node{budget}, common.ProjectQuota{"cores": 4, "dhbw-ipv4": 1}, nil)
	if err != nil {
		t.Fatalf("granting an availability was charged against the budget: %v", err)
	}
}

func TestAvailableResources_RootSeesTheWholeCatalogue(t *testing.T) {
	svc := availabilityService(t)

	got := svc.availableResourcesFor(Node{ID: RootNodeID, Limit: common.ProjectQuota{}})

	if len(got) != 2 {
		t.Fatalf("root must see every resource, got %v", got)
	}
}

func TestAvailableResources_BelowTheRootOnlyWhatWasDelegated(t *testing.T) {
	svc := availabilityService(t)

	cases := map[string]struct {
		limit common.ProjectQuota
		want  []string
	}{
		"a withheld availability disappears": {common.ProjectQuota{"cores": 4, "dhbw-ipv4": 0}, []string{"cores"}},
		"a granted one appears":              {common.ProjectQuota{"cores": 4, "dhbw-ipv4": 1}, []string{"cores", "dhbw-ipv4"}},
		"a zero quantity disappears too":     {common.ProjectQuota{"cores": 0, "dhbw-ipv4": 1}, []string{"dhbw-ipv4"}},
		"an absent key is not delegated":     {common.ProjectQuota{"cores": 4}, []string{"cores"}},
		"an uncapped quantity stays visible": {common.ProjectQuota{"cores": common.UnlimitedQuota}, []string{"cores"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := svc.availableResourcesFor(Node{ID: "child", Limit: tc.limit})

			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Withdrawal: the direction the parent-child rule cannot see.
//
// That rule runs when a CHILD is written, so lowering a parent from 1 to 0
// leaves every existing descendant holding a resource its parent no longer has.
// Quantities are protected by "new limit is below current active usage"; this is
// the same protection for the other kind.
func TestAvailability_CannotBeWithdrawnWhileSomethingBelowHoldsIt(t *testing.T) {
	svc := availabilityService(t)
	ctx := context.Background()

	budget := Node{ID: "b", Kind: KindBudget, Status: StatusApproved, Name: "Faculty",
		Limit: common.ProjectQuota{"cores": 10, "dhbw-ipv4": 1}}
	parent := budget.ID
	leaf := Node{ID: "p1", Kind: KindProject, Status: StatusApproved, Name: "Someone's project",
		ParentID: &parent, Limit: common.ProjectQuota{"cores": 2, "dhbw-ipv4": 1}}
	for _, n := range []Node{budget, leaf} {
		if err := svc.store.UpsertNode(ctx, n); err != nil {
			t.Fatalf("seed %s: %v", n.ID, err)
		}
	}

	err := svc.checkAvailabilityWithdrawal(ctx, budget, common.ProjectQuota{"cores": 10, "dhbw-ipv4": 0})
	if err == nil {
		t.Fatal("the availability was withdrawn while a project below still held it")
	}
	// The message has to name both the resource and one holder, or the admin
	// cannot act on it — a refusal without a target is just a wall.
	if !strings.Contains(err.Error(), "dhbw-ipv4") || !strings.Contains(err.Error(), "Someone's project") {
		t.Errorf("error names neither the resource nor a holder: %q", err)
	}
}

func TestAvailability_WithdrawalIsFineWhenNothingHoldsIt(t *testing.T) {
	svc := availabilityService(t)
	ctx := context.Background()

	budget := Node{ID: "b", Kind: KindBudget, Status: StatusApproved,
		Limit: common.ProjectQuota{"cores": 10, "dhbw-ipv4": 1}}
	parent := budget.ID
	leaf := Node{ID: "p1", Kind: KindProject, Status: StatusApproved, ParentID: &parent,
		Limit: common.ProjectQuota{"cores": 2, "dhbw-ipv4": 0}}
	for _, n := range []Node{budget, leaf} {
		if err := svc.store.UpsertNode(ctx, n); err != nil {
			t.Fatalf("seed %s: %v", n.ID, err)
		}
	}

	if err := svc.checkAvailabilityWithdrawal(ctx, budget, common.ProjectQuota{"cores": 10, "dhbw-ipv4": 0}); err != nil {
		t.Fatalf("withdrawal refused although nothing below holds it: %v", err)
	}
}

// The node being changed is not its own descendant. Without the exclusion every
// withdrawal would refuse itself, and the guard would be unusable rather than
// merely wrong.
func TestAvailability_WithdrawalIgnoresTheNodeItself(t *testing.T) {
	svc := availabilityService(t)
	ctx := context.Background()

	budget := Node{ID: "b", Kind: KindBudget, Status: StatusApproved,
		Limit: common.ProjectQuota{"dhbw-ipv4": 1}}
	if err := svc.store.UpsertNode(ctx, budget); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.checkAvailabilityWithdrawal(ctx, budget, common.ProjectQuota{"dhbw-ipv4": 0}); err != nil {
		t.Fatalf("a budget could not give up its own availability: %v", err)
	}
}

// Lowering a quantity must not be mistaken for withdrawing an availability.
func TestAvailability_WithdrawalLeavesQuantitiesAlone(t *testing.T) {
	svc := availabilityService(t)
	ctx := context.Background()

	budget := Node{ID: "b", Kind: KindBudget, Status: StatusApproved,
		Limit: common.ProjectQuota{"cores": 10, "dhbw-ipv4": 1}}
	parent := budget.ID
	leaf := Node{ID: "p1", Kind: KindProject, Status: StatusApproved, ParentID: &parent,
		Limit: common.ProjectQuota{"cores": 2, "dhbw-ipv4": 1}}
	for _, n := range []Node{budget, leaf} {
		if err := svc.store.UpsertNode(ctx, n); err != nil {
			t.Fatalf("seed %s: %v", n.ID, err)
		}
	}

	// cores drops, the availability stays: this guard has nothing to say.
	if err := svc.checkAvailabilityWithdrawal(ctx, budget, common.ProjectQuota{"cores": 4, "dhbw-ipv4": 1}); err != nil {
		t.Fatalf("lowering a quantity tripped the availability guard: %v", err)
	}
}

// A resource added to a RUNNING deployment has to reach the root, or it can
// never be granted to anyone: every edge is bounded by its parent, and the root
// is where delegation starts. The root's limit is written when the root is
// created, which on any existing tree was long ago.
func TestBootstrap_RootAdoptsResourcesNewToTheCatalogue(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore(zap.NewNop().Sugar())

	// A tree that predates the availability: the root exists with the old set.
	before := NewService(store, noRoles{}, []common.ManagedProject{{ID: "cores", Name: "Cores"}},
		common.TokenList{"group:root-admin"}, 5*time.Second,
		common.DefaultMaxAuthorizedUsers,
		Accounting{}, zap.NewNop().Sugar())
	if err := before.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatalf("initial bootstrap: %v", err)
	}

	// The catalogue grows, the service restarts.
	after := NewService(store, noRoles{}, availabilityCatalogue,
		common.TokenList{"group:root-admin"}, 5*time.Second,
		common.DefaultMaxAuthorizedUsers,
		Accounting{}, zap.NewNop().Sugar())
	if err := after.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatalf("bootstrap after the catalogue grew: %v", err)
	}

	root, err := store.GetNode(ctx, RootNodeID)
	if err != nil || root == nil {
		t.Fatalf("load root: %v", err)
	}
	if root.Limit["dhbw-ipv4"] != 1 {
		t.Fatalf("the root did not adopt the new availability, so nobody can be granted it: %v", root.Limit)
	}
	// And a child can now actually be given it.
	if err := after.validateChildBudgetLimit(root, common.ProjectQuota{"dhbw-ipv4": 1}); err != nil {
		t.Errorf("the root holds it but cannot pass it on: %v", err)
	}
}

// Adopting must not overrule a deliberate cap: an operator who limited the root
// meant it.
func TestBootstrap_RootAdoptionLeavesExistingLimitsAlone(t *testing.T) {
	svc := availabilityService(t)
	root := Node{ID: RootNodeID, Limit: common.ProjectQuota{"cores": 100, "dhbw-ipv4": 0}}

	added := svc.adoptNewCatalogueResources(&root)

	if len(added) != 0 {
		t.Errorf("added %v although every resource was already present", added)
	}
	if root.Limit["cores"] != 100 || root.Limit["dhbw-ipv4"] != 0 {
		t.Errorf("existing values were overwritten: %v", root.Limit)
	}
}
