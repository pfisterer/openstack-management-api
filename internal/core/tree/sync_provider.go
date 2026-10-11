package tree

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// ProviderSync is everything the system that carries the tree out — today the
// OpenStack reconciler — may do with it: read what is wanted, and report what it
// found. Each write is a named operation, and the rule for how a report lands
// on a node lives here, next to the model, instead of in a callback the
// reconciler passes to the store.
//
// Every write goes through Store.UpdateNode or Store.DeleteNodeIf, so it applies
// to the node as it is NOW: a pass loads its leaves once and then spends seconds
// per leaf in OpenStack, and a user may have filed a change, been approved or
// released the project meanwhile.
//
// One per provider: what it reports lands in the leaf's binding under its name.
type ProviderSync struct {
	store    nodestore.Store
	provider string
	changes  <-chan struct{}
	// resources is the current catalogue, to tell which leaves are this
	// provider's; nil (NewProviderSync) means all of them.
	resources func() []common.Resource
}

// NewProviderSync is provider's view of the tree straight from its store, without a service that announces changes; Service.ProviderSync is the one with them.
func NewProviderSync(store nodestore.Store, provider string) *ProviderSync {
	return &ProviderSync{store: store, provider: provider}
}

// Provider is whose view this is.
func (p *ProviderSync) Provider() string { return p.provider }

// Changes receives when the service changed what is wanted since the last receive. Nil — never ready — for a view made by NewProviderSync.
func (p *ProviderSync) Changes() <-chan struct{} { return p.changes }

// Leaves returns the projects in the given statuses that are this provider's
// business: those it already carries out (they have its binding — a project
// emptied of its resources still has to be cleaned up there) and those that
// hold more than zero of one of its resources.
func (p *ProviderSync) Leaves(ctx context.Context, statuses []string) ([]model.Node, error) {
	leaves, err := p.store.ListNodes(ctx, nodestore.NodeQuery{Kinds: []string{model.KindProject}, Statuses: statuses}, 0, 0)
	if err != nil || p.resources == nil {
		return leaves, err
	}
	var mine []string
	for _, r := range p.resources() {
		if r.BelongsTo(p.provider) {
			mine = append(mine, r.ID)
		}
	}
	return slices.DeleteFunc(leaves, func(n model.Node) bool { return !concerns(n, p.provider, mine) }), nil
}

// concerns reports whether leaf n is provider's business, given the ids of
// provider's resources.
func concerns(n model.Node, provider string, resourceIDs []string) bool {
	if _, bound := n.Bindings[provider]; bound {
		return true
	}
	limit := n.EffectiveLimit()
	return slices.ContainsFunc(resourceIDs, func(id string) bool { return limit[id] > 0 || limit[id] == common.UnlimitedQuota })
}

// Budgets returns every budget, for naming a project's path in the tree.
func (p *ProviderSync) Budgets(ctx context.Context) ([]model.Node, error) {
	return p.store.ListNodes(ctx, nodestore.NodeQuery{Kinds: []string{model.KindBudget}}, 0, 0)
}

// RecordExternalID records the project created for a leaf, whatever the leaf's
// status: one released meanwhile still needs the ID, or its project could never
// be cleaned up.
func (p *ProviderSync) RecordExternalID(ctx context.Context, leafID, externalID string) error {
	_, err := p.store.UpdateNode(ctx, leafID, func(n *model.Node) error {
		if n.Binding(p.provider).ExternalID == externalID {
			return nodestore.ErrSkipUpdate
		}
		n.EditBinding(p.provider, func(b *model.Binding) { b.ExternalID = externalID })
		return nil
	})
	return err
}

// RecordObservation writes what a pass found at the provider, touching only
// those fields and only when something changed (see ApplyObservation).
func (p *ProviderSync) RecordObservation(ctx context.Context, leafID, externalID string, o *Observation) error {
	_, err := p.store.UpdateNode(ctx, leafID, func(n *model.Node) error {
		if !Observe(n, p.provider, externalID, o) {
			return nodestore.ErrSkipUpdate
		}
		return nil
	})
	return err
}

