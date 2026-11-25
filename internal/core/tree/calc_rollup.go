package tree

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// buildSubtreeParentMap performs a BFS over BUDGET nodes from the given roots,
// building a map of every subtree budget ID → its parent ID (roots included).
func (s *Service) buildSubtreeParentMap(ctx context.Context, roots []model.Node) (map[string]*string, error) {
	parentMap := make(map[string]*string, len(roots))
	queue := make([]string, 0, len(roots))
	for _, n := range roots {
		if _, seen := parentMap[n.ID]; seen {
			continue
		}
		parentMap[n.ID] = n.ParentID
		queue = append(queue, n.ID)
	}

	for len(queue) > 0 {
		children, err := s.store.ListNodes(ctx, nodestore.NodeQuery{ParentIDs: queue, Kinds: []string{model.KindBudget}}, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("load child budgets: %w", err)
		}
		queue = queue[:0]
		for _, child := range children {
			if _, seen := parentMap[child.ID]; seen {
				continue
			}
			parentMap[child.ID] = child.ParentID
			queue = append(queue, child.ID)
		}
	}
	return parentMap, nil
}

// loadSubtreeUsage computes resource consumption for each budget in roots, rolling
// up what the charged ALLOCATIONS below cost through the budget hierarchy.
//
// Three phases: (1) BFS discovers the budget subtree, (2) one batched load fetches
// all charged allocations across the subtree and the projects they pay for, (3) each
// allocation's cost is attributed upward from its budget through every tracked ancestor.
func (s *Service) loadSubtreeUsage(ctx context.Context, roots []model.Node) (model.UsagePerNode, error) {
	usage, _, err := s.loadSubtreeUsageAndInUse(ctx, roots)
	return usage, err
}

// loadSubtreeUsageAndInUse is loadSubtreeUsage plus, per budget, what the projects below measurably use (see Node.SubtreeInUse) — from the same allocations, so the two cannot disagree.
func (s *Service) loadSubtreeUsageAndInUse(ctx context.Context, roots []model.Node) (model.UsagePerNode, map[string]common.ProjectQuota, error) {
	if len(roots) == 0 {
		return make(model.UsagePerNode), nil, nil
	}
	parentMap, err := s.buildSubtreeParentMap(ctx, roots)
	if err != nil {
		return nil, nil, err
	}
	budgetIDs := make([]string, 0, len(parentMap))
	for id := range parentMap {
		budgetIDs = append(budgetIDs, id)
	}
	leaves, err := s.store.ListNodes(ctx, nodestore.NodeQuery{
		ParentIDs: budgetIDs,
		Kinds:     []string{model.KindAllocation},
		Statuses:  s.chargedStatuses(),
	}, 0, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("load charged allocations for usage rollup: %w", err)
	}
	projects, err := s.projectsOfAllocations(ctx, leaves)
	if err != nil {
		return nil, nil, err
	}
	return buildRolledUpUsage(leaves, projects, parentMap, s.countIDs(), s.leafCost), rollUpInUse(leaves, projects, parentMap), nil
}

