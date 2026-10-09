package tree

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// dropRetiredResources removes from every node the resources the catalogue no
// longer has — a quantity taken out of the configuration, an availability
// removed for good. Left in place they make the node uneditable: an edit sends
// the whole limit back, and validateKnownResources refuses the unknown key.
//
// Runs on every start after the catalogue is loaded (a failed load stops the
// start before this), and is a no-op once the data is clean. History entries
// keep what they recorded.
func (s *Service) dropRetiredResources(ctx context.Context) error {
	known := map[string]bool{}
	for _, r := range s.resources() {
		known[r.ID] = true
	}
	nodes, err := s.store.ListNodes(ctx, NodeQuery{}, 0, 0)
	if err != nil {
		return fmt.Errorf("load nodes to drop retired resources: %w", err)
	}
	dropped := map[string]bool{}
	rewritten := 0
	for _, n := range nodes {
		if len(retiredKeys(&n, known)) == 0 {
			continue
		}
		wrote, err := s.store.UpdateNode(ctx, n.ID, func(stored *Node) error {
			keys := retiredKeys(stored, known)
			if len(keys) == 0 {
				return ErrSkipUpdate
			}
			for _, k := range keys {
				dropped[k] = true
			}
			dropKeys(stored, known)
			return nil
		})
		if err != nil {
			return fmt.Errorf("drop retired resources of %s: %w", n.ID, err)
		}
		if wrote {
			rewritten++
		}
	}
	if rewritten > 0 {
		names := slices.Collect(maps.Keys(dropped))
		sort.Strings(names)
		s.log.Infow("dropped resources the catalogue no longer has", "resources", names, "nodes", rewritten)
	}
	return nil
}

// quotasOf lists every quota on a node that names resources.
func quotasOf(n *Node) []common.ProjectQuota {
	out := []common.ProjectQuota{n.Limit}
	if n.Pending != nil && n.Pending.Limit != nil {
		out = append(out, *n.Pending.Limit)
	}
	for _, a := range n.Allocations {
		out = append(out, a.Limit)
	}
	if n.AutoApprove != nil {
		out = append(out, n.AutoApprove.PerRequesterLimit)
	}
	return out
}

// retiredKeys names the unknown resources on a node.
func retiredKeys(n *Node, known map[string]bool) []string {
	var out []string
	for _, q := range quotasOf(n) {
		for k := range q {
			if !known[k] && !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// dropKeys removes the unknown resources, with copies rather than edits in
// place: the maps may be shared with whatever handed the node over. An
// allocation left with nothing is removed.
func dropKeys(n *Node, known map[string]bool) {
	clean := func(q common.ProjectQuota) common.ProjectQuota {
		if q == nil {
			return nil
		}
		out := make(common.ProjectQuota, len(q))
		for k, v := range q {
			if known[k] {
				out[k] = v
			}
		}
		return out
	}
	n.Limit = clean(n.Limit)
	if n.Pending != nil && n.Pending.Limit != nil {
		p := *n.Pending
		l := clean(*p.Limit)
		p.Limit = &l
		n.Pending = &p
	}
	if len(n.Allocations) > 0 {
		var kept []Allocation
		for _, a := range n.Allocations {
			a.Limit = clean(a.Limit)
			if len(a.Limit) > 0 {
				kept = append(kept, a)
			}
		}
		n.Allocations = kept
	}
	if n.AutoApprove != nil {
		a := *n.AutoApprove
		a.PerRequesterLimit = clean(a.PerRequesterLimit)
		n.AutoApprove = &a
	}
}
