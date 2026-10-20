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
// up the limits of all ACTIVE descendant leaves through the budget hierarchy.
//
// Three phases: (1) BFS discovers the budget subtree, (2) one batched load fetches
// all active leaves across the subtree, (3) each leaf's limit is attributed upward
// from its parent through every tracked ancestor.
func (s *Service) loadSubtreeUsage(ctx context.Context, roots []model.Node) (model.UsagePerNode, error) {
	if len(roots) == 0 {
		return make(model.UsagePerNode), nil
	}

	parentMap, err := s.buildSubtreeParentMap(ctx, roots)
	if err != nil {
		return nil, err
	}

	budgetIDs := make([]string, 0, len(parentMap))
	for id := range parentMap {
		budgetIDs = append(budgetIDs, id)
	}
	leaves, err := s.store.ListNodes(ctx, nodestore.NodeQuery{
		ParentIDs: budgetIDs,
		Kinds:     []string{model.KindProject},
		Statuses:  s.chargedStatuses(),
	}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load active leaves for usage rollup: %w", err)
	}

	return buildRolledUpUsage(leaves, parentMap, s.countIDs(), s.leafCost), nil
}

// loadSubtreeUsageAndAllocations is loadSubtreeUsage plus, per budget, what it
// has allocated to projects further down (see Node.AllocatedOut) and what the
// projects below measurably use (see Node.SubtreeInUse) — from the same
// leaves, so the three cannot disagree.
func (s *Service) loadSubtreeUsageAndAllocations(ctx context.Context, roots []model.Node) (model.UsagePerNode, map[string]model.AllocatedOut, map[string]common.ProjectQuota, error) {
	if len(roots) == 0 {
		return make(model.UsagePerNode), nil, nil, nil
	}
	parentMap, err := s.buildSubtreeParentMap(ctx, roots)
	if err != nil {
		return nil, nil, nil, err
	}
	budgetIDs := make([]string, 0, len(parentMap))
	for id := range parentMap {
		budgetIDs = append(budgetIDs, id)
	}
	leaves, err := s.store.ListNodes(ctx, nodestore.NodeQuery{
		ParentIDs: budgetIDs,
		Kinds:     []string{model.KindProject},
		Statuses:  s.chargedStatuses(),
	}, 0, 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load active leaves for usage rollup: %w", err)
	}
	wanted := make(map[string]bool, len(roots))
	for _, r := range roots {
		wanted[r.ID] = true
	}
	allocated := map[string]model.AllocatedOut{}
	for _, leaf := range leaves {
		if _, held := s.leafCost(leaf); !held {
			continue
		}
		for _, a := range leaf.Allocations {
			if !wanted[a.BudgetID] {
				continue
			}
			out := allocated[a.BudgetID]
			out.Projects++
			out.Limit = quotaAdd(out.Limit, a.Limit, s.countIDs())
			// Availabilities never add up as capacity, but how many projects
			// have one from here is exactly what this summary is for.
			for rid, v := range a.Limit {
				if s.isBool(rid) {
					out.Limit[rid] += v
				}
			}
			allocated[a.BudgetID] = out
		}
	}
	return buildRolledUpUsage(leaves, parentMap, s.countIDs(), s.leafCost), allocated, rollUpInUse(leaves, parentMap), nil
}

