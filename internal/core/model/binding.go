package model

import (
	"maps"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Binding is one provider's side of a project: where the project runs there and what the provider measured. A leaf has one per provider that carries it out, so a project can have both servers and workspaces.
type Binding struct {
	// ExternalID is the project's ID at the provider, ExternalName its name there.
	ExternalID    string `json:"external_id,omitempty"`
	ExternalName  string `json:"external_name,omitempty"`
	Overcommitted bool   `json:"overcommitted,omitempty"`
	// InUse is what the project actually consumes, as opposed to the Limit it
	// was granted. Only resources the provider measures appear here; a
	// missing key means "not measured", not zero.
	//
	// It exists because a quota reduction below current usage is something
	// OpenStack accepts silently — the servers keep running and only new ones
	// are refused. Without this the platform sees only the smaller claim and a
	// shrink looks like capacity handed back when nothing was.
	InUse common.ProjectQuota `json:"in_use,omitempty"`
	// Servers is how many servers the project has, measured with InUse; nil
	// until a pass has measured it.
	Servers *int `json:"servers,omitempty"`
	// ServerLimit is how many servers the project may have, read with
	// Servers. Not a catalogue resource: for a managed OpenStack project it
	// follows the cores (os_linked_field), an import has its own.
	ServerLimit *int `json:"server_limit,omitempty"`
	// GrantUse says, per availability of the catalogue (a network, flavour or
	// image), how many of the project's servers or devices use it: whether
	// switching it off would take something away. A missing key means "not
	// measured", like InUse.
	GrantUse common.ProjectQuota `json:"grant_use,omitempty"`
	// Purge is how far emptying the project before its deletion has got,
	// written by the reconciler on every pass while it runs, so its owner
	// sees why a deletion takes long.
	Purge                    *PurgeProgress                   `json:"purge,omitempty"`
	ExternalGroupAssignments []common.ExternalGroupAssignment `json:"external_group_assignments,omitempty"`
}

// Binding is what provider reports about the project; the zero value when it has not reported anything.
func (n Node) Binding(provider string) Binding { return n.Bindings[provider] }

// EditBinding changes provider's side of the project. The map is replaced rather than written into: nodes are copied shallowly, and a write into a shared map would change the copy it came from too.
func (n *Node) EditBinding(provider string, edit func(b *Binding)) {
	b := n.Bindings[provider]
	edit(&b)
	m := maps.Clone(n.Bindings)
	if m == nil {
		m = map[string]Binding{}
	}
	m[provider] = b
	n.Bindings = m
}

// InUse is what the project consumes across all its providers. Resource IDs are unique across providers, so the measurements do not overlap.
func (n Node) InUse() common.ProjectQuota {
	var out common.ProjectQuota
	for _, b := range n.Bindings {
		for id, v := range b.InUse {
			if out == nil {
				out = common.ProjectQuota{}
			}
			out[id] = v
		}
	}
	return out
}

// ServerCount is how many servers the project has across its providers; false while none has measured it.
func (n Node) ServerCount() (int, bool) {
	total, measured := 0, false
	for _, b := range n.Bindings {
		if b.Servers != nil {
			total += *b.Servers
			measured = true
		}
	}
	return total, measured
}
