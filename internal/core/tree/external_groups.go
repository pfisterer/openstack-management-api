package tree

import (
	"fmt"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// RemoveExternalGroup takes a group that was given access in OpenStack — not
// through the portal — off a project. The reconciler keeps such groups assigned
// as long as the project names them, so this is the one place to end that
// access; the next pass removes it in OpenStack. Whoever looks after the
// project may do it, at once: it only takes access away. Adding external groups
// is not offered — groups from the role provider are the way to grant access.
func (s *Service) RemoveExternalGroup(id, groupID string, actor Actor, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return Node{}, err
	}
	if !current.IsLeaf() {
		return Node{}, fmt.Errorf("only projects have external groups")
	}
	if !isOwner(userTokens, current) {
		if manages, err := s.managesNode(ctx, userTokens, current); err != nil {
			return Node{}, err
		} else if !manages {
			return Node{}, common.ErrForbidden
		}
	}
	i := slices.IndexFunc(current.ExternalGroupAssignments, func(g common.ExternalGroupAssignment) bool { return g.GroupID == groupID })
	if i < 0 {
		return Node{}, fmt.Errorf("external group %w", common.ErrNotFound)
	}
	name := current.ExternalGroupAssignments[i].GroupName
	if name == "" {
		name = groupID
	}
	entry := newHistoryEntry("external_group_removed", actor, current.Status)
	entry.Reason = common.Ptr(name)

	var updated Node
	found, err := s.store.UpdateNode(ctx, id, func(n *Node) error {
		n.ExternalGroupAssignments = slices.DeleteFunc(slices.Clone(n.ExternalGroupAssignments),
			func(g common.ExternalGroupAssignment) bool { return g.GroupID == groupID })
		if len(n.ExternalGroupAssignments) == 0 {
			n.ExternalGroupAssignments = nil
		}
		n.History = append(slices.Clone(n.History), entry)
		updated = *n
		return nil
	})
	if err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	if !found {
		return Node{}, common.ErrNotFound
	}
	named, err := s.attachParentNames(ctx, []Node{updated}, userTokens)
	if err != nil {
		return updated, nil
	}
	return named[0], nil
}
