// Package tree implements the unified resource-delegation model: a single tree of
// nodes in which budgets are the inner nodes and project requests are the leaves.
//
// Three rules replace the previous delegation/project/eligibility special cases:
//
//  1. Usage(N) is the sum of the Limits of all active (approved / change_pending)
//     descendant LEAVES of N.
//  2. Managing a node (approve/reject children, edit, delete, reparent) requires a
//     token in the AdminScope of the node's PARENT CHAIN. Root admins are simply the
//     AdminScope of the root node — there is no separate bypass code path.
//  3. Requesting under N requires a token in N.EligibleRequesters; the child starts
//     pending. If N carries an AutoApprove policy and the requester's cumulative
//     active usage under N stays within the per-requester limit (and every ancestor
//     has capacity), the request is approved immediately.
package tree

import (
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// SubBudgetRequestsAllowed reports whether eligible requesters may request a
// sub-budget under this node. Unset means allowed, so existing budgets keep
// behaving as before.
func (n *Node) SubBudgetRequestsAllowed() bool {
	return n == nil || n.AllowSubBudgetRequests == nil || *n.AllowSubBudgetRequests
}

// Node kinds.
const (
	KindBudget  = "budget"  // inner node: a delegated budget
	KindProject = "project" // leaf: a concrete resource allocation request
)

// Node lifecycle statuses.
//
// pending → approved | rejected
// approved → change_pending → approved (change applied or discarded)
// approved → released (leaves only; drives OpenStack deprovisioning)
// imported: synthetic read-only leaf created by the reconciler for an OpenStack
// project that is not tracked here; lives under the "unassigned" node until promoted.
//
// Note: there is deliberately NO change_rejected status. Rejecting a change on an
// approved node discards the pending changes and returns the node to approved —
// the previously approved state stays fully valid (the old model killed the whole
// project on a rejected change, which orphaned its OpenStack resources).
const (
	StatusPending       = "pending"
	StatusApproved      = "approved"
	StatusChangePending = "change_pending"
	StatusRejected      = "rejected"
	StatusReleased      = "released"
	StatusImported      = "imported"
)

// Node flags — orthogonal markers that modify reconciler behaviour.
const (
	// FlagPromoteOnReconcile is set by the promote API on an imported leaf. The
	// reconciler tags the existing OpenStack project with the node ID, transitions
	// the node to "pending" and removes the flag; the node then flows through the
	// normal approval cycle under its new parent.
	FlagPromoteOnReconcile = "promote_on_reconcile"
)

// Well-known bootstrapped node IDs.
const (
	// RootNodeID is the single parentless node. Its AdminScope is synchronized from
	// ROOT_ADMIN_TOKENS on every startup — the config is the source of truth.
	RootNodeID = "root"
	// UnassignedNodeID is the collection point for reconciler-imported leaves. Its
	// limit is all-zero, which makes approving anything under it arithmetically
	// impossible; imported leaves must be promoted (reparented) into a real budget.
	UnassignedNodeID = "unassigned"
)

// ActiveStatuses are the leaf states that always consume capacity: the project
// is live in OpenStack and its limit is in force.
var ActiveStatuses = []string{StatusApproved, StatusChangePending}

// ActiveStatusesWithReleased adds released leaves to them, which is what the
// accounting charges by default — see Accounting.ChargeReleased. Releasing does
// not delete the OpenStack project; it hands the deletion to OpenStack via the
// pending-deletion tag, and until that happens the servers are still running.
var ActiveStatusesWithReleased = []string{StatusApproved, StatusChangePending, StatusReleased}

// ReconcilableStatuses are the leaf states the reconciler projects into OpenStack.
// change_pending leaves keep their currently approved limit active while the
// proposed change awaits approval.
var ReconcilableStatuses = []string{StatusApproved, StatusChangePending}

// ManagedStatuses are the states in which a node awaits a manager decision.
var ManagedStatuses = []string{StatusPending, StatusChangePending}

// KnownStatuses contains every real (non-imported) status. The reconciler uses it
// to recognise OpenStack projects that are already tracked by a node in any state,
// so they are not re-imported.
var KnownStatuses = []string{
	StatusPending,
	StatusApproved,
	StatusChangePending,
	StatusRejected,
	StatusReleased,
}

// AutoApprove is the auto-approve policy of a budget node. When set, an eligible
// requester's leaf is approved immediately as long as the requester's cumulative
// active usage under this budget (matched by Owner) stays within PerRequesterLimit
// and all ancestors have remaining capacity. This replaces the former "allowance"
// delegation strategy — the per-requester cap and the budget's own total Limit are
// now two separate, independently meaningful values.
type AutoApprove struct {
	PerRequesterLimit common.ProjectQuota `json:"per_requester_limit"`
}

// PendingChanges holds proposed modifications awaiting approval (status change_pending).
type PendingChanges struct {
	Limit           *common.ProjectQuota     `json:"limit,omitempty"`
	TerminationDate *string                  `json:"termination_date,omitempty"`
	AuthorizedUsers *[]common.AuthorizedUser `json:"authorized_users,omitempty"`
}

// HistoryEntry records a lifecycle event on a node.
type HistoryEntry struct {
	Timestamp           string               `json:"timestamp"`
	Event               string               `json:"event"`
	Actor               string               `json:"actor"`
	StatusFrom          *string              `json:"status_from,omitempty"`
	StatusTo            string               `json:"status_to"`
	LimitFrom           *common.ProjectQuota `json:"limit_from,omitempty"`
	LimitTo             *common.ProjectQuota `json:"limit_to,omitempty"`
	TerminationDateFrom *string              `json:"termination_date_from,omitempty"`
	TerminationDateTo   *string              `json:"termination_date_to,omitempty"`
	ParentFrom          *string              `json:"parent_from,omitempty"`
	ParentTo            *string              `json:"parent_to,omitempty"`
	OwnerFrom           *string              `json:"owner_from,omitempty"`
	OwnerTo             *string              `json:"owner_to,omitempty"`
	Reason              *string              `json:"reason,omitempty"`
}

// StatusUsage groups the aggregated limits and contributing leaf IDs for one status.
type StatusUsage struct {
	Limit   common.ProjectQuota `json:"limit"`
	NodeIDs []string            `json:"node_ids"`
}

// UsageByStatus maps leaf status → aggregated usage for that status.
type UsageByStatus map[string]StatusUsage

// Total collapses all status buckets into a single quota by summing each resource.
func (u UsageByStatus) Total(resourceIDs []string) common.ProjectQuota {
	out := make(common.ProjectQuota)
	for _, statusUsage := range u {
		for _, id := range resourceIDs {
			out[id] += statusUsage.Limit[id]
		}
	}
	return out
}

// UsagePerNode maps node ID → UsageByStatus rolled up over that node's subtree.
type UsagePerNode map[string]UsageByStatus

// Node is the single entity of the model. Budgets are inner nodes, project
// requests are leaves; both share one lifecycle, one authorization rule and one
// capacity mechanism.
type Node struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"` // KindBudget | KindProject
	ParentID *string `json:"parent_id"`
	Status   string  `json:"status"`

	// Name is the human-readable label (budgets always, leaves optionally).
	Name string `json:"name,omitempty"`
	// Reason documents why the node was requested/created.
	Reason string `json:"reason,omitempty"`

	// Limit is the capacity cap for budgets and the concrete allocation for leaves.
	// Budget limits may use common.UnlimitedQuota (-1); leaf limits must be >= 0.
	Limit common.ProjectQuota `json:"limit"`

	// AdminScope holds the MANAGER tokens of this node: they approve/reject child
	// requests, edit this node's policy and create children directly. Managing
	// authority additionally flows down from every ancestor's AdminScope.
	// Consumption rights are deliberately NOT part of AdminScope (see
	// EligibleRequesters) — the old model's dual use of admin scope let allowance
	// members approve each other.
	AdminScope common.TokenList `json:"admin_scope,omitempty"`
	// EligibleRequesters holds the tokens allowed to request child nodes under
	// this node.
	EligibleRequesters common.TokenList `json:"eligible_requesters,omitempty"`
	// AutoApprove, when set on a budget, enables per-requester auto-approval.
	AutoApprove *AutoApprove `json:"auto_approve,omitempty"`
	// AllowSubBudgetRequests controls whether EligibleRequesters may ask for a
	// sub-budget here, or only for projects. It does NOT restrict managers: they
	// create sub-budgets directly and this governs requests only.
	// nil means allowed — the default, and the meaning of every node written
	// before this field existed.
	AllowSubBudgetRequests *bool `json:"allow_sub_budget_requests,omitempty"`

	// Owner is the single responsible person of a leaf ("user:<email>").
	// Additional participants are granted via AuthorizedUsers. Managers of the
	// parent chain may transfer ownership.
	Owner           string                  `json:"owner,omitempty"`
	AuthorizedUsers []common.AuthorizedUser `json:"authorized_users,omitempty"`

	// TerminationDate is the intended end of life (leaves) or validity end
	// (budgets, formerly EndDate). Informative for now; enforcement is a
	// follow-up task (expiry sweep in the reconciler).
	TerminationDate *string `json:"termination_date,omitempty"`

	Pending *PendingChanges `json:"pending,omitempty"`
	History []HistoryEntry  `json:"history,omitempty"`
	Flags   []string        `json:"flags,omitempty"`

	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`

	// ── OpenStack linkage (leaves, maintained by the reconciler) ──────────────
	OSProjectID     string `json:"os_project_id,omitempty"`
	OSProjectName   string `json:"os_project_name,omitempty"`
	OSOvercommitted bool   `json:"os_overcommitted,omitempty"`
	// OSInUse is what the project actually consumes in OpenStack, as opposed to
	// the Limit it was granted. Only resources OpenStack reports an in-use
	// counter for appear here; a missing key means "not measured", not zero.
	//
	// It exists because a quota reduction below current usage is something
	// OpenStack accepts silently — the servers keep running and only new ones
	// are refused. Without this the platform sees only the smaller claim and a
	// shrink looks like capacity handed back when nothing was.
	OSInUse                  common.ProjectQuota              `json:"os_in_use,omitempty"`
	ExternalGroupAssignments []common.ExternalGroupAssignment `json:"external_group_assignments,omitempty"`

	// ChildCount is attached to API responses (never persisted): the number of
	// direct children, so a client can tell an empty budget from one whose
	// children have not been fetched yet. Children are loaded lazily, so without
	// this every budget looks expandable.
	ChildCount int `json:"child_count"`
	// Usage is attached to API responses (never persisted): the rollup of active
	// descendant leaves for budgets.
	Usage UsageByStatus `json:"usage,omitempty"`
	// ParentName is attached to API responses (never persisted): the display name
	// of ParentID, so a client can name the budget a node is paid from without
	// fetching each parent separately. Empty for roots.
	ParentName string `json:"parent_name,omitempty"`
}

// IsLeaf reports whether the node is a project leaf.
func (n *Node) IsLeaf() bool { return n.Kind == KindProject }

// OwnerEmail returns the email part of the owner token ("" when unset).
func (n *Node) OwnerEmail() string {
	const prefix = "user:"
	if len(n.Owner) > len(prefix) && n.Owner[:len(prefix)] == prefix {
		return n.Owner[len(prefix):]
	}
	return ""
}

// IsTerminalStatus reports whether a node in this status must not transition further.
func IsTerminalStatus(status string) bool {
	return status == StatusRejected || status == StatusReleased
}
