package tree

import (
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// NodePage is the envelope every node listing returns.
//
// Total is the number of matches BEFORE limit/offset is applied. Without it a
// full page and a truncated result look the same to a client, and the budget
// tree silently dropped everything past the page size — a course budget with
// 600 projects showed 500 and said nothing about the rest.
type NodePage struct {
	Items  []Node `json:"items"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// newNodePage wraps a page of results together with the query bounds that
// produced it.
func newNodePage(items []Node, total, limit, offset int) NodePage {
	if items == nil {
		items = []Node{}
	}
	return NodePage{Items: items, Total: total, Limit: limit, Offset: offset}
}

// CreateNodeRequest creates a child node under ParentID. Two entry paths share it:
//   - a manager of the parent chain creates the child directly (status approved),
//   - an eligible requester submits a request (status pending, possibly auto-approved).
type CreateNodeRequest struct {
	ParentID string `json:"parent_id" binding:"required"`
	Kind     string `json:"kind" binding:"required,oneof=budget project"`
	Name     string `json:"name"`
	Reason   string `json:"reason" binding:"required"`
	// Limit is the requested allocation (leaves) or cap (budgets).
	Limit common.ProjectQuota `json:"limit" binding:"required"`
	// TerminationDate may not lie after the end of any budget above. Omitted
	// under a budget that ends, it is that budget's end.
	TerminationDate *string `json:"termination_date"`
	// Leaf fields.
	AuthorizedUsers []common.AuthorizedUser `json:"authorized_users"`
	// Budget fields.
	AdminScope         common.TokenList `json:"admin_scope"`
	EligibleRequesters common.TokenList `json:"eligible_requesters"`
	AutoApprove        *AutoApprove     `json:"auto_approve"`
	// AllowSubBudgetRequests defaults to true when omitted.
	AllowSubBudgetRequests *bool `json:"allow_sub_budget_requests"`
	// AllowRequestsBeyondAutoApprove defaults to true when omitted.
	AllowRequestsBeyondAutoApprove *bool `json:"allow_requests_beyond_auto_approve"`
	// AutoApproveExtensions defaults to true when omitted.
	AutoApproveExtensions *bool `json:"auto_approve_extensions"`
	// MaxProjectTermDays may not exceed the cap of a budget above; omitted
	// under a budget with a cap, it is that cap.
	MaxProjectTermDays *int `json:"max_project_term_days"`
	// InheritsLimit creates a budget that passes on the limit of its parent
	// (Node.InheritsLimit); Limit is then ignored. Managers only.
	InheritsLimit bool `json:"inherits_limit"`
}

// UpdateNodeRequest is a direct edit that takes effect immediately (no approval
// cycle). Policy fields require a manager of the node or its ancestors; Limit
// requires a manager of the parent chain (you cannot raise your own budget).
type UpdateNodeRequest struct {
	Name                           *string           `json:"name"`
	AdminScope                     *common.TokenList `json:"admin_scope"`
	EligibleRequesters             *common.TokenList `json:"eligible_requesters"`
	AutoApprove                    *AutoApprove      `json:"auto_approve"`
	ClearAutoApprove               bool              `json:"clear_auto_approve"`
	AllowSubBudgetRequests         *bool             `json:"allow_sub_budget_requests"`
	AllowRequestsBeyondAutoApprove *bool             `json:"allow_requests_beyond_auto_approve"`
	AutoApproveExtensions          *bool             `json:"auto_approve_extensions"`
	// MaxProjectTermDays may not exceed the cap of a budget above. Lowering it
	// is carried down: sub-budgets with a longer cap take the new one, and
	// projects ending later than that many days from today are shortened.
	// ClearMaxProjectTermDays removes the cap, only where no budget above has
	// one.
	MaxProjectTermDays      *int                 `json:"max_project_term_days"`
	ClearMaxProjectTermDays bool                 `json:"clear_max_project_term_days"`
	Limit                   *common.ProjectQuota `json:"limit"`
	TerminationDate         *string              `json:"termination_date"`
	// TerminationDate may not lie after the end of any budget above. A budget's
	// new end is carried down: every node below it that ends later, or never,
	// ends with it, waiting requests and proposals included.
	// ClearTerminationDate removes an existing end date: the node then runs
	// until somebody changes it — only allowed where no budget above ends. A
	// nil TerminationDate cannot express this — it means "leave as is" — so
	// removal needs its own flag, like ClearAutoApprove.
	ClearTerminationDate bool `json:"clear_termination_date"`
	// InheritsLimit switches passing on the parent's limit on or off
	// (Node.InheritsLimit). Switched on, Limit must be absent; switched off,
	// the budget keeps the inherited values unless Limit sets others.
	InheritsLimit *bool `json:"inherits_limit"`
}

// ChangeNodeRequest proposes changes to a node. On an active leaf a change that
// gives something back, changes members only, or that the budget's auto-approve
// policy covers takes effect at once; anything else waits for a manager of the
// parent chain in change_pending.
type ChangeNodeRequest struct {
	Limit           *common.ProjectQuota     `json:"limit"`
	TerminationDate *string                  `json:"termination_date"`
	AuthorizedUsers *[]common.AuthorizedUser `json:"authorized_users"`
	Reason          *string                  `json:"reason"`
}

// ApproveNodeRequest approves a pending or change_pending node. ModifiedLimit
// lets the approver grant a different limit than requested.
type ApproveNodeRequest struct {
	ModifiedLimit *common.ProjectQuota `json:"modified_limit"`
}

// RejectNodeRequest rejects a pending node (→ rejected) or discards the pending
// changes of a change_pending node (→ back to approved).
type RejectNodeRequest struct {
	Reason *string `json:"reason"`
}

// ReparentNodeRequest moves a node under a new parent. Requires managing the
// current parent chain AND the new parent; active nodes are capacity-checked
// against the new ancestor chain.
type ReparentNodeRequest struct {
	NewParentID string `json:"new_parent_id" binding:"required"`
}

// TransferOwnerRequest hands a leaf to a new responsible person.
type TransferOwnerRequest struct {
	// NewOwner is the new owner's email (with or without the "user:" prefix).
	NewOwner string `json:"new_owner" binding:"required"`
}

// ReleaseNodeRequest releases an approved project. Delete marks it for deletion
// in the same step (see Service.RequestDeletion), instead of archiving it and
// keeping it for the grace period.
type ReleaseNodeRequest struct {
	Delete bool `json:"delete,omitempty"`
}

// PromoteNodeRequest converts an imported leaf into a managed request: the leaf is
// reparented under NewParentID, gets an owner, and is flagged so the reconciler
// tags the existing OpenStack project and transitions the leaf to pending.
type PromoteNodeRequest struct {
	NewParentID string `json:"new_parent_id" binding:"required"`
	// Owner is the future owner's email (with or without the "user:" prefix).
	Owner           string  `json:"owner" binding:"required"`
	Reason          string  `json:"reason" binding:"required"`
	TerminationDate *string `json:"termination_date"`
	// Limit overrides the imported OpenStack quota. When omitted the current
	// imported limit is kept.
	Limit           common.ProjectQuota     `json:"limit,omitempty"`
	AuthorizedUsers []common.AuthorizedUser `json:"authorized_users"`
}
