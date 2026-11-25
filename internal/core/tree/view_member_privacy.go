package tree

import (
	"maps"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// Member privacy: a project's members (authorized users) may read it, but who else works in it is none of their business. Everyone who answers for the project sees it in full — its owner, its admins, the managers of a budget paying for it (or asked to, through any allocation) and the root admins. A member sees the project through redactForMember: their own entries of the member list and how many others there are, the owner as the contact, the allocations with budget, limit and end; no admins, no history, no creators, no group assignments from OpenStack.
//
// It is applied in attachParentNames, which every response that carries a project or an allocation passes through (reads, listings, the result of a change, the MCP tools alike), so no endpoint can forget it.

// seesProjectInFull reports whether the viewer sees n — a project, or an allocation with its project — in full rather than as a member (see redactForMember). The chains of the paying budgets must be in a (attachParentNames walks them).
func (a ancestry) seesProjectInFull(n model.Node, viewer common.TokenSet, rootAdmins common.TokenList) bool {
	if viewer.ContainsAny(rootAdmins) {
		return true
	}
	switch n.Kind {
	case model.KindProject:
		if (n.Owner != "" && viewer.Contains(n.Owner)) || viewer.ContainsAny(n.AdminScope) {
			return true
		}
		for _, b := range a.projectBudgets(n) {
			if managesInChain(viewer, a.chainFrom(b)) {
				return true
			}
		}
		return false
	case model.KindAllocation:
		if (n.Owner != "" && viewer.Contains(n.Owner)) || managesInChain(viewer, a.budgetsAbove(n)) {
			return true
		}
		return n.Project != nil && a.seesProjectInFull(*n.Project, viewer, rootAdmins)
	}
	return true
}

// projectBudgets are the budgets whose managers see a project in full: of every allocation it has (paying or not), the ones it is funded from, and for an import nothing pays for yet the unassigned node, whose managers adopt it.
func (a ancestry) projectBudgets(n model.Node) []string {
	out := slices.Clone(n.FundingBudgetIDs)
	for _, al := range n.Allocations {
		out = append(out, al.BudgetID)
	}
	if len(out) == 0 && n.Status == model.StatusImported {
		out = append(out, model.UnassignedNodeID)
	}
	return out
}

// redactForMember turns n — a project, or an allocation with its project — into what a member of the project sees: of the authorized users only the viewer's own entries (the person, or a group the viewer is in, as the group's name) and MembersHidden counting the others; no admin scope, no history (the revision stays), no creator, no OpenStack group assignments or sync errors; of each allocation its budget, limit, end and status, without who asked for it, why, and the attributes of the paying budget. The owner stays as the contact. ViewerRelation becomes "member". Works on copies: nothing n shares with the store is changed.
func redactForMember(n *model.Node, viewer common.TokenSet) {
	n.AuthorizedUsers, n.MembersHidden = ownEntries(n.AuthorizedUsers, viewer)
	n.AdminScope = nil
	n.HistoryDropped = n.CurrentRevision()
	n.History = nil
	n.CreatedBy = ""
	n.Attributes = nil
	n.EffectiveAttributes = nil
	if n.Pending != nil {
		p := *n.Pending
		if p.AuthorizedUsers != nil {
			own, _ := ownEntries(*p.AuthorizedUsers, viewer)
			p.AuthorizedUsers = &own
		}
		n.Pending = &p
	}
	if len(n.Bindings) > 0 {
		bindings := maps.Clone(n.Bindings)
		for k, b := range bindings {
			b.ExternalGroupAssignments = nil
			b.SyncErrors = nil
			bindings[k] = b
		}
		n.Bindings = bindings
	}
	if len(n.Allocations) > 0 {
		allocs := slices.Clone(n.Allocations)
		for i := range allocs {
			allocs[i].CreatedBy = ""
			allocs[i].Reason = ""
			allocs[i].Attributes = nil
			if allocs[i].Pending != nil && allocs[i].Pending.AuthorizedUsers != nil {
				p := *allocs[i].Pending
				own, _ := ownEntries(*p.AuthorizedUsers, viewer)
				p.AuthorizedUsers = &own
				allocs[i].Pending = &p
			}
		}
		n.Allocations = allocs
	}
	if n.Project != nil {
		p := *n.Project
		redactForMember(&p, viewer)
		n.Project = &p
	}
	if n.Kind == model.KindAllocation {
		n.Reason = ""
	}
	n.ViewerRelation = "member"
}

// ownEntries splits a member list into the viewer's own entries and the number of the others.
func ownEntries(users []common.AuthorizedUser, viewer common.TokenSet) ([]common.AuthorizedUser, int) {
	var own []common.AuthorizedUser
	for _, u := range users {
		if viewer.Contains(u.Token) {
			own = append(own, u)
		}
	}
	return own, len(users) - len(own)
}
