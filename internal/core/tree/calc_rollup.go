package tree

import (
	"cmp"
	"context"
	"fmt"
	"github.com/pfisterer/openstack-management-api/internal/core/usage"
	"maps"
	"math"
	"slices"
	"strings"

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

// funded is a project as the accounting sees it: the project and its allocations that pay for it (in its status, or paid, for one given up), oldest first.
type funded struct {
	project model.Node
	paying  []model.Node
}

// projectsOfAllocations loads the projects the allocations pay for, with all their paying allocations — also those outside the subtree being rolled up, since the shares are of the whole project —, by project id.
func (s *Service) projectsOfAllocations(ctx context.Context, allocs []model.Node) (map[string]funded, error) {
	var ids []string
	for _, a := range allocs {
		if a.ProjectID != "" {
			ids = append(ids, a.ProjectID)
		}
	}
	out := make(map[string]funded, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	list, err := s.store.ListNodes(ctx, nodestore.NodeQuery{IDs: ids}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load projects of allocations: %w", err)
	}
	all, err := s.allocationsOf(ctx, ids...)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		statuses := nodestore.ProjectChargedStatuses(p.Status)
		var paying []model.Node
		for _, a := range all[p.ID] {
			if slices.Contains(statuses, a.Status) {
				paying = append(paying, a)
			}
		}
		slices.SortStableFunc(paying, func(a, b model.Node) int {
			if c := strings.Compare(a.CreatedAt, b.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
		out[p.ID] = funded{project: p, paying: paying}
	}
	return out, nil
}

// splitByShare splits what a project has of a resource among its paying allocations in proportion to each one's part of their limits of it (usage.Share), in whole units: the remainders go to the largest fractions, the earliest allocation first on a tie, so the parts add up exactly. Returns the part of the allocation id.
func splitByShare(f funded, rid string, total int, id string) int {
	n := len(f.paying)
	if n == 0 || total == 0 {
		return 0
	}
	// A single allocation bears all of it, exactly: no float on the way, which near the end of the int range would not convert back (an out-of-range float-to-int conversion is not defined, and amd64 wraps it to the minimum).
	if n == 1 {
		if f.paying[0].ID == id {
			return total
		}
		return 0
	}
	parts := make([]int, n)
	fracs := make([]float64, n)
	sum := 0
	for i, a := range f.paying {
		exact := float64(total) * usage.Share(f.project, f.paying, a, rid)
		switch {
		case exact >= math.MaxInt:
			parts[i] = math.MaxInt
		case exact <= math.MinInt:
			parts[i] = math.MinInt
		default:
			parts[i] = int(math.Floor(exact))
		}
		fracs[i] = exact - math.Floor(exact)
		if next, ok := addInts(sum, parts[i]); ok {
			sum = next
		} else {
			sum = total
		}
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(fracs[b], fracs[a]) })
	for k := 0; sum < total && k < n; k++ {
		parts[order[k]]++
		sum++
	}
	for i, a := range f.paying {
		if a.ID == id {
			return parts[i]
		}
	}
	return 0
}

// rollUpInUse sums what each project measurably uses (its providers' in_use) into its allocations' budgets and every tracked budget above, so a budget can show what of its handed-out capacity is really used: split among the paying allocations by their share of the project's limit, per resource (splitByShare), so nothing is counted twice. Keys no project measures stay absent rather than 0, as in Binding.InUse.
func rollUpInUse(leaves []model.Node, projects map[string]funded, parentMap map[string]*string) map[string]common.ProjectQuota {
	out := map[string]common.ProjectQuota{}
	for _, leaf := range leaves {
		if leaf.ParentID == nil {
			continue
		}
		f, ok := projects[leaf.ProjectID]
		if !ok || !slices.ContainsFunc(f.paying, func(a model.Node) bool { return a.ID == leaf.ID }) {
			continue
		}
		used := f.project.InUse()
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
				part := splitByShare(f, id, v, leaf.ID)
				total, ok := addInts(sum[id], part)
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
func buildRolledUpUsage(leaves []model.Node, projects map[string]funded, parentMap map[string]*string, resourceIDs []string, cost leafCostFunc) model.UsagePerNode {
	result := make(model.UsagePerNode)
	for _, leaf := range leaves {
		if leaf.ParentID == nil {
			continue
		}
		var f *funded
		if p, ok := projects[leaf.ProjectID]; ok {
			f = &p
		}
		// Per allocation, before any summation — see chargedQuota.
		amount := cost(leaf, f)
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
		var f *funded
		if p, ok := projects[leaf.ProjectID]; ok {
			f = &p
		}
		charged := s.leafCost(leaf, f)
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

// leafCostFunc says what one allocation costs its budget, given its project and the project's paying allocations (nil when that could not be loaded).
type leafCostFunc func(alloc model.Node, f *funded) common.ProjectQuota

// leafCost is the deployment's leafCostFunc (decided 25.11.2026): an allocation costs its limit plus its share of what the project measurably uses beyond its limit (chargedQuota); of an archived project its share of the storage the project still holds. Shares are per resource, in proportion to each paying allocation's part of the project's limit (splitByShare).
func (s *Service) leafCost(alloc model.Node, f *funded) common.ProjectQuota {
	if f == nil {
		if alloc.Status == model.StatusArchived {
			return common.ProjectQuota{}
		}
		return alloc.Limit
	}
	if alloc.Status == model.StatusArchived {
		out := common.ProjectQuota{}
		for id, v := range archivedQuota(f.project, s.storageIDs()) {
			out[id] = splitByShare(*f, id, v, alloc.ID)
		}
		return out
	}
	if !s.accounting.ChargeInUse {
		return alloc.Limit
	}
	charged := chargedQuota(f.project, s.countIDs(), true)
	out := make(common.ProjectQuota, len(alloc.Limit))
	for id, v := range alloc.Limit {
		out[id] = v
	}
	for _, id := range s.countIDs() {
		if over := charged[id] - f.project.Limit[id]; over > 0 {
			out[id] += splitByShare(*f, id, over, alloc.ID)
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
// What runs over the limit is split among its allocations (leafCost).
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
