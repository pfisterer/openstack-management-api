package tree

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
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
type ProviderSync struct {
	store Store
}

// NewProviderSync gives the reconciler its view of the tree.
func NewProviderSync(store Store) *ProviderSync {
	return &ProviderSync{store: store}
}

// Leaves returns the projects in the given statuses.
func (p *ProviderSync) Leaves(ctx context.Context, statuses []string) ([]model.Node, error) {
	return p.store.ListNodes(ctx, NodeQuery{Kinds: []string{model.KindProject}, Statuses: statuses}, 0, 0)
}

// Budgets returns every budget, for naming a project's path in the tree.
func (p *ProviderSync) Budgets(ctx context.Context) ([]model.Node, error) {
	return p.store.ListNodes(ctx, NodeQuery{Kinds: []string{model.KindBudget}}, 0, 0)
}

// RecordExternalID records the project created for a leaf, whatever the leaf's
// status: one released meanwhile still needs the ID, or its project could never
// be cleaned up.
func (p *ProviderSync) RecordExternalID(ctx context.Context, leafID, externalID string) error {
	_, err := p.store.UpdateNode(ctx, leafID, func(n *model.Node) error {
		if n.ExternalID == externalID {
			return ErrSkipUpdate
		}
		n.ExternalID = externalID
		return nil
	})
	return err
}

// RecordObservation writes what a pass found in OpenStack, touching only those
// fields and only when something changed (see ApplyObservation).
func (p *ProviderSync) RecordObservation(ctx context.Context, leafID, externalID string, o *Observation) error {
	_, err := p.store.UpdateNode(ctx, leafID, func(n *model.Node) error {
		if !ApplyObservation(n, externalID, o) {
			return ErrSkipUpdate
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
			return ErrSkipUpdate
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
		prev := n.Purge
		removed := r.Removed
		if prev != nil {
			removed += prev.Removed
		}
		if prev != nil && prev.Step == r.Step && prev.Blocked == r.Blocked && prev.Detail == r.Detail && r.Removed == 0 {
			return ErrSkipUpdate
		}
		n.Purge = &model.PurgeProgress{
			Step: r.Step, Index: r.Index, Steps: r.Steps,
			Blocked: r.Blocked, Detail: r.Detail,
			Removed: removed, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
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
			return ErrSkipUpdate
		}
		n.Name = leaf.Name
		n.Reason = leaf.Reason
		n.Limit = leaf.Limit
		n.AuthorizedUsers = leaf.AuthorizedUsers
		n.ExternalGroupAssignments = leaf.ExternalGroupAssignments
		n.ExternalID = leaf.ExternalID
		n.ExternalName = leaf.ExternalName
		ApplyObservation(n, leaf.ExternalID, o)
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
			return ErrSkipUpdate
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

// Observation is what one pass read from OpenStack about a project. A nil
// *Observation means it could not be read — "not measured", never "nothing in
// use".
type Observation struct {
	Overcommitted bool
	InUse         common.ProjectQuota
	// Servers counts the project's servers, whatever their state, and
	// ServerLimit is how many OpenStack allows.
	Servers     int
	ServerLimit int
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
func ApplyObservation(n *model.Node, externalID string, o *Observation) bool {
	changed := n.ExternalID != externalID
	if o != nil {
		changed = changed || n.Overcommitted != o.Overcommitted || !quotaEqual(n.InUse, o.InUse) ||
			n.Servers == nil || *n.Servers != o.Servers ||
			n.ServerLimit == nil || *n.ServerLimit != o.ServerLimit ||
			(o.GrantUse != nil && !quotaEqual(n.GrantUse, o.GrantUse))
	}
	if !changed {
		return false
	}
	n.ExternalID = externalID
	if o != nil {
		n.Overcommitted = o.Overcommitted
		n.InUse = o.InUse
		servers, serverLimit := o.Servers, o.ServerLimit
		n.Servers = &servers
		n.ServerLimit = &serverLimit
		if o.GrantUse != nil {
			n.GrantUse = o.GrantUse
		}
	}
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
