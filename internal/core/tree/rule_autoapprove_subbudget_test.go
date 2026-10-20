package tree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// subBudgetStep is one request by "stud@x" under the fixture budget: a sub-budget (managed by stud) or a project.
type subBudgetStep struct {
	kind    string
	limit   common.ProjectQuota
	inherit bool
	want    string // expected status; "" expects errForbidden, "refused" any error
}

func subBudget(n int, want string) subBudgetStep {
	return subBudgetStep{kind: model.KindBudget, limit: cores(n), want: want}
}

func project(n int, want string) subBudgetStep {
	return subBudgetStep{kind: model.KindProject, limit: cores(n), want: want}
}

// A pool grants a sub-budget request whenever the edge rule allows it (a sub-budget's limit is a cap, not a reservation); anything else waits for a manager or, under a hard limit, is refused.
func TestAutoApprove_SubBudgets(t *testing.T) {
	cases := []struct {
		name   string
		policy *model.AutoApprove
		hard   bool
		steps  []subBudgetStep
	}{
		{
			name:   "pool grants sub-budgets, overbooking included",
			policy: &model.AutoApprove{},
			steps:  []subBudgetStep{subBudget(8, model.StatusApproved), subBudget(8, model.StatusApproved), project(8, model.StatusApproved)},
		},
		{
			name:   "pool leaves a sub-budget larger than the budget to a manager",
			policy: &model.AutoApprove{},
			steps:  []subBudgetStep{subBudget(11, model.StatusPending)},
		},
		{
			name:   "a hard pool refuses a sub-budget larger than the budget",
			policy: &model.AutoApprove{},
			hard:   true,
			steps:  []subBudgetStep{subBudget(11, "refused")},
		},
		{
			name:   "pool leaves a request for nothing to a manager",
			policy: &model.AutoApprove{},
			steps:  []subBudgetStep{subBudget(0, model.StatusPending)},
		},
		{
			name:   "an inheriting sub-budget is for managers only",
			policy: &model.AutoApprove{},
			steps:  []subBudgetStep{{kind: model.KindBudget, inherit: true, want: ""}},
		},
		{
			name:   "without a policy a sub-budget request waits",
			policy: nil,
			steps:  []subBudgetStep{subBudget(1, model.StatusPending)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSvc(t, common.TokenList{"group:root"})
			req := tree.CreateNodeRequest{
				ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Faculty", Reason: "t",
				Limit: cores(10), AdminScope: common.TokenList{"group:root"},
				EligibleRequesters: common.TokenList{"user:stud@x"},
				AutoApprove:        tc.policy,
			}
			if tc.hard {
				req.AllowRequestsBeyondAutoApprove = new(false)
			}
			budget, err := svc.CreateNode(req, model.UIActor("root@x"), "root@x", rootTokens)
			if err != nil {
				t.Fatalf("create budget: %v", err)
			}
			for i, step := range tc.steps {
				var admins common.TokenList
				if step.kind == model.KindBudget {
					admins = common.TokenList{"user:stud@x"}
				}
				node, err := svc.CreateNode(tree.CreateNodeRequest{
					ParentID: budget.ID, Kind: step.kind, Name: "n", Reason: "r",
					Limit: step.limit, AdminScope: admins, InheritsLimit: step.inherit,
				}, model.UIActor("stud@x"), "stud@x", studTokens)
				if step.want == "" {
					if !errors.Is(err, common.ErrForbidden) {
						t.Fatalf("step %d: want a refusal, got %v (status %q)", i, err, node.Status)
					}
					continue
				}
				if step.want == "refused" {
					if err == nil {
						t.Fatalf("step %d: want a refusal, got status %q", i, node.Status)
					}
					continue
				}
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if node.Status != step.want {
					t.Fatalf("step %d (%s of %v): want %q, got %q", i, step.kind, step.limit, step.want, node.Status)
				}
				if node.Status == model.StatusApproved {
					last := node.History[len(node.History)-1]
					if last.Actor != "system:auto-approval" {
						t.Fatalf("step %d: approval should be recorded as auto-approval, got actor %q", i, last.Actor)
					}
				}
			}
		})
	}
}

