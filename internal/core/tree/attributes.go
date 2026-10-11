package tree

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Attributes are free-form facts a manager attaches to a node, grouped under a
// name: {"billing": {"cost_center": "4711", "wbs": "D-2026-017"}}. The platform
// does not interpret them; they are carried into the daily usage rows and the
// export, and whoever bills reads them there.
//
// A GROUP is the unit of inheritance: a node sees the group of the nearest node
// at or above it that sets it, whole — never keys of one group from two levels,
// which for billing would put a cost centre from above next to a project number
// from below that does not belong to it. A group set to an empty map stops the
// inheritance of that group below it.
type Attributes map[string]map[string]string

// AttributeGroup is one group as it applies at a node, and where it comes from.
type AttributeGroup struct {
	Values map[string]string `json:"values"`
	// From is the node that sets the group: the node itself or one above it.
	From PathEntry `json:"from"`
}

// SetAttributesRequest replaces a node's own attributes as a whole. Groups it
// leaves out are inherited again from above.
type SetAttributesRequest struct {
	Attributes Attributes `json:"attributes"`
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
func validateAttributes(a Attributes) error {
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

// ResolveAttributes is what applies at a node: chain is the node itself first,
// then each node above it up to the root. The nearest node that sets a group
// wins; an empty group there means the group does not apply.
func ResolveAttributes(chain []Node) map[string]AttributeGroup {
	out := map[string]AttributeGroup{}
	decided := map[string]bool{}
	for _, n := range chain {
		for group, values := range n.Attributes {
			if decided[group] {
				continue
			}
			decided[group] = true
			if len(values) > 0 {
				out[group] = AttributeGroup{Values: maps.Clone(values), From: PathEntry{ID: n.ID, Name: n.Name}}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// effectiveAttributes resolves the attributes of a node from what walkAncestors
// loaded above it.
func (a ancestry) effectiveAttributes(n Node) map[string]AttributeGroup {
	chain := []Node{n}
	if n.ParentID != nil {
		for _, id := range append([]string{*n.ParentID}, a.chain(*n.ParentID)...) {
			chain = append(chain, Node{ID: id, Name: a.name[id], Attributes: a.attrs[id]})
		}
	}
	return ResolveAttributes(chain)
}

// attributesVisible says whether the viewer looks after the node and so sees
// its attributes: a manager of it or of a budget above it, and on a project its
// owner and admins too. Members of a project and those who may only request
// under a budget do not.
func (a ancestry) attributesVisible(n Node, viewer common.TokenSet) bool {
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
func (s *Service) AttributesVisible(n *Node, userTokens common.TokenList) (bool, error) {
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
func (s *Service) SetAttributes(id string, req SetAttributesRequest, actor Actor, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	if err := validateAttributes(req.Attributes); err != nil {
		return Node{}, err
	}
	current, err := s.loadLive(ctx, id)
	if err != nil {
		return Node{}, err
	}
	var manages bool
	if current.IsLeaf() {
		manages, err = s.managesParentChain(ctx, userTokens, current)
	} else {
		manages, err = s.managesNode(ctx, userTokens, current)
	}
	if err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}

	next := req.Attributes
	if len(next) == 0 {
		next = nil
	}
	entry := newHistoryEntry("attributes_changed", actor, current.Status)
	entry.AttributesFrom = current.Attributes
	entry.AttributesTo = next
	if entry.AttributesFrom == nil {
		entry.AttributesFrom = Attributes{}
	}
	if entry.AttributesTo == nil {
		entry.AttributesTo = Attributes{}
	}

	var updated Node
	found, err := s.store.UpdateNode(ctx, id, func(n *Node) error {
		n.Attributes = next
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
