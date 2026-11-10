package tree

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// NodePage is the envelope every node listing returns.
//
// Total is the number of matches BEFORE limit/offset is applied. Without it a
// full page and a truncated result look the same to a client, and the budget
// tree silently dropped everything past the page size — a course budget with
// 600 projects showed 500 and said nothing about the rest.
type NodePage struct {
	Items  []model.Node `json:"items"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// newNodePage wraps a page of results together with the query bounds that
// produced it.
func newNodePage(items []model.Node, total, limit, offset int) NodePage {
	if items == nil {
		items = []model.Node{}
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
	AdminScope         common.TokenList   `json:"admin_scope"`
	EligibleRequesters common.TokenList   `json:"eligible_requesters"`
	AutoApprove        *model.AutoApprove `json:"auto_approve"`
	// AllowSubBudgetRequests defaults to true when omitted; it must be false where AutoApprove has a per-requester limit.
	AllowSubBudgetRequests *bool `json:"allow_sub_budget_requests"`
	// AllowRequestsBeyondAutoApprove defaults to true when omitted.
	AllowRequestsBeyondAutoApprove *bool `json:"allow_requests_beyond_auto_approve"`
	// AutoApproveExtensions defaults to true when omitted.
	AutoApproveExtensions *bool `json:"auto_approve_extensions"`
	// MaxProjectTermDays may not exceed the cap of a budget above; omitted
	// under a budget with a cap, it is that cap.
	MaxProjectTermDays *int `json:"max_project_term_days"`
	// ReleaseMax are the budget's own release maximums (model.ReleaseMax); each value at most what the budgets above allow. Omitted or empty, the budget sets none of its own and those above apply.
	ReleaseMax *model.ReleaseMax `json:"release_max"`
	// WorkspaceScratchGB is the budget's own local storage per workspace in GB (model.WorkspaceScratchGB), 1 to model.MaxWorkspaceScratchGB and at most what the budgets above allow. Omitted, the budget sets none of its own and those above apply.
	WorkspaceScratchGB *int `json:"workspace_scratch_gb" example:"5"`
	// BatchPriority is the budget's own batch priority (model.BatchPriority), "guaranteed" or "preemptible"; it may not guarantee what the budgets above make preemptible. Omitted, the budget sets none of its own and those above apply.
	BatchPriority *string `json:"batch_priority" example:"preemptible" enums:"guaranteed,preemptible"`
	// BatchJobs switches batch jobs on for the projects below the budget (model.BatchJobs): jobs per person and the longest run time in hours, only where the budgets above have them and each at most what they allow. Omitted, the budget has none, unless it inherits its limit (InheritsLimit): then it passes on those above and must not set any.
	BatchJobs *model.BatchJobs `json:"batch_jobs"`
	// InheritsLimit creates a budget that passes on the limit of its parent
	// (Node.InheritsLimit); Limit is then ignored. Managers only.
	InheritsLimit bool `json:"inherits_limit"`
}

// UpdateNodeRequest is a direct edit that takes effect immediately (no approval
// cycle). Policy fields require a manager of the node or its ancestors; Limit
// requires a manager of the parent chain (you cannot raise your own budget).
//
// A field left out stays as it is; a field sent as null is removed (auto_approve, max_project_term_days, release_max, workspace_scratch_gb, batch_priority, batch_jobs, termination_date). Decided 10.11.2026 in place of the clear_* flags, which are refused since (UnmarshalJSON). The Clear* fields below carry the null into the service and are not part of the JSON.
type UpdateNodeRequest struct {
	Name                           *string            `json:"name,omitempty"`
	AdminScope                     *common.TokenList  `json:"admin_scope,omitempty"`
	EligibleRequesters             *common.TokenList  `json:"eligible_requesters,omitempty"`
	AutoApprove                    *model.AutoApprove `json:"auto_approve,omitempty"`
	ClearAutoApprove               bool               `json:"-" swaggerignore:"true"`
	AllowSubBudgetRequests         *bool              `json:"allow_sub_budget_requests,omitempty"`
	AllowRequestsBeyondAutoApprove *bool              `json:"allow_requests_beyond_auto_approve,omitempty"`
	AutoApproveExtensions          *bool              `json:"auto_approve_extensions,omitempty"`
	// MaxProjectTermDays may not exceed the cap of a budget above. Lowering it
	// is carried down: sub-budgets with a longer cap take the new one, and
	// projects ending later than that many days from today are shortened.
	// null removes the cap, only where no budget above has one.
	MaxProjectTermDays      *int `json:"max_project_term_days,omitempty"`
	ClearMaxProjectTermDays bool `json:"-" swaggerignore:"true"`
	// ReleaseMax replaces the budget's own release maximums as a whole (model.ReleaseMax); a field left out is not set here, so the budgets above decide it, and an empty object or null removes them all. Each value at most what the budgets above allow (Node.InheritedReleaseMax). Managers of the budget or above, like the other policy fields. Omitted, they stay as they are.
	ReleaseMax *model.ReleaseMax `json:"release_max,omitempty"`
	// WorkspaceScratchGB sets the budget's own local storage per workspace in GB (model.WorkspaceScratchGB): 1 to model.MaxWorkspaceScratchGB and at most what the budgets above allow (Node.InheritedWorkspaceScratchGB). null removes it, so the budgets above (or the AI service's default) apply. Managers of the budget or above, like the other policy fields. Omitted, it stays as it is.
	WorkspaceScratchGB      *int `json:"workspace_scratch_gb,omitempty" example:"5"`
	ClearWorkspaceScratchGB bool `json:"-" swaggerignore:"true"`
	// BatchPriority sets the budget's own batch priority (model.BatchPriority): "guaranteed" or "preemptible", and not "guaranteed" where the budgets above make it preemptible (Node.InheritedBatchPriority). null removes it, so the budgets above apply (none: preemptible). Managers of the budget or above, like the other policy fields. Omitted, it stays as it is.
	BatchPriority      *string `json:"batch_priority,omitempty" example:"preemptible" enums:"guaranteed,preemptible"`
	ClearBatchPriority bool    `json:"-" swaggerignore:"true"`
	// BatchJobs switches batch jobs on for the projects below the budget or changes their bounds (model.BatchJobs): jobs per person and the longest run time in hours, only where the budgets above have them (Node.InheritedBatchJobs) and each at most what they allow; the root sets the most anybody gets. null switches them off here and below. Not on a budget that inherits its limit: it passes on those above. Managers of the budget or above, like the other policy fields. Omitted, they stay as they are.
	BatchJobs       *model.BatchJobs     `json:"batch_jobs,omitempty"`
	ClearBatchJobs  bool                 `json:"-" swaggerignore:"true"`
	Limit           *common.ProjectQuota `json:"limit,omitempty"`
	TerminationDate *string              `json:"termination_date,omitempty"`
	// TerminationDate may not lie after the end of any budget above. A budget's
	// new end is carried down: every node below it that ends later, or never,
	// ends with it, waiting requests and proposals included.
	// termination_date: null removes an existing end date: the node then runs
	// until somebody changes it — only allowed where no budget above ends.
	ClearTerminationDate bool `json:"-" swaggerignore:"true"`
	// InheritsLimit switches passing on the parent's limit on or off
	// (Node.InheritsLimit). Switched on, Limit must be absent; switched off,
	// the budget keeps the inherited values unless Limit sets others.
	InheritsLimit *bool `json:"inherits_limit,omitempty"`
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
	// ExpectedRevision is the node's revision (Node.Revision) as the approver saw it. The approval is refused with a conflict when the request changed since — amended, replaced by another proposal — so nobody approves something they never saw.
	ExpectedRevision *int `json:"expected_revision" binding:"required"`
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

// nullableUpdateFields are the fields of UpdateNodeRequest that null removes, with the flag that used to do it.
var nullableUpdateFields = []struct{ field, formerFlag string }{
	{"auto_approve", "clear_auto_approve"},
	{"max_project_term_days", "clear_max_project_term_days"},
	{"workspace_scratch_gb", "clear_workspace_scratch_gb"},
	{"batch_priority", "clear_batch_priority"},
	{"batch_jobs", "clear_batch_jobs"},
	{"termination_date", "clear_termination_date"},
}

// UnmarshalJSON reads an UpdateNodeRequest and tells a field sent as null (remove it) from one left out (leave it): encoding/json gives both as nil. A former clear_* flag is refused with the field to send instead.
func (r *UpdateNodeRequest) UnmarshalJSON(b []byte) error {
	type plain UpdateNodeRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	isNull := func(k string) bool {
		v, ok := raw[k]
		return ok && string(bytes.TrimSpace(v)) == "null"
	}
	for _, f := range nullableUpdateFields {
		if _, ok := raw[f.formerFlag]; ok {
			return fmt.Errorf("%s is no longer supported: send \"%s\": null to remove it", f.formerFlag, f.field)
		}
	}
	*r = UpdateNodeRequest(p)
	r.ClearAutoApprove = isNull("auto_approve")
	r.ClearMaxProjectTermDays = isNull("max_project_term_days")
	r.ClearWorkspaceScratchGB = isNull("workspace_scratch_gb")
	r.ClearBatchPriority = isNull("batch_priority")
	r.ClearBatchJobs = isNull("batch_jobs")
	r.ClearTerminationDate = isNull("termination_date")
	if isNull("release_max") {
		r.ReleaseMax = &model.ReleaseMax{}
	}
	return nil
}
