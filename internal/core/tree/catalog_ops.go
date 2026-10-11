package tree

import (
	"context"
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// What the tree does when root admins add, withdraw or remove an availability
// (package catalog decides when; these only change the nodes).
//
// Withdrawing is the one place an availability is taken away from a whole
// subtree at once. Everywhere else that is refused while somebody below holds
// it (checkAvailabilityWithdrawal), so that a budget manager cannot revoke a
// network in projects whose owners never heard of the change. Here the
// resource itself goes away — there is nobody below who could decide
// differently, and a root admin confirmed a list of everyone affected first.

// Holder is a node that holds an availability, and how.
type Holder struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// Via is where the 1 is stored: "limit", "allocation" (from AllocationFrom),
	// "pending" (a change waiting for approval) or "auto_approve" (a
	// per-requester cap).
	Via            string `json:"via"`
	AllocationFrom string `json:"allocation_from,omitempty"`
}

// Holders lists the nodes holding availability id, the root excepted: it
// holds everything while the resource is offered.
func (s *Service) Holders(ctx context.Context, id string) ([]Holder, error) {
	nodes, err := s.store.ListNodes(ctx, NodeQuery{}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := []Holder{}
	for _, n := range nodes {
		if n.ID == RootNodeID {
			continue
		}
		h := Holder{NodeID: n.ID, Name: nodeLabel(n), Kind: n.Kind, Status: n.Status}
		if n.Limit[id] == 1 {
			h.Via = "limit"
			out = append(out, h)
		}
		for _, a := range n.Allocations {
			if a.Limit[id] == 1 {
				h.Via, h.AllocationFrom = "allocation", a.BudgetID
				out = append(out, h)
			}
		}
		h.AllocationFrom = ""
		if n.Pending != nil && n.Pending.Limit != nil && (*n.Pending.Limit)[id] == 1 {
			h.Via = "pending"
			out = append(out, h)
		}
		if n.AutoApprove != nil && n.AutoApprove.PerRequesterLimit[id] == 1 {
			h.Via = "auto_approve"
			out = append(out, h)
		}
	}
	return out, nil
}

// WithdrawAvailability sets availability id to 0 wherever it is stored, the
// root included, and returns how many nodes changed. The reconciler then
// revokes it in OpenStack. Running it again changes nothing.
func (s *Service) WithdrawAvailability(ctx context.Context, id string) (int, error) {
	return s.rewriteAll(ctx, func(q common.ProjectQuota) bool {
		if v, ok := q[id]; ok && v != 0 {
			q[id] = 0
			return true
		}
		return false
	})
}

// ForgetResource removes id from every node, for a resource that has left the
// catalogue: a stored key the catalogue does not know makes the node
// unsavable, and a resource added again under the same id starts clean.
// Allocations left granting nothing go too, here and in a withdrawal.
func (s *Service) ForgetResource(ctx context.Context, id string) (int, error) {
	return s.rewriteAll(ctx, func(q common.ProjectQuota) bool {
		if _, ok := q[id]; ok {
			delete(q, id)
			return true
		}
		return false
	})
}

// AdoptAvailability gives the root availability id, so it can be delegated —
// what Bootstrap does for resources found in the catalogue at startup.
func (s *Service) AdoptAvailability(ctx context.Context, id string) error {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	_, err := s.store.UpdateNode(ctx, RootNodeID, func(n *Node) error {
		if n.Limit[id] == 1 {
			return ErrSkipUpdate
		}
		if n.Limit == nil {
			n.Limit = common.ProjectQuota{}
		}
		n.Limit[id] = 1
		return nil
	})
	return err
}

// rewriteAll applies edit to every stored limit map of every node — own limit,
// allocations, a pending change, the per-requester cap — and writes the nodes
// it changed. Under approvalMu, so no approval interleaves with a half
// rewritten tree.
func (s *Service) rewriteAll(ctx context.Context, edit func(common.ProjectQuota) bool) (int, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	nodes, err := s.store.ListNodes(ctx, NodeQuery{}, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("list nodes: %w", err)
	}
	changed := 0
	for _, listed := range nodes {
		wrote, err := s.store.UpdateNode(ctx, listed.ID, func(n *Node) error {
			touched := false
			if n.Limit != nil && edit(n.Limit) {
				touched = true
			}
			kept := n.Allocations[:0]
			for _, a := range n.Allocations {
				if a.Limit != nil && edit(a.Limit) {
					touched = true
					if grantsNothing(a.Limit) {
						// It existed for this availability alone.
						continue
					}
				}
				kept = append(kept, a)
			}
			if len(kept) == 0 {
				kept = nil
			}
			n.Allocations = kept
			if n.Pending != nil && n.Pending.Limit != nil && edit(*n.Pending.Limit) {
				touched = true
			}
			if n.AutoApprove != nil && n.AutoApprove.PerRequesterLimit != nil && edit(n.AutoApprove.PerRequesterLimit) {
				touched = true
			}
			if !touched {
				return ErrSkipUpdate
			}
			return nil
		})
		if err != nil {
			return changed, fmt.Errorf("update node %s: %w", listed.ID, err)
		}
		if wrote {
			changed++
		}
	}
	return changed, nil
}

func grantsNothing(q common.ProjectQuota) bool {
	for _, v := range q {
		if v != 0 {
			return false
		}
	}
	return true
}

// ProjectsByOSID maps the OpenStack project ID of every project that has one
// to the project, so a grant found in OpenStack can be named.
func (s *Service) ProjectsByOSID(ctx context.Context) (map[string]Holder, error) {
	nodes, err := s.store.ListNodes(ctx, NodeQuery{Kinds: []string{KindProject}}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	out := make(map[string]Holder, len(nodes))
	for _, n := range nodes {
		if n.OSProjectID != "" {
			out[n.OSProjectID] = Holder{NodeID: n.ID, Name: nodeLabel(n), Kind: n.Kind, Status: n.Status}
		}
	}
	return out, nil
}
