package tree_test

import (
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// subBudgetStep is one request by "stud@x" under the fixture budget: a sub-budget (managed by admins, stud by default) or a project.
type subBudgetStep struct {
	kind    string
	limit   common.ProjectQuota
	admins  common.TokenList
	inherit bool
	want    string // expected status; "" expects errForbidden
}

func subBudget(n int, want string) subBudgetStep {
	return subBudgetStep{kind: model.KindBudget, limit: cores(n), want: want}
}

func project(n int, want string) subBudgetStep {
	return subBudgetStep{kind: model.KindProject, limit: cores(n), want: want}
}

// A sub-budget requested from a budget with an auto-approve policy is granted like a project: from a pool whenever the edge rule allows it (a sub-budget's limit is a cap, not a reservation), under a per-requester limit only while the requester's projects plus the full limits of the sub-budgets they requested fit it — whoever they hand the management to.
func TestAutoApprove_SubBudgets(t *testing.T) {
	perRequester := &model.AutoApprove{PerRequesterLimit: cores(6)}
	cases := []struct {
		name        string
		budgetLimit common.ProjectQuota
		policy      *model.AutoApprove
		hard        bool
		steps       []subBudgetStep
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
			name:   "pool leaves a request for nothing to a manager",
			policy: &model.AutoApprove{},
			steps:  []subBudgetStep{subBudget(0, model.StatusPending)},
		},
		{
			name:   "per-requester grants what fits",
			policy: perRequester,
			steps:  []subBudgetStep{subBudget(4, model.StatusApproved), subBudget(2, model.StatusApproved)},
		},
		{
			name:   "per-requester counts projects and sub-budgets together",
			policy: perRequester,
			steps:  []subBudgetStep{project(3, model.StatusApproved), subBudget(2, model.StatusApproved), subBudget(2, model.StatusPending), project(1, model.StatusApproved)},
		},
		{
			name:   "project auto-approve counts the requester's sub-budgets",
			policy: perRequester,
			steps:  []subBudgetStep{subBudget(5, model.StatusApproved), project(2, model.StatusPending), project(1, model.StatusApproved)},
		},
		{
			name:   "a sub-budget delegated to someone else still counts against the requester",
			policy: perRequester,
			steps: []subBudgetStep{
				{kind: model.KindBudget, limit: cores(4), admins: common.TokenList{"user:dean@x", "group:deans"}, want: model.StatusApproved},
				{kind: model.KindBudget, limit: cores(3), admins: common.TokenList{"user:dean@x"}, want: model.StatusPending},
				project(3, model.StatusPending),
			},
		},
		{
			name:        "per-requester leaves an unlimited sub-budget to a manager",
			budgetLimit: cores(common.UnlimitedQuota),
			policy:      perRequester,
			steps:       []subBudgetStep{subBudget(common.UnlimitedQuota, model.StatusPending)},
		},
		{
			name:   "an inheriting sub-budget is for managers only",
			policy: perRequester,
			steps:  []subBudgetStep{{kind: model.KindBudget, inherit: true, want: ""}},
		},
		{
			name:   "a hard limit refuses a sub-budget beyond the share",
			policy: perRequester,
			hard:   true,
			steps:  []subBudgetStep{subBudget(4, model.StatusApproved), subBudget(3, "")},
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
			limit := tc.budgetLimit
			if limit == nil {
				limit = cores(10)
			}
			req := tree.CreateNodeRequest{
				ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Faculty", Reason: "t",
				Limit: limit, AdminScope: common.TokenList{"group:root"},
				EligibleRequesters: common.TokenList{"user:stud@x"},
				AutoApprove:        tc.policy,
			}
			if tc.hard {
				req.AllowRequestsBeyondAutoApprove = new(false)
			}
			budget, err := svc.CreateNode(req, model.UIActor("root@x"), "root@x", common.TokenList{"user:root@x", "group:root"})
			if err != nil {
				t.Fatalf("create budget: %v", err)
			}
			for i, step := range tc.steps {
				admins := step.admins
				if step.kind == model.KindBudget && admins == nil {
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
