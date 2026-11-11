package tree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

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
	// Caps are the budget's own caps (common.Cap, /v1/config lists them) by id; each within its cap and at most what the budgets above allow, a Grant cap only where they have it. Omitted, the budget sets none of its own and those above apply.
	Caps common.Caps `json:"caps"`
	// InheritsLimit creates a budget that passes on the limit of its parent
	// (Node.InheritsLimit); Limit is then ignored. Managers only.
	InheritsLimit bool `json:"inherits_limit"`
}

// UpdateNodeRequest is a direct edit that takes effect immediately (no approval
// cycle). Policy fields require a manager of the node or its ancestors; Limit
// requires a manager of the parent chain (you cannot raise your own budget).
//
// A field left out stays as it is; a field sent as null is removed (auto_approve, max_project_term_days, caps, termination_date). Decided 10.11.2026 in place of the clear_* flags, which are refused since (UnmarshalJSON). The Clear* fields below carry the null into the service and are not part of the JSON.
type UpdateNodeRequest struct {
	Name                           *string            `json:"name,omitempty"`
	AdminScope                     *common.TokenList  `json:"admin_scope,omitempty"`
	EligibleRequesters             *common.TokenList  `json:"eligible_requesters,omitempty"`
	AutoApprove                    *model.AutoApprove `json:"auto_approve,omitempty"`
	ClearAutoApprove               bool               `json:"-" swaggerignore:"true"`
	AllowSubBudgetRequests         *bool              `json:"allow_sub_budget_requests,omitempty"`
	AllowRequestsBeyondAutoApprove *bool              `json:"allow_requests_beyond_auto_approve,omitempty"`
	AutoApproveExtensions          *bool              `json:"auto_approve_extensions,omitempty"`
	// Reason is the budget's purpose (Node.Reason), required and at most common.MaxReasonLength characters as on creation. Budgets only: on a project it is refused like every edit beyond name and admins. Managers of the budget or above, like the other policy fields.
	Reason *string `json:"reason,omitempty"`
	// MaxProjectTermDays may not exceed the cap of a budget above. Lowering it
	// is carried down: sub-budgets with a longer cap take the new one, and
	// projects ending later than that many days from today are shortened.
	// null removes the cap, only where no budget above has one.
	MaxProjectTermDays      *int `json:"max_project_term_days,omitempty"`
	ClearMaxProjectTermDays bool `json:"-" swaggerignore:"true"`
	// Caps changes the budget's own caps (common.Cap) by id: a value sets one, null removes it, an id left out stays; "caps": null removes them all. The result must hold as on creation (model.ValidateCaps). Managers of the budget or above, like the other policy fields.
	Caps            map[string]*int      `json:"caps,omitempty"`
	ClearCaps       bool                 `json:"-" swaggerignore:"true"`
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
	r.ClearCaps = isNull("caps")
	r.ClearTerminationDate = isNull("termination_date")
	return nil
}

// updateClass is what an UpdateNodeRequest field edits, which decides who may send it.
type updateClass int

const (
	// updateLabel fields (name, admin scope) are the only direct edits on a project, by its owner or a manager; on a budget they are policy.
	updateLabel updateClass = iota + 1
	// updatePolicy fields need a manager of the node or above (managesNode).
	updatePolicy
	// updateCapacity fields need a manager above the node (managesParentChain): nobody raises their own limit or end.
	updateCapacity
)

// updateFieldClass classifies every field of UpdateNodeRequest by its Go name; the one list a new field has to be added to (TestUpdateNodeRequest_EveryFieldClassified fails otherwise). A field counts as sent when it is not its zero value: a pointer that is set, a Clear* flag that is true.
var updateFieldClass = map[string]updateClass{
	"Name":                           updateLabel,
	"AdminScope":                     updateLabel,
	"Reason":                         updatePolicy,
	"EligibleRequesters":             updatePolicy,
	"AutoApprove":                    updatePolicy,
	"ClearAutoApprove":               updatePolicy,
	"AllowSubBudgetRequests":         updatePolicy,
	"AllowRequestsBeyondAutoApprove": updatePolicy,
	"AutoApproveExtensions":          updatePolicy,
	"MaxProjectTermDays":             updatePolicy,
	"ClearMaxProjectTermDays":        updatePolicy,
	"Caps":                           updatePolicy,
	"ClearCaps":                      updatePolicy,
	"Limit":                          updateCapacity,
	"TerminationDate":                updateCapacity,
	"ClearTerminationDate":           updateCapacity,
	"InheritsLimit":                  updateCapacity,
}

// sentClasses reports which classes of field r sends.
func (r UpdateNodeRequest) sentClasses() map[updateClass]bool {
	out := map[updateClass]bool{}
	v := reflect.ValueOf(r)
	for i := range v.NumField() {
		if !v.Field(i).IsZero() {
			out[updateFieldClass[v.Type().Field(i).Name]] = true
		}
	}
	return out
}

// leafDirectEdit reports whether r touches nothing but the name and the admin scope — the direct edits a project leaf accepts. Neither costs anything, so neither goes through the approval cycle.
func (r UpdateNodeRequest) leafDirectEdit() bool {
	c := r.sentClasses()
	return c[updateLabel] && len(c) == 1
}

// policyEdit reports whether r edits anything that needs a manager of the node or above: a label or a policy field.
func (r UpdateNodeRequest) policyEdit() bool {
	c := r.sentClasses()
	return c[updateLabel] || c[updatePolicy]
}

// capacityEdit reports whether r edits the limit or the end, which needs a manager above the node.
func (r UpdateNodeRequest) capacityEdit() bool { return r.sentClasses()[updateCapacity] }

// createBudgetSettings are the fields of CreateNodeRequest a project refuses as budget settings, in the order they are named in the refusal. The other budget fields (eligible requesters, auto-approve and its switches) are ignored on a project, as they always were; TestCreateNodeRequest_EveryFieldClassified holds the two lists to the struct.
var createBudgetSettings = []string{"MaxProjectTermDays", "Caps", "InheritsLimit"}

// createIgnoredOnProject are the budget fields of CreateNodeRequest a project ignores.
var createIgnoredOnProject = []string{"EligibleRequesters", "AutoApprove", "AllowSubBudgetRequests", "AllowRequestsBeyondAutoApprove", "AutoApproveExtensions"}

// budgetSetting names, by its JSON name, the first budget setting (createBudgetSettings) r sends; "" when it sends none.
func (r CreateNodeRequest) budgetSetting() string {
	v := reflect.ValueOf(r)
	for _, name := range createBudgetSettings {
		if !v.FieldByName(name).IsZero() {
			f, _ := v.Type().FieldByName(name)
			return strings.Split(f.Tag.Get("json"), ",")[0]
		}
	}
	return ""
}