// rollUpInUse sums what each leaf measurably uses (its providers' in_use) into its parent budget and every tracked budget above, so a budget can show what of its handed-out capacity is really used. A leaf's whole consumption goes up its own chain: in_use is not split by where its limit came from, and every budget on that chain has the project in its subtree. Keys no leaf measures stay absent rather than 0, as in Binding.InUse.
func rollUpInUse(leaves []model.Node, parentMap map[string]*string) map[string]common.ProjectQuota {
	out := map[string]common.ProjectQuota{}
	for _, leaf := range leaves {
		if leaf.ParentID == nil {
			continue
		}
		used := leaf.InUse()
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

// buildRolledUpUsage attributes each leaf's limit and ID to its parent budget and
// every tracked ancestor, so a budget's usage reflects total consumption across
// its entire subtree — not just its direct children. A leaf's allocations from
// budgets further up are attributed from THEIR budget upwards: the budgets in
// between never held them, so they never carry them either.
func buildRolledUpUsage(leaves []model.Node, parentMap map[string]*string, resourceIDs []string, cost leafCostFunc) model.UsagePerNode {
	result := make(model.UsagePerNode)
	charge := func(leaf model.Node, from string, amount common.ProjectQuota) {
		current := from
		for {
			if _, tracked := parentMap[current]; !tracked {
				return // outside the tracked subtree — stop climbing
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
				return
			}
			current = *parent
		}
	}

	for _, leaf := range leaves {
		if leaf.ParentID == nil {
			continue
		}
		// Per leaf, before any summation — see chargedQuota.
		own, withAllocations := cost(leaf)
		charge(leaf, *leaf.ParentID, own)
		if !withAllocations {
			continue
		}
		for _, a := range leaf.Allocations {
			charge(leaf, a.BudgetID, a.Limit)
		}
	}
	return result
}

// ownerActiveUsage sums the owner's committed (active) leaf limits directly under
// the given budget, so auto-approval enforces a cumulative per-requester cap.
// Matching is by the single Owner token — group memberships do not blur the count.
func (s *Service) ownerActiveUsage(ctx context.Context, budgetID string, ownerToken string) (common.ProjectQuota, error) {
	leaves, err := s.store.ListNodes(ctx, nodestore.NodeQuery{
		ParentIDs: []string{budgetID},
		Kinds:     []string{model.KindProject},
		Statuses:  s.chargedStatuses(),
		Owner:     ownerToken,
	}, 0, 0)
	if err != nil {
		return nil, err
	}
	usage := make(common.ProjectQuota, len(s.countIDs()))
	for _, leaf := range leaves {
		charged, _ := s.leafCost(leaf)
		for _, id := range s.countIDs() {
			usage[id] += charged[id]
		}
	}
	return usage, nil
}

// chargedStatuses are the leaf states whose limits count against a budget. The
// single answer for every place that asks — the rollup, the per-requester
// auto-approve cap and the reparent capacity check must agree, or a leaf gets
// billed in one view and moves for free in another. What a leaf in one of them
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

// leafCostFunc says what one leaf costs its own budget, and whether its
// allocations are charged to theirs as well.
type leafCostFunc func(leaf model.Node) (own common.ProjectQuota, withAllocations bool)

// leafCost is the deployment's leafCostFunc: an archived leaf costs the storage
// it still holds, everything else costs its chargedQuota plus its allocations.
func (s *Service) leafCost(leaf model.Node) (common.ProjectQuota, bool) {
	if leaf.Status == model.StatusArchived {
		return archivedQuota(leaf, s.storageIDs()), false
	}
	return chargedQuota(leaf, s.countIDs(), s.accounting.ChargeInUse), true
}

// archivedQuota is the storage an archived leaf still holds: what OpenStack
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

// chargedQuota is what a leaf COSTS its budget: the declared limit, or the
// measured OpenStack usage where that is higher. With chargeInUse off it is
// the declared limit alone, which is the pre-2026-08 behaviour.
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
// The result must be computed PER LEAF and only then summed: max() does not
// distribute over addition, so applying it to an already-aggregated total
// gives a different (and wrong) answer.
func chargedQuota(leaf model.Node, resourceIDs []string, chargeInUse bool) common.ProjectQuota {
	if !chargeInUse {
		return leaf.Limit
	}
	// Measured use is compared with everything the project holds, allocations
	// included; whatever runs over that is charged to its own budget, which is
	// where the project lives.
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
			return nil, fmt.Errorf("%q: %d + %d is out of range", resourceID, a[resourceID], b[resourceID])
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

// quotaFits checks whether the needed quota stays within the given limits.
// A limit of common.UnlimitedQuota (-1) means no cap for that resource.
// shortfall is the first resource of resourceIDs where need exceeds limit;
// false when everything fits (quotaFits).
func shortfall(need, limit common.ProjectQuota, resourceIDs []string) (string, bool) {
	for _, resourceID := range resourceIDs {
		if limit[resourceID] != common.UnlimitedQuota && need[resourceID] > limit[resourceID] {
			return resourceID, true
		}
	}
	return "", false
}

func quotaFits(need, limit common.ProjectQuota, resourceIDs []string) bool {
	for _, resourceID := range resourceIDs {
		if limit[resourceID] == common.UnlimitedQuota {
			continue
		}
		if need[resourceID] > limit[resourceID] {
			return false
		}
	}
	return true
}
