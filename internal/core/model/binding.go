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
	// Facts are counts the provider reports beside the catalogue's resources,
	// for showing, never for arithmetic: for OpenStack how many servers the
	// project has and may have (FactServers, FactServerLimit — the limit is no
	// catalogue resource, it follows the cores). A missing key means "not
	// measured", like InUse.
	Facts map[string]int `json:"facts,omitempty"`
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
	// SyncErrors are the resources the provider could not carry out on its last attempt, by resource id: an availability it could not grant or revoke, a quota it could not set. Cleared once it succeeds. Without them a project read "granted" in the portal while it was not in the cloud.
	SyncErrors map[string]SyncError `json:"sync_errors,omitempty"`
}

// SyncError is why the provider could not carry out one resource of a project.
type SyncError struct {
	// Kind is SyncKindAvailability or SyncKindQuota.
	Kind string `json:"kind"`
	// Reason is the cause as one of the Sync* reasons, for a text the portal can show the project's owner.
	Reason string `json:"reason"`
	// Detail is the provider's own message, for the admins.
	Detail string `json:"detail,omitempty"`
	// Since is when it first failed for this reason (RFC3339).
	Since string `json:"since"`
}

// What a SyncError is about.
const (
	SyncKindAvailability = "availability"
	SyncKindQuota        = "quota"
)

// Why a resource could not be carried out.
const (
	// SyncOverQuota: the cloud refused for a quota of its own — for an availability that is the quota of network shares of the project the service works from, nothing the project's owner can change.
	SyncOverQuota = "over_quota"
	// SyncForbidden: the service may not do it.
	SyncForbidden = "forbidden"
	// SyncNotFound: the object the resource names does not exist (any more).
	SyncNotFound = "not_found"
	// SyncConflict: the cloud refused because of the state the object is in.
	SyncConflict = "conflict"
	// SyncUnreachable: the service did not answer, or answered with a server error.
	SyncUnreachable = "unreachable"
	// SyncFailed: anything else.
	SyncFailed = "failed"
)

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

// Facts a provider may report.
const (
	FactServers     = "servers"      // servers of the project, whatever their state
	FactServerLimit = "server_limit" // servers it may have
)

// ServerCount is how many servers the project has across its providers; false while none has measured it.
func (n Node) ServerCount() (int, bool) {
	total, measured := 0, false
	for _, b := range n.Bindings {
		if v, ok := b.Facts[FactServers]; ok {
			total += v
			measured = true
		}
	}
	return total, measured
}