// A budget with a per-requester limit takes no sub-budget requests: creating or editing one so that it would (allow_sub_budget_requests left out means true) is refused, whichever of the two fields comes last. Its managers still create sub-budgets directly, and its requesters are told to request a project.
func TestAutoApprove_PerRequesterLimitRefusesSubBudgetRequests(t *testing.T) {
	perRequester := &model.AutoApprove{PerRequesterLimit: cores(4)}
	newBudget := func(policy *model.AutoApprove, allowSub *bool) tree.CreateNodeRequest {
		return tree.CreateNodeRequest{
			ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Course", Reason: "t",
			Limit: cores(10), AdminScope: common.TokenList{"group:root"},
			EligibleRequesters: common.TokenList{"user:stud@x"},
			AutoApprove:        policy, AllowSubBudgetRequests: allowSub,
		}
	}
	wantRefused := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || errors.Is(err, common.ErrForbidden) || !strings.Contains(err.Error(), "allow_sub_budget_requests") {
			t.Fatalf("want a validation error naming allow_sub_budget_requests, got %v", err)
		}
	}

	t.Run("create", func(t *testing.T) {
		svc, _ := newSvc(t, common.TokenList{"group:root"})
		for _, allowSub := range []*bool{nil, new(true)} {
			_, err := svc.CreateNode(newBudget(perRequester, allowSub), model.UIActor("root@x"), "root@x", rootTokens)
			wantRefused(t, err)
		}
		for _, req := range []tree.CreateNodeRequest{newBudget(perRequester, new(false)), newBudget(&model.AutoApprove{}, nil), newBudget(nil, nil)} {
			if _, err := svc.CreateNode(req, model.UIActor("root@x"), "root@x", rootTokens); err != nil {
				t.Fatalf("create %+v: %v", req.AutoApprove, err)
			}
		}
	})

	t.Run("update sets the limit on a budget that takes sub-budget requests", func(t *testing.T) {
		svc, _ := newSvc(t, common.TokenList{"group:root"})
		for _, allowSub := range []*bool{nil, new(true)} {
			budget, err := svc.CreateNode(newBudget(&model.AutoApprove{}, allowSub), model.UIActor("root@x"), "root@x", rootTokens)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			_, err = svc.UpdateNode(budget.ID, tree.UpdateNodeRequest{AutoApprove: perRequester}, model.UIActor("root@x"), rootTokens)
			wantRefused(t, err)
			// Both in one edit is fine.
			if _, err := svc.UpdateNode(budget.ID, tree.UpdateNodeRequest{AutoApprove: perRequester, AllowSubBudgetRequests: new(false)}, model.UIActor("root@x"), rootTokens); err != nil {
				t.Fatalf("update with both fields: %v", err)
			}
		}
	})

	t.Run("update allows sub-budget requests on a budget with the limit", func(t *testing.T) {
		svc, _ := newSvc(t, common.TokenList{"group:root"})
		budget, err := svc.CreateNode(newBudget(perRequester, new(false)), model.UIActor("root@x"), "root@x", rootTokens)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		_, err = svc.UpdateNode(budget.ID, tree.UpdateNodeRequest{AllowSubBudgetRequests: new(true)}, model.UIActor("root@x"), rootTokens)
		wantRefused(t, err)
		// Unrelated edits and switching to a pool are not affected.
		if _, err := svc.UpdateNode(budget.ID, tree.UpdateNodeRequest{Name: new("Renamed")}, model.UIActor("root@x"), rootTokens); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if _, err := svc.UpdateNode(budget.ID, tree.UpdateNodeRequest{AutoApprove: &model.AutoApprove{}, AllowSubBudgetRequests: new(true)}, model.UIActor("root@x"), rootTokens); err != nil {
			t.Fatalf("switch to a pool: %v", err)
		}
	})

	t.Run("managers still create sub-budgets, requesters are refused", func(t *testing.T) {
		svc, _ := newSvc(t, common.TokenList{"group:root"})
		budget, err := svc.CreateNode(newBudget(perRequester, new(false)), model.UIActor("root@x"), "root@x", rootTokens)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		sub, err := svc.CreateNode(tree.CreateNodeRequest{
			ParentID: budget.ID, Kind: model.KindBudget, Name: "Group", Reason: "r",
			Limit: cores(6), AdminScope: common.TokenList{"user:stud@x"},
		}, model.UIActor("root@x"), "root@x", rootTokens)
		if err != nil || sub.Status != model.StatusApproved {
			t.Fatalf("manager's sub-budget: %v (status %q)", err, sub.Status)
		}
		_, err = svc.CreateNode(tree.CreateNodeRequest{
			ParentID: budget.ID, Kind: model.KindBudget, Name: "Mine", Reason: "r",
			Limit: cores(2), AdminScope: common.TokenList{"user:stud@x"},
		}, model.UIActor("stud@x"), "stud@x", studTokens)
		if !errors.Is(err, common.ErrForbidden) {
			t.Fatalf("requester's sub-budget: want a refusal, got %v", err)
		}
	})
}