// MarkArchived records that a released project was archived. A leaf in any
// other status is left alone.
func (p *ProviderSync) MarkArchived(ctx context.Context, leafID string) error {
	_, err := p.store.UpdateNode(ctx, leafID, func(n *model.Node) error {
		if !model.MarkArchived(n) {
			return nodestore.ErrSkipUpdate
		}
		return nil
	})
	return err
}

// PurgeReport is one pass's progress in emptying a project before its
// deletion. Removed counts what this pass deleted; the leaf keeps the total.
type PurgeReport struct {
	Step    string
	Index   int
	Steps   int
	Blocked string
	Detail  string
	Removed int
}

// RecordPurgeProgress writes where emptying the project stands. Only on a
// change: a pass every 30 s that rewrites the same state would be a write
// every 30 s for nothing.
func (p *ProviderSync) RecordPurgeProgress(ctx context.Context, leafID string, r PurgeReport) error {
	_, err := p.store.UpdateNode(ctx, leafID, func(n *model.Node) error {
		prev := n.Binding(p.provider).Purge
		removed := r.Removed
		if prev != nil {
			removed += prev.Removed
		}
		if prev != nil && prev.Step == r.Step && prev.Blocked == r.Blocked && prev.Detail == r.Detail && r.Removed == 0 {
			return nodestore.ErrSkipUpdate
		}
		next := &model.PurgeProgress{
			Step: r.Step, Index: r.Index, Steps: r.Steps,
			Blocked: r.Blocked, Detail: r.Detail,
			Removed: removed, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		n.EditBinding(p.provider, func(b *model.Binding) { b.Purge = next })
		return nil
	})
	return err
}

// RemoveRetired deletes a released or archived leaf whose project is gone.
// Reports false when the leaf is no longer retired — then it is not deleted.
func (p *ProviderSync) RemoveRetired(ctx context.Context, leafID string) (bool, error) {
	return p.store.DeleteNodeIf(ctx, leafID, func(n model.Node) bool { return model.IsRetiredStatus(n.Status) })
}

// RemoveImport deletes an imported leaf: its project left the scope, or it
// turned out to shadow a managed one. Reports false once the leaf was promoted
// or moved — it then belongs to the managed tree.
func (p *ProviderSync) RemoveImport(ctx context.Context, leafID string) (bool, error) {
	return p.store.DeleteNodeIf(ctx, leafID, IsStillImported)
}

// AddImport stores a project found in OpenStack as a new imported leaf.
func (p *ProviderSync) AddImport(ctx context.Context, leaf model.Node) error {
	return p.store.UpsertNode(ctx, leaf)
}

// RefreshImport updates an imported leaf with what OpenStack says about its
// project now, on the node as it is: history, parent, owner and flags belong to
// the tree, and a promotion requested during the pass must survive it.
func (p *ProviderSync) RefreshImport(ctx context.Context, leaf model.Node, o *Observation) error {
	_, err := p.store.UpdateNode(ctx, leaf.ID, func(n *model.Node) error {
		if !IsStillImported(*n) || slices.Contains(n.Flags, model.FlagPromoteOnReconcile) {
			return nodestore.ErrSkipUpdate
		}
		n.Name = leaf.Name
		n.Reason = leaf.Reason
		n.Limit = leaf.Limit
		n.AuthorizedUsers = leaf.AuthorizedUsers
		found := leaf.Binding(p.provider)
		n.EditBinding(p.provider, func(b *model.Binding) {
			b.ExternalGroupAssignments = found.ExternalGroupAssignments
			b.ExternalName = found.ExternalName
			ApplyObservation(b, found.ExternalID, o)
		})
		return nil
	})
	return err
}

// Promote turns an imported leaf whose adoption was asked for into a pending
// project. current are the members OpenStack has now: the leaf only knows them
// as of the last pass, and once the project is approved the member sync removes
// everyone the leaf does not name. Reports false when the promotion was
// withdrawn meanwhile.
func (p *ProviderSync) Promote(ctx context.Context, leafID string, current []common.AuthorizedUser) (bool, error) {
	return p.store.UpdateNode(ctx, leafID, func(n *model.Node) error {
		if n.Status != model.StatusImported || !slices.Contains(n.Flags, model.FlagPromoteOnReconcile) {
			return nodestore.ErrSkipUpdate
		}
		n.AuthorizedUsers = MergeMembers(n.AuthorizedUsers, current, n.Owner)
		n.Status = model.StatusPending
		n.Flags = slices.DeleteFunc(slices.Clone(n.Flags), func(f string) bool { return f == model.FlagPromoteOnReconcile })
		return nil
	})
}

// IsStillImported is the guard for deleting or refreshing an imported leaf:
// once promoted it belongs to the managed tree and the import logic must leave
// it be.
func IsStillImported(n model.Node) bool {
	return n.Status == model.StatusImported
}

// MergeMembers adds to a leaf's members those of current it does not name yet,
// leaving out the owner and keeping the role the leaf gave someone already
// there.
func MergeMembers(leafUsers, current []common.AuthorizedUser, owner string) []common.AuthorizedUser {
	out := slices.Clone(leafUsers)
	known := map[string]bool{strings.ToLower(owner): true}
	for _, u := range leafUsers {
		known[strings.ToLower(u.Token)] = true
	}
	for _, u := range current {
		if !known[strings.ToLower(u.Token)] {
			known[strings.ToLower(u.Token)] = true
			out = append(out, u)
		}
	}
	return out
}

// Observation is what one pass read from the provider about a project. A nil
// *Observation means it could not be read — "not measured", never "nothing in
// use".
type Observation struct {
	Overcommitted bool
	InUse         common.ProjectQuota
	// Facts are the counts beside the resources (model.Fact*); nil when none
	// were read, and then the last known ones stay.
	Facts map[string]int
	// GrantUse is what uses each availability; nil when it could not be read,
	// and then the last known values stay.
	GrantUse common.ProjectQuota
}

// ApplyObservation writes an observation onto a node and reports whether
// anything changed.
//
// The rule that matters: the measurement is only written when there WAS one.
// A failed quota-detail call used to arrive here as an empty in-use map, which
// differs from whatever was stored and therefore overwrote it with nothing.
// Since the accounting bills max(limit, in-use), the charge then silently fell
// back to the declared limit until the next successful pass — the
// shrink-after-filling loophole, reopened by a transient API error, and
// Overcommitted cleared along with it so the UI stopped warning too.
func ApplyObservation(b *model.Binding, externalID string, o *Observation) bool {
	changed := b.ExternalID != externalID
	if o != nil {
		changed = changed || b.Overcommitted != o.Overcommitted || !quotaEqual(b.InUse, o.InUse) ||
			(o.Facts != nil && !maps.Equal(b.Facts, o.Facts)) ||
			(o.GrantUse != nil && !quotaEqual(b.GrantUse, o.GrantUse))
	}
	if !changed {
		return false
	}
	b.ExternalID = externalID
	if o != nil {
		b.Overcommitted = o.Overcommitted
		b.InUse = o.InUse
		if o.Facts != nil {
			b.Facts = o.Facts
		}
		if o.GrantUse != nil {
			b.GrantUse = o.GrantUse
		}
	}
	return true
}

// Observe is ApplyObservation on provider's binding of n. The binding is only replaced when something changed.
func Observe(n *model.Node, provider, externalID string, o *Observation) bool {
	b := n.Binding(provider)
	if !ApplyObservation(&b, externalID, o) {
		return false
	}
	n.EditBinding(provider, func(nb *model.Binding) { *nb = b })
	return true
}

// quotaEqual compares two quota maps; a missing key and a zero differ, or a
// measured 0 would read the same as an unmeasured resource.
func quotaEqual(a, b common.ProjectQuota) bool {
	if len(a) != len(b) {
		return false
	}
	for id, av := range a {
		if bv, ok := b[id]; !ok || av != bv {
			return false
		}
	}
	return true
}
