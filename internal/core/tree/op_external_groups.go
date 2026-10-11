package tree

import (
	"fmt"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// RemoveExternalGroup takes a group that was given access in OpenStack — not
// through the portal — off a project. The reconciler keeps such groups assigned
// as long as the project names them, so this is the one place to end that
// access; the next pass removes it in OpenStack. Whoever looks after the
// project may do it, at once: it only takes access away. Adding external groups
// is not offered — groups from the role provider are the way to grant access.
func (s *Service) RemoveExternalGroup(id, groupID string, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if !current.IsLeaf() {
		return model.Node{}, fmt.Errorf("only projects have external groups")
	}
	if !isOwner(userTokens, current) {
		if manages, err := s.managesNode(ctx, userTokens, current); err != nil {
			return model.Node{}, err
		} else if !manages {
			return model.Node{}, common.ErrForbidden
		}
	}
	// Whichever provider assigned it: group IDs are the provider's own, and only one of them knows this one.
	provider, name := "", ""
	for p, b := range current.Bindings {
		if i := slices.IndexFunc(b.ExternalGroupAssignments, func(g common.ExternalGroupAssignment) bool { return g.GroupID == groupID }); i >= 0 {
			provider, name = p, b.ExternalGroupAssignments[i].GroupName
			break
		}
	}
	if provider == "" {
		return model.Node{}, fmt.Errorf("external group %w", common.ErrNotFound)
	}
	if name == "" {
		name = groupID
	}
	entry := model.NewHistoryEntry("external_group_removed", actor, current.Status)
	entry.Reason = new(name)

	var updated model.Node
	found, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		n.EditBinding(provider, func(b *model.Binding) {
			b.ExternalGroupAssignments = slices.DeleteFunc(slices.Clone(b.ExternalGroupAssignments),
				func(g common.ExternalGroupAssignment) bool { return g.GroupID == groupID })
			if len(b.ExternalGroupAssignments) == 0 {
				b.ExternalGroupAssignments = nil
			}
		})
		n.History = append(slices.Clone(n.History), entry)
		updated = *n
		return nil
	})
	if err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	if !found {
		return model.Node{}, common.ErrNotFound
	}
	named, err := s.attachParentNames(ctx, []model.Node{updated}, userTokens)
	if err != nil {
		return updated, nil
	}
	return named[0], nil
}