// projectsOfAllocations loads the projects the allocations pay for, by id.
func (s *Service) projectsOfAllocations(ctx context.Context, allocs []model.Node) (map[string]model.Node, error) {
	var ids []string
	for _, a := range allocs {
		if a.ProjectID != "" {
			ids = append(ids, a.ProjectID)
		}
	}
	out := make(map[string]model.Node, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	list, err := s.store.ListNodes(ctx, nodestore.NodeQuery{IDs: ids}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load projects of allocations: %w", err)
	}
	for _, p := range list {
		out[p.ID] = p
	}
	return out, nil
}

// isHome reports whether alloc is its project's home allocation (Node.HomeAllocationID): the one charged with what the project measurably uses beyond its limit, and with the storage an archived project still holds.
func isHome(alloc model.Node, project *model.Node) bool {
	return project != nil && project.HomeAllocationID == alloc.ID
}

// rollUpInUse sums what each project measurably uses (its providers' in_use) into the budget of its home allocation and every tracked budget above, so a budget can show what of its handed-out capacity is really used. A project's whole consumption goes up the chain of its home allocation only — in_use is not split by where its limit came from, and counting it under every allocation would count it several times. Keys no project measures stay absent rather than 0, as in Binding.InUse.
func rollUpInUse(leaves []model.Node, projects map[string]model.Node, parentMap map[string]*string) map[string]common.ProjectQuota {
	out := map[string]common.ProjectQuota{}
	for _, leaf := range leaves {
		if leaf.ParentID == nil {
			continue
		}
		project, ok := projects[leaf.ProjectID]
		if !ok || !isHome(leaf, &project) {
			continue
		}
		used := project.InUse()
		if len(used) == 0 {
			continue
		}
		seen := map[string]bool{}
		for current := *leaf.ParentID; !seen[current]; {
			if _, tracked := parentMap[current]; !tracked {
				break
			}
			seen[current] = true
			sum := out[current]
			if sum == nil {
				sum = common.ProjectQuota{}
			}
			for id, v := range used {
				total, ok := addInts(sum[id], v)
				if !ok {
					total = saturate(sum[id])
				}
				sum[id] = total
			}
			out[current] = sum
			parent := parentMap[current]
			if parent == nil {
				break
			}
			current = *parent
		}
	}
	return out
}

// buildRolledUpUsage attributes each allocation's cost and ID to its budget and
// every tracked ancestor, so a budget's usage reflects total consumption across
// its entire subtree — not just its direct children. Every allocation is charged
// to its own budget and those above it; a budget beside it never carries it.
func buildRolledUpUsage(leaves []model.Node, projects map[string]model.Node, parentMap map[string]*string, resourceIDs []string, cost leafCostFunc) model.UsagePerNode {
	result := make(model.UsagePerNode)
	for _, leaf := range leaves {
		if leaf.ParentID == nil {
			continue
		}
		var project *model.Node
		if p, ok := projects[leaf.ProjectID]; ok {
			project = &p
		}
		// Per allocation, before any summation — see chargedQuota.
		amount := cost(leaf, project)
		current := *leaf.ParentID
		for {
			if _, tracked := parentMap[current]; !tracked {
				break // outside the tracked subtree — stop climbing
			}
			if result[current] == nil {
				result[current] = make(model.UsageByStatus)
			}
			entry := result[current][leaf.Status]
			entry.Limit = quotaAdd(entry.Limit, amount, resourceIDs)
			entry.NodeIDs = append(entry.NodeIDs, leaf.ID)
			result[current][leaf.Status] = entry

			parent := parentMap[current]
			if parent == nil {
				break
			}
			current = *parent
		}
	}
	return result
}

// ownerActiveUsage sums what the owner's charged allocations directly under the
// given budget cost, so auto-approval enforces a cumulative per-requester cap —
// per budget: what a project draws from another budget does not count here.
// Matching is by the single Owner token — group memberships do not blur the count.
func (s *Service) ownerActiveUsage(ctx context.Context, budgetID string, ownerToken string) (common.ProjectQuota, error) {
	leaves, err := s.store.ListNodes(ctx, nodestore.NodeQuery{
		ParentIDs: []string{budgetID},
		Kinds:     []string{model.KindAllocation},
		Statuses:  s.chargedStatuses(),
		Owner:     ownerToken,
	}, 0, 0)
	if err != nil {
		return nil, err
	}
	projects, err := s.projectsOfAllocations(ctx, leaves)
	if err != nil {
		return nil, err
	}
	usage := make(common.ProjectQuota, len(s.countIDs()))
	for _, leaf := range leaves {
		var project *model.Node
		if p, ok := projects[leaf.ProjectID]; ok {
			project = &p
		}
		charged := s.leafCost(leaf, project)
		for _, id := range s.countIDs() {
			usage[id] += charged[id]
		}
	}
	return usage, nil
}

// chargedStatuses are the allocation states whose limits count against a budget. The
// single answer for every place that asks — the rollup, the per-requester
// auto-approve cap and the reparent capacity check must agree, or an allocation gets
// billed in one view and moves for free in another. What an allocation in one of them
// costs is leafCost's answer.
func (s *Service) chargedStatuses() []string {
	statuses := slices.Clone(model.ActiveStatuses)
	if s.accounting.ChargeReleased {
		statuses = append(statuses, model.StatusReleased)
	}
	if s.accounting.ChargeArchived {
		statuses = append(statuses, model.StatusArchived)
	}
	return statuses
}

// leafCostFunc says what one allocation costs its budget, given the project it pays for (nil when that could not be loaded).
type leafCostFunc func(alloc model.Node, project *model.Node) common.ProjectQuota

// leafCost is the deployment's leafCostFunc. An allocation costs its limit. The home allocation of its project (isHome) also carries what the project measurably uses beyond everything it holds (chargedQuota); of an archived project it costs only the storage the project still holds, and the others nothing.
func (s *Service) leafCost(alloc model.Node, project *model.Node) common.ProjectQuota {
	home := isHome(alloc, project)
	if alloc.Status == model.StatusArchived {
		if !home {
			return common.ProjectQuota{}
		}
		return archivedQuota(*project, s.storageIDs())
	}
	if !home || !s.accounting.ChargeInUse {
		return alloc.Limit
	}
	excess := chargedQuota(*project, s.countIDs(), true)
	out := make(common.ProjectQuota, len(alloc.Limit))
	for id, v := range alloc.Limit {
		out[id] = v
	}
	for _, id := range s.countIDs() {
		if over := excess[id] - project.Limit[id]; over > 0 {
			out[id] += over
		}
	}
	return out
}

// archivedQuota is the storage an archived project still holds: what OpenStack
// measured, or its own limit where nothing was measured. The rest — servers
// shelved, floating IPs released, quotas frozen — holds nothing any more.
func archivedQuota(leaf model.Node, storageIDs []string) common.ProjectQuota {
	out := common.ProjectQuota{}
	used := leaf.InUse()
	for _, id := range storageIDs {
		if inUse, measured := used[id]; measured {
			out[id] = inUse
		} else if v := leaf.Limit[id]; v > 0 {
			out[id] = v
		}
	}
	return out
}

// chargedQuota is what a project COSTS in total: its limit (the sum of its
// allocations), or the measured OpenStack usage where that is higher. With
// chargeInUse off it is the limit alone, which is the pre-2026-08 behaviour.
// What runs over the limit is charged to the home allocation (leafCost).
//
// Why the max: shrinking a limit after filling the project frees budget on
// paper while the servers keep running. OpenStack accepts the smaller quota,
// existing servers are untouched, only NEW ones are refused — so 8 cores
// requested, filled, then shrunk to 1 books 1 and leaves 8 in use. Repeat per
// project and the platform's books say "almost nothing" while the cloud is
// full. Billing the larger of the two numbers removes the gain regardless of
// which route (self-service or an approving manager) granted the shrink.
//
// Two rules that are easy to get wrong:
//   - A resource MISSING from InUse means "OpenStack does not measure this",
//     NOT "zero in use". Treating an absent key as 0 would be exactly the
//     understatement this function exists to prevent, so it leaves the limit
//     untouched instead.
//   - An unlimited limit (common.UnlimitedQuota, -1) stays unlimited. A naive
//     max(-1, n) would turn "no cap" into a finite number and quietly tighten
//     a budget nobody asked to tighten.
//
// The result must be computed PER PROJECT and only then summed: max() does not
// distribute over addition, so applying it to an already-aggregated total
// gives a different (and wrong) answer.
func chargedQuota(leaf model.Node, resourceIDs []string, chargeInUse bool) common.ProjectQuota {
	if !chargeInUse {
		return leaf.Limit
	}
	total := leaf.EffectiveLimit()
	used := leaf.InUse()
	out := make(common.ProjectQuota, len(resourceIDs))
	for _, id := range resourceIDs {
		limit := leaf.Limit[id]
		out[id] = limit
		if total[id] == common.UnlimitedQuota {
			continue
		}
		inUse, measured := used[id]
		if measured && inUse > total[id] {
			out[id] = limit + inUse - total[id]
		}
	}
	return out
}

// quotaAdd sums configured resource types from two quota objects, for what is shown and rolled up. A sum that would overflow saturates at the largest int instead of wrapping negative: a wrapped total reads as room in a budget that has none. Decisions use quotaAddChecked, which refuses instead.
func quotaAdd(a, b common.ProjectQuota, resourceIDs []string) common.ProjectQuota {
	out := make(common.ProjectQuota, len(a)+len(resourceIDs))
	maps.Copy(out, a)
	for _, resourceID := range resourceIDs {
		sum, ok := addInts(a[resourceID], b[resourceID])
		if !ok {
			sum = saturate(a[resourceID])
		}
		out[resourceID] = sum
	}
	return out
}

// quotaAddChecked is quotaAdd for a decision: an overflow is an error, so a capacity or per-requester check can never be passed by a sum that wrapped around.
func quotaAddChecked(a, b common.ProjectQuota, resourceIDs []string) (common.ProjectQuota, error) {
	out := make(common.ProjectQuota, len(a)+len(resourceIDs))
	maps.Copy(out, a)
	for _, resourceID := range resourceIDs {
		sum, ok := addInts(a[resourceID], b[resourceID])
		if !ok {
			return nil, invalid("%q: %d + %d liegt außerhalb des erlaubten Bereichs.", "%q: %d + %d is outside the allowed range.", resourceID, a[resourceID], b[resourceID])
		}
		out[resourceID] = sum
	}
	return out, nil
}

// addInts adds two ints and reports false where the sum overflows.
func addInts(a, b int) (int, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// saturate is the end of the int range an overflowing sum ran past, by the sign of a summand.
func saturate(a int) int {
	if a < 0 {
		return math.MinInt
	}
	return math.MaxInt
}

// shortfall is the first resource of resourceIDs where need exceeds limit;
// false when everything fits. A limit of common.UnlimitedQuota (-1) means no
// cap for that resource. The one test of "fits under a limit": capacity,
// per-requester share and a budget's limit against what is in use below it.
func shortfall(need, limit common.ProjectQuota, resourceIDs []string) (string, bool) {
	for _, resourceID := range resourceIDs {
		if limit[resourceID] != common.UnlimitedQuota && need[resourceID] > limit[resourceID] {
			return resourceID, true
		}
	}
	return "", false
}

// deduct subtracts sub from q in place for resourceIDs: what a change takes back before it adds its new size (a change_pending project's current limit, an owner's project being resized), so it is not counted twice.
func deduct(q, sub common.ProjectQuota, resourceIDs []string) {
	for _, id := range resourceIDs {
		q[id] -= sub[id]
	}
}
