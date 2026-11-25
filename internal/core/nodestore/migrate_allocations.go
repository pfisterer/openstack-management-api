package nodestore

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// allocationNamespace makes the ids of migrated allocations a function of project and budget, so a dry run and the real run name them alike and a run that is repeated after a failure writes the same nodes.
var allocationNamespace = uuid.MustParse("0b6f5f3e-6d0c-4c8e-9a51-5a8c2d1f7e42")

// MigratedAllocationID is the id the migration gives the allocation of project under budget.
func MigratedAllocationID(projectID, budgetID string) string {
	return "a_" + uuid.NewSHA1(allocationNamespace, []byte(projectID+"/"+budgetID)).String()
}

// AllocationMigrationReport says what the migration to allocation leaves did or, in a dry run, would do.
type AllocationMigrationReport struct {
	Projects            int      `json:"projects"`
	ProjectsAfter       int      `json:"projects_after"`
	Allocations         int      `json:"allocations"`
	ExtraAllocations    int      `json:"extra_allocations"`
	PendingChanges      int      `json:"pending_changes"`
	Imported            int      `json:"imported"`
	DroppedMemberChange int      `json:"dropped_member_changes"`
	Revised             int      `json:"revised,omitempty"`
	ExpiryMarksRemoved  int      `json:"expiry_marks_removed,omitempty"`
	Mismatches          []string `json:"mismatches,omitempty"`
}

// MigrateToAllocations turns every project leaf of the former model (a project hanging under its budget, with its own limit and optional extra allocations from budgets above) into a project object with the same id and one allocation leaf per paying budget. It returns the nodes to write and a report; nodes that need no change are not returned. A store already migrated returns nothing to write.
//
// The mapping (decided 25.11.2026): the project keeps id, owner, members, admins, status, history, bindings, attributes and flags, and leaves the tree (ParentID nil); its own limit becomes an allocation under its former parent with that limit and the project's end, its status and a waiting change of limit or end; every extra allocation becomes an allocation under its budget. A waiting change of members alone has no place any more (members change without approval) and is dropped, counted in the report. Imported projects leave the unassigned node without an allocation. The project's Limit stays what OpenStack has been given, the sum of its allocations.
func MigrateToAllocations(nodes []LegacyNode, now time.Time) ([]model.Node, AllocationMigrationReport, error) {
	var rep AllocationMigrationReport
	var out []model.Node
	stamp := now.UTC().Format(time.RFC3339)
	for _, l := range nodes {
		p := l.Node
		if p.Kind != model.KindProject {
			continue
		}
		rep.Projects++
		if p.ParentID == nil {
			continue // migrated already
		}
		parent := *p.ParentID
		project := p
		project.ParentID = nil
		project.Allocations = nil
		if p.Status == model.StatusImported {
			rep.Imported++
			out = append(out, project)
			continue
		}
		old := SumAllocations([]model.Node{{Limit: p.Limit}}, nil)
		for _, a := range l.Extras {
			old = SumAllocations([]model.Node{{Limit: old}, {Limit: a.Limit}}, nil)
		}

		mainStatus := p.Status
		var pending *model.PendingChanges
		if p.Pending != nil {
			if p.Pending.Limit != nil || p.Pending.TerminationDate != nil {
				pending = &model.PendingChanges{Limit: p.Pending.Limit, TerminationDate: p.Pending.TerminationDate}
				rep.PendingChanges++
			} else {
				mainStatus = model.StatusApproved
			}
			if p.Pending.AuthorizedUsers != nil {
				rep.DroppedMemberChange++
			}
		}
		if mainStatus == model.StatusChangePending && pending == nil {
			mainStatus = model.StatusApproved
		}
		entry := model.HistoryEntry{Timestamp: stamp, Event: "migrated", Actor: "system:migration", Via: model.ChannelSystem, StatusTo: mainStatus, Reason: new("Aus dem eigenen Anteil des Projekts (Umstellung auf Zuteilungen)")}
		main := model.Node{
			ID:              MigratedAllocationID(p.ID, parent),
			Kind:            model.KindAllocation,
			ParentID:        &parent,
			ProjectID:       p.ID,
			Status:          mainStatus,
			Name:            p.Name,
			Reason:          p.Reason,
			Owner:           p.Owner,
			Limit:           maps.Clone(p.Limit),
			TerminationDate: p.TerminationDate,
			Pending:         pending,
			CreatedBy:       p.CreatedBy,
			CreatedAt:       p.CreatedAt,
			History:         []model.HistoryEntry{entry},
		}
		if main.Limit == nil {
			main.Limit = common.ProjectQuota{}
		}
		allocs := []model.Node{main}
		for _, a := range l.Extras {
			status := p.Status
			if status == model.StatusChangePending {
				status = model.StatusApproved
			}
			e := entry
			e.StatusTo = status
			e.Reason = new("Aus einer Sonderzuteilung (Umstellung auf Zuteilungen)")
			allocs = append(allocs, model.Node{
				ID:              MigratedAllocationID(p.ID, a.BudgetID),
				Kind:            model.KindAllocation,
				ParentID:        new(a.BudgetID),
				ProjectID:       p.ID,
				Status:          status,
				Name:            p.Name,
				Reason:          a.Reason,
				Owner:           p.Owner,
				Limit:           maps.Clone(a.Limit),
				TerminationDate: p.TerminationDate,
				CreatedBy:       a.GrantedBy,
				CreatedAt:       a.GrantedAt,
				History:         []model.HistoryEntry{e},
			})
			rep.ExtraAllocations++
		}
		rep.Allocations += len(allocs)

		if project.Status == model.StatusChangePending {
			project.Status = model.StatusApproved
		}
		project.Pending = nil
		// Attributes (cost centre and the like) belong to the allocations now, set by their budget's managers: the project's own go to each of them.
		if len(project.Attributes) > 0 {
			for i := range allocs {
				allocs[i].Attributes = maps.Clone(project.Attributes)
			}
			project.Attributes = nil
		}
		project.FundingBudgetIDs = FundingBudgets(allocs)
		project.StartWhen = model.StartNow
		project.Limit = SumAllocations(allocs, ProjectChargedStatuses(project.Status))
		if model.IsRetiredStatus(project.Status) || project.Status == model.StatusApproved {
			if !QuotaEqual(project.Limit, old) {
				rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("%s: limit %v, before %v", p.ID, project.Limit, old))
			}
		}
		if all := SumAllocations(allocs, nil); !QuotaEqual(all, old) {
			rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("%s: allocations %v, before %v", p.ID, all, old))
		}
		out = append(out, project)
		out = append(out, allocs...)
	}
	rep.ProjectsAfter = rep.Projects
	sort.Strings(rep.Mismatches)
	return out, rep, nil
}

