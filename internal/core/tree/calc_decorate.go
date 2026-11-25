package tree

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// attachUsage computes per-status subtree usage for the budget nodes in the list
// and returns a new slice with usage attached (leaves are passed through).
// attachChildCounts fills Node.ChildCount for every budget in the set. Leaves
// never have children, so they are skipped rather than queried.
func (s *Service) attachChildCounts(ctx context.Context, nodes []model.Node) ([]model.Node, error) {
	parentIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == model.KindBudget {
			parentIDs = append(parentIDs, n.ID)
		}
	}
	if len(parentIDs) == 0 {
		return nodes, nil
	}
	counts, err := s.store.CountChildren(ctx, parentIDs)
	if err != nil {
		return nil, fmt.Errorf("count children: %w", err)
	}
	for i := range nodes {
		if nodes[i].Kind == model.KindBudget {
			c := counts[nodes[i].ID]
			nodes[i].ChildCount = c.Budgets + c.Projects
			nodes[i].ChildBudgetCount = c.Budgets
			nodes[i].ChildProjectCount = c.Projects
		}
	}
	return nodes, nil
}

// attachParentNames fills Node.ParentName and Node.ParentPath for every node
// that has a parent, and the budget name and path of every allocation. Without
// it a client that shows "paid from <budget>" has to fetch every parent
// separately — one request per node — and a name alone is ambiguous: two
// budgets called "Vorlesung" under different parents look the same. Only names
// are exposed; no other field of those budgets is copied.
//
// It also resolves the attributes that apply at each node, which need the same
// walk.
//
// viewer is who asks: attributes go only to those who look after the node
// (see attributesVisible), everyone else gets the node without them.
func (s *Service) attachParentNames(ctx context.Context, nodes []model.Node, viewer common.TokenList) ([]model.Node, error) {
	var start []string
	for _, n := range nodes {
		start = append(start, n.FundingBudgets()...)
		for _, a := range n.Allocations {
			start = append(start, a.BudgetID)
		}
		if n.Project != nil {
			for _, a := range n.Project.Allocations {
				start = append(start, a.BudgetID)
			}
		}
	}
	tree, err := s.walkAncestors(ctx, start)
	if err != nil {
		return nil, err
	}
	viewerSet := common.NewTokenSet(viewer)
	for i := range nodes {
		if tree.attributesVisible(nodes[i], viewerSet) {
			nodes[i].EffectiveAttributes = tree.effectiveAttributes(nodes[i])
		} else {
			nodes[i].Attributes = nil
			nodes[i].EffectiveAttributes = nil
		}
		nodes[i].InheritedCaps = s.capsOf(tree, nodes[i])
		if nodes[i].ParentID != nil {
			nodes[i].ParentName = tree.nodes[*nodes[i].ParentID].Name
			nodes[i].ParentPath = tree.path(*nodes[i].ParentID)
		}
		nodes[i].Allocations = tree.nameAllocations(nodes[i].Allocations)
		if nodes[i].Project != nil {
			p := *nodes[i].Project
			p.Allocations = tree.nameAllocations(p.Allocations)
			nodes[i].Project = &p
		}
	}
	return nodes, nil
}

// capsOf is what applies to a node of the caps the budgets set: below a budget or an allocation the strictest along its chain; for a project, merged over the chains of the budgets paying for it (model.MergeCaps).
func (s *Service) capsOf(tree ancestry, n model.Node) common.Caps {
	if n.Kind != model.KindProject {
		return model.ResolveCaps(tree.budgetsAbove(n), s.capDefs())
	}
	var per []common.Caps
	for _, b := range n.FundingBudgetIDs {
		per = append(per, model.ResolveCaps(tree.chainFrom(b), s.capDefs()))
	}
	if len(per) == 0 {
		return nil
	}
	return model.MergeCaps(per, s.capDefs())
}

// nameAllocations gives each allocation the name and path of its budget, in a copy.
func (a ancestry) nameAllocations(allocs []model.Allocation) []model.Allocation {
	if len(allocs) == 0 {
		return allocs
	}
	named := slices.Clone(allocs)
	for j := range named {
		named[j].BudgetName = a.nodes[named[j].BudgetID].Name
		named[j].BudgetPath = a.path(named[j].BudgetID)
	}
	return named
}

