package model

import (
	"maps"
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