// LegacyNode is a stored node as the migration reads it: the node, and on a project of the former model its extra allocations from budgets above (stored under "allocations" in that shape).
type LegacyNode struct {
	Node   model.Node
	Extras []LegacyAllocation
}

// LegacyAllocation is an extra allocation of the former model (Node.Allocations before 25.11.2026).
type LegacyAllocation struct {
	BudgetID  string              `json:"budget_id"`
	Limit     common.ProjectQuota `json:"limit"`
	Reason    string              `json:"reason"`
	GrantedBy string              `json:"granted_by"`
	GrantedAt string              `json:"granted_at"`
}

// ProjectEnd is the end of a project with these allocations (decided 25.11.2026): the earliest of their ends — every allocation runs until the project ends —, leaving out those the project was extended without (Node.EndsAlone) unless that is all of them, and those without an end; none when none has one.
func ProjectEnd(allocs []model.Node) *string {
	considered := slices.DeleteFunc(slices.Clone(allocs), func(a model.Node) bool { return a.EndsAlone })
	if len(considered) == 0 {
		considered = allocs
	}
	var end *string
	for _, a := range considered {
		if a.TerminationDate == nil {
			continue
		}
		if end == nil || model.EndsNoLater(*a.TerminationDate, end) {
			end = a.TerminationDate
		}
	}
	return end
}