// budgetsAbove are the budgets above n as walkAncestors loaded them, parent first: the chain the model.Resolve* functions take, as parentChainNodes would load it. Only the fields walkAncestors keeps (slimNode) are set; a budget it could not load is an empty node with its id. Nil for the root.
func (a ancestry) budgetsAbove(n model.Node) []model.Node {
	if n.ParentID == nil {
		return nil
	}
	return a.chainFrom(*n.ParentID)
}

// chainFrom is the budget id and those above it as walkAncestors loaded them, nearest first.
func (a ancestry) chainFrom(id string) []model.Node {
	ids := append([]string{id}, a.chain(id)...)
	out := make([]model.Node, 0, len(ids))
	for _, id := range ids {
		b := a.nodes[id]
		b.ID = id
		out = append(out, b)
	}
	return out
}

// maxAncestorDepth bounds the upward walk in walkAncestors. A tree this deep is
// already a bug; the cap is here so a cycle that slipped past the cycle guard
// cannot turn into an endless query loop.
const maxAncestorDepth = 64

// ancestry is what walkAncestors learnt: each loaded node's parent, and the node itself as far as views need it (slimNode).
type ancestry struct {
	parent map[string]string
	nodes  map[string]model.Node
	log    interface{ Warnw(string, ...any) }
}

// slimNode keeps of a loaded ancestor what views resolve along the chain: its name, attributes and managers, and the budget settings passed down (model.Resolve*). Anything added to those settings is added here, or the inherited value in a response silently ignores it.
func slimNode(n model.Node) model.Node {
	return model.Node{
		ID: n.ID, ParentID: n.ParentID, Name: n.Name, Attributes: n.Attributes, AdminScope: n.AdminScope,
		Caps:          n.Caps,
		InheritsLimit: n.InheritsLimit, MaxProjectTermDays: n.MaxProjectTermDays,
	}
}

