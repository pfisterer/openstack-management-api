package tree

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// SetAttributesRequest replaces a node's own attributes as a whole. Groups it
// leaves out are inherited again from above.
type SetAttributesRequest struct {
	Attributes model.Attributes `json:"attributes"`
}

const (
	maxAttributeGroups    = 20
	maxAttributeKeys      = 50
	maxAttributeValueLen  = 512
	maxAttributesJSONSize = 16 * 1024
)

var attributeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// validateAttributes keeps attributes small and their names plain, so that an
// export can carry them as they are.
func validateAttributes(a model.Attributes) error {
	if len(a) > maxAttributeGroups {
		return fmt.Errorf("at most %d attribute groups", maxAttributeGroups)
	}
	for group, values := range a {
		if !attributeNamePattern.MatchString(group) {
			return fmt.Errorf("attribute group %q: lower-case letters, digits, '-' and '_' only", group)
		}
		if len(values) > maxAttributeKeys {
			return fmt.Errorf("attribute group %q: at most %d keys", group, maxAttributeKeys)
		}
		for key, v := range values {
			if !attributeNamePattern.MatchString(key) {
				return fmt.Errorf("attribute %q in %q: lower-case letters, digits, '-' and '_' only", key, group)
			}
			if len(v) > maxAttributeValueLen {
				return fmt.Errorf("attribute %q in %q: at most %d characters", key, group, maxAttributeValueLen)
			}
		}
	}
	if b, _ := json.Marshal(a); len(b) > maxAttributesJSONSize {
		return fmt.Errorf("attributes are larger than %d bytes", maxAttributesJSONSize)
	}
	return nil
}

// effectiveAttributes resolves the attributes of a node from what walkAncestors
// loaded above it.
func (a ancestry) effectiveAttributes(n model.Node) map[string]model.AttributeGroup {
	chain := []model.Node{n}
	if n.ParentID != nil {
		for _, id := range append([]string{*n.ParentID}, a.chain(*n.ParentID)...) {
			chain = append(chain, model.Node{ID: id, Name: a.name[id], Attributes: a.attrs[id]})
		}
	}
	return model.ResolveAttributes(chain)
}

// attributesVisible says whether the viewer looks after the node and so sees
// its attributes: a manager of it or of a budget above it, and on a project its
// owner and admins too. Members of a project and those who may only request
// under a budget do not.
func (a ancestry) attributesVisible(n model.Node, viewer common.TokenSet) bool {
	if viewer.ContainsAny(n.AdminScope) || (n.IsLeaf() && n.Owner != "" && viewer.Contains(n.Owner)) {
		return true
	}
	if n.ParentID == nil {
		return false
	}
	for _, id := range append([]string{*n.ParentID}, a.chain(*n.ParentID)...) {
		if viewer.ContainsAny(a.admins[id]) {
			return true
		}
	}
	return false
}

// AttributesVisible is attributesVisible for one node already loaded.
func (s *Service) AttributesVisible(n *model.Node, userTokens common.TokenList) (bool, error) {
	ctx, cancel := s.newCtx()
	defer cancel()
	var start []string
	if n.ParentID != nil {
		start = []string{*n.ParentID}
	}
	tree, err := s.walkAncestors(ctx, start)
	if err != nil {
		return false, err
	}
	return tree.attributesVisible(*n, common.NewTokenSet(userTokens)), nil
}

// SetAttributes replaces a node's own attributes. They say who pays and what
// for, so they are a manager's to set: on a budget its managers or those above,
// on a project the managers of its budgets — not its owner or admins.
func (s *Service) SetAttributes(id string, req SetAttributesRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	if err := validateAttributes(req.Attributes); err != nil {
		return model.Node{}, err
	}
	current, err := s.loadLive(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	var manages bool
	if current.IsLeaf() {
		manages, err = s.managesParentChain(ctx, userTokens, current)
	} else {
		manages, err = s.managesNode(ctx, userTokens, current)
	}
	if err != nil {
		return model.Node{}, err
	} else if !manages {
		return model.Node{}, common.ErrForbidden
	}

	next := req.Attributes
	if len(next) == 0 {
		next = nil
	}
	entry := model.NewHistoryEntry("attributes_changed", actor, current.Status)
	entry.AttributesFrom = current.Attributes
	entry.AttributesTo = next
	if entry.AttributesFrom == nil {
		entry.AttributesFrom = model.Attributes{}
	}
	if entry.AttributesTo == nil {
		entry.AttributesTo = model.Attributes{}
	}

	var updated model.Node
	found, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		n.Attributes = next
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