// FundingBudgets are the budgets of the allocations, oldest allocation first, each once.
func FundingBudgets(allocs []model.Node) []string {
	sorted := slices.Clone(allocs)
	slices.SortStableFunc(sorted, func(a, b model.Node) int {
		if c := strings.Compare(a.CreatedAt, b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	var out []string
	for _, a := range sorted {
		if a.ParentID != nil && !slices.Contains(out, *a.ParentID) {
			out = append(out, *a.ParentID)
		}
	}
	return out
}

// ReviseAllocationLeaves brings data migrated by the first form of the model (25.11.2026, morning) to the revised one (same day): no home allocation, attributes on the allocations, no expiry marks, and the project's end the earliest of its paying allocations. It returns the nodes to write. Nothing ends or shrinks: an allocation that was marked as expired just loses the mark and keeps paying; the project's limit is not touched.
func ReviseAllocationLeaves(nodes []model.Node) ([]model.Node, error) {
	allocs := map[string][]model.Node{}
	for _, n := range nodes {
		if n.Kind == model.KindAllocation {
			allocs[n.ProjectID] = append(allocs[n.ProjectID], n)
		}
	}
	var out []model.Node
	for _, p := range nodes {
		if p.Kind != model.KindProject || p.ParentID != nil {
			continue
		}
		own := allocs[p.ID]
		if len(p.Attributes) > 0 {
			for i := range own {
				if len(own[i].Attributes) == 0 {
					own[i].Attributes = maps.Clone(p.Attributes)
				}
			}
			p.Attributes = nil
		}
		statuses := ProjectChargedStatuses(p.Status)
		var counted, waiting []model.Node
		for _, a := range own {
			switch {
			case slices.Contains(statuses, a.Status):
				counted = append(counted, a)
			case a.Status == model.StatusPending:
				waiting = append(waiting, a)
			}
		}
		set := counted
		if len(set) == 0 {
			set = waiting
		}
		if len(set) > 0 {
			p.FundingBudgetIDs = FundingBudgets(set)
			p.TerminationDate = ProjectEnd(set)
		}
		out = append(out, p)
		out = append(out, own...)
	}
	return out, nil
}

// ProjectChargedStatuses are the allocation statuses that make up a project's Limit in the given project status: what pays for a running project, or what paid for it when it was given up. Nil (every status) is never returned.
func ProjectChargedStatuses(projectStatus string) []string {
	if model.IsRetiredStatus(projectStatus) {
		return []string{model.StatusReleased, model.StatusArchived}
	}
	return []string{model.StatusApproved, model.StatusChangePending}
}

// SumAllocations adds the limits of the allocations in one of statuses (all of them for nil): quantities add up, an availability is granted when one of them grants it, an unlimited amount stays unlimited.
func SumAllocations(allocs []model.Node, statuses []string) common.ProjectQuota {
	out := common.ProjectQuota{}
	for _, a := range allocs {
		if statuses != nil && !slices.Contains(statuses, a.Status) {
			continue
		}
		for id, v := range a.Limit {
			switch {
			case out[id] == common.UnlimitedQuota || v == common.UnlimitedQuota:
				out[id] = common.UnlimitedQuota
			default:
				out[id] += v
			}
		}
	}
	return out
}

// QuotaEqual compares two limits, a missing key counting as 0.
func QuotaEqual(a, b common.ProjectQuota) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// MigrateNodes is MigrateToAllocations on a whole set of nodes written as the former model (seed and test data): a project in the tree, its extra allocations in Allocations. It returns the set with every project replaced by its migrated form and its allocations added.
func MigrateNodes(nodes []model.Node, now time.Time) ([]model.Node, error) {
	legacy := make([]LegacyNode, 0, len(nodes))
	for _, n := range nodes {
		l := LegacyNode{Node: n}
		if n.Kind == model.KindProject && n.ParentID != nil {
			for _, a := range n.Allocations {
				l.Extras = append(l.Extras, LegacyAllocation{BudgetID: a.BudgetID, Limit: a.Limit, Reason: a.Reason, GrantedBy: a.CreatedBy, GrantedAt: a.CreatedAt})
			}
		}
		legacy = append(legacy, l)
	}
	writes, rep, err := MigrateToAllocations(legacy, now)
	if err != nil {
		return nil, err
	}
	if len(rep.Mismatches) > 0 {
		return nil, fmt.Errorf("migrate: %s", rep.Mismatches[0])
	}
	replaced := map[string]model.Node{}
	var added []model.Node
	for _, w := range writes {
		if w.Kind == model.KindProject {
			replaced[w.ID] = w
		} else {
			added = append(added, w)
		}
	}
	out := make([]model.Node, 0, len(nodes)+len(added))
	for _, n := range nodes {
		if r, ok := replaced[n.ID]; ok {
			n = r
		}
		out = append(out, n)
	}
	return append(out, added...), nil
}