// chain returns the nodes above id, parent first, stopping at a cycle.
func (a ancestry) chain(id string) []string {
	var out []string
	seen := map[string]struct{}{id: {}}
	for p, ok := a.parent[id]; ok; p, ok = a.parent[p] {
		if _, dup := seen[p]; dup {
			// Report what was walked so far rather than failing the whole
			// listing: a broken tree should still be visible enough to fix.
			a.log.Warnw("cycle in ancestor chain", "node", id, "at", p)
			break
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// path names the budgets from below the root down to id itself, root-most
// first. The root is left out — every path starts there, so it tells nobody
// anything — and id being the root gives an empty path.
func (a ancestry) path(id string) []model.PathEntry {
	if _, ok := a.nodes[id]; !ok {
		return nil
	}
	ids := append([]string{id}, a.chain(id)...)
	out := make([]model.PathEntry, 0, len(ids))
	for _, x := range ids {
		if _, hasParent := a.parent[x]; !hasParent {
			continue // the root
		}
		out = append(out, model.PathEntry{ID: x, Name: a.nodes[x].Name})
	}
	slices.Reverse(out)
	return out
}

// walkAncestors loads the nodes start and everything above them.
//
// Batched by LEVEL rather than per node: walking each chain with nodeChain
// would be one query per node per level, and a listing can hold hundreds. Here
// each level of the tree costs one query no matter how many nodes are on it,
// and the levels above the first are shared by nearly all of them anyway.
func (s *Service) walkAncestors(ctx context.Context, start []string) (ancestry, error) {
	a := ancestry{parent: map[string]string{}, nodes: map[string]model.Node{}, log: s.log}
	var frontier []string
	for _, id := range start {
		if !slices.Contains(frontier, id) {
			frontier = append(frontier, id)
		}
	}
	for depth := 0; len(frontier) > 0 && depth < maxAncestorDepth; depth++ {
		level, err := s.store.ListNodes(ctx, nodestore.NodeQuery{IDs: frontier}, 0, 0)
		if err != nil {
			return ancestry{}, fmt.Errorf("load ancestors: %w", err)
		}
		var next []string
		for _, n := range level {
			a.nodes[n.ID] = slimNode(n)
			if n.ParentID == nil {
				continue
			}
			a.parent[n.ID] = *n.ParentID
			if _, known := a.nodes[*n.ParentID]; !known && !slices.Contains(next, *n.ParentID) {
				next = append(next, *n.ParentID)
			}
		}
		frontier = next
	}
	return a, nil
}

// attachAncestorIDs fills Node.AncestorIDs, root-most first, for every node
// given.
func (s *Service) attachAncestorIDs(ctx context.Context, nodes []model.Node) ([]model.Node, error) {
	var start []string
	for _, n := range nodes {
		if n.ParentID != nil {
			start = append(start, *n.ParentID)
		}
	}
	tree, err := s.walkAncestors(ctx, start)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if nodes[i].ParentID == nil {
			nodes[i].AncestorIDs = nil
			continue
		}
		// The parent, then what is above it; the field is documented root-most
		// first, which is the order a client compares against and reads in.
		chain := append([]string{*nodes[i].ParentID}, tree.chain(*nodes[i].ParentID)...)
		slices.Reverse(chain)
		nodes[i].AncestorIDs = chain
	}
	return nodes, nil
}

// attachUsage adds the derived fields every view needs on top of the stored
// node: the subtree usage rollup, what of it is measurably used, the direct child count, the parent's name and
// the resources in scope at that node.
func (s *Service) attachUsage(ctx context.Context, nodes []model.Node, viewer common.TokenList) ([]model.Node, error) {
	budgets := make([]model.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == model.KindBudget {
			budgets = append(budgets, n)
		}
	}
	usage, inUse, err := s.loadSubtreeUsageAndInUse(ctx, budgets)
	if err != nil {
		return nil, fmt.Errorf("compute subtree usage: %w", err)
	}
	out := make([]model.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == model.KindBudget {
			n.Usage = usage[n.ID]
			n.SubtreeInUse = inUse[n.ID]
		}
		out = append(out, n)
	}
	out, err = s.attachAllocations(ctx, out)
	if err != nil {
		return nil, err
	}
	out, err = s.attachInheritedLimits(ctx, out)
	if err != nil {
		return nil, err
	}
	out, err = s.attachChildCounts(ctx, out)
	if err != nil {
		return nil, err
	}
	out, err = s.attachParentNames(ctx, out, viewer)
	if err != nil {
		return nil, err
	}
	return s.attachAvailableResources(out), nil
}

// availableResourcesFor lists the resources in scope at one node.
//
// The root sees the whole catalogue: it is where delegation starts, so hiding
// anything there would make it ungrantable. Every other node sees what its own
// limit carries — a quantity above zero, an unlimited quantity, or a granted
// availability.
//
// Zero means "not delegated" and therefore disappears. That is consistent rather
// than harsh: the only place a resource can be handed down is the parent, and
// there it is visible by this same rule, or the parent would not have it either.
func (s *Service) availableResourcesFor(n model.Node) []string {
	out := make([]string, 0, len(s.resources()))
	for _, r := range s.resources() {
		if r.Withdrawn {
			// Nobody may be granted it any more, so nobody is offered it.
			continue
		}
		if n.ID == model.RootNodeID {
			out = append(out, r.ID)
			continue
		}
		if n.Limit.Holds(r.ID) {
			out = append(out, r.ID)
		}
	}
	return out
}

// attachAvailableResources fills the derived scope list on every node. Ordered
// by the catalogue rather than by the node's map, so two nodes list the same
// resources in the same order and the UI does not reshuffle between them.
func (s *Service) attachAvailableResources(nodes []model.Node) []model.Node {
	out := make([]model.Node, 0, len(nodes))
	for _, n := range nodes {
		n.AvailableResources = s.availableResourcesFor(n)
		out = append(out, n)
	}
	return out
}

// attachInheritedLimits shows a budget that inherits with the limit that
// applies to it, for reading only: the API, the UI's bars and the resources a
// request may name all want that one. Nothing written goes through here, and
// nodes is a slice of copies (attachUsage), so the store's are untouched.
func (s *Service) attachInheritedLimits(ctx context.Context, nodes []model.Node) ([]model.Node, error) {
	for i := range nodes {
		if nodes[i].Kind != model.KindBudget || !nodes[i].InheritsLimit {
			continue
		}
		b, err := s.bindingBudget(ctx, &nodes[i])
		if err != nil {
			return nil, err
		}
		nodes[i].Limit = maps.Clone(b.Limit)
	}
	return nodes, nil
}
