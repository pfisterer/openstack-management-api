package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// Tree is what the admin steps change in the tree (tree.Service).
type Tree interface {
	Holders(ctx context.Context, id string) ([]tree.Holder, error)
	WithdrawAvailability(ctx context.Context, id string) (int, error)
	ForgetResource(ctx context.Context, id string) (int, error)
	AdoptAvailability(ctx context.Context, id string) error
	ProjectsByOSID(ctx context.Context) (map[string]tree.Holder, error)
}

// OpenStack checks a target before it is offered and reads who holds a grant
// before it is forgotten. ErrUnavailable says the cloud cannot be asked right
// now (still connecting).
type OpenStack interface {
	CheckGrantTarget(g common.Grant) error
	GrantedProjects(g common.Grant) ([]string, error)
}

var ErrUnavailable = errors.New("OpenStack is not connected yet")

// Admin runs the root admins' steps on the catalogue and the tree together.
type Admin struct {
	Catalog *Catalog
	Tree    Tree
	// OpenStack is nil without a reconciler: then nothing is checked there,
	// and nothing is granted there either.
	OpenStack OpenStack
}

// View is an entry with how many nodes hold it.
type View struct {
	Entry
	Holders int `json:"holders"`
}

// Status is everything that decides whether an availability can go: who in
// the tree holds it, and which projects OpenStack still grants it to.
type Status struct {
	Entry   Entry         `json:"entry"`
	Holders []tree.Holder `json:"holders"`
	// OpenStack is absent when there is no cloud to ask.
	OpenStack *GrantState `json:"openstack,omitempty"`
}

// GrantState is what OpenStack shows for a grant. Managed are projects of
// this portal; the others are grants somebody made by hand, or remnants of
// projects that no longer exist — they never block removal, because the
// reconciler never touched them.
type GrantState struct {
	Error     string        `json:"error,omitempty"`
	Managed   []tree.Holder `json:"managed"`
	Unmanaged []string      `json:"unmanaged"`
}

func (a *Admin) List(ctx context.Context) ([]View, error) {
	out := []View{}
	for _, e := range a.Catalog.Entries() {
		h, err := a.Tree.Holders(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, View{Entry: e, Holders: len(h)})
	}
	return out, nil
}

func (a *Admin) Status(ctx context.Context, id string) (Status, error) {
	e, ok := a.Catalog.Get(id)
	if !ok || e.State == StateRemoved {
		return Status{}, ErrNotFound
	}
	h, err := a.Tree.Holders(ctx, id)
	if err != nil {
		return Status{}, err
	}
	st := Status{Entry: e, Holders: h}
	if a.OpenStack != nil {
		gs, err := a.grantState(ctx, e.Grant)
		if err != nil {
			gs = &GrantState{Error: err.Error(), Managed: []tree.Holder{}, Unmanaged: []string{}}
		}
		st.OpenStack = gs
	}
	return st, nil
}

func (a *Admin) grantState(ctx context.Context, g common.Grant) (*GrantState, error) {
	projects, err := a.OpenStack.GrantedProjects(g)
	if err != nil {
		return nil, err
	}
	byOS, err := a.Tree.ProjectsByOSID(ctx)
	if err != nil {
		return nil, err
	}
	gs := &GrantState{Managed: []tree.Holder{}, Unmanaged: []string{}}
	for _, p := range projects {
		if h, ok := byOS[p]; ok {
			gs.Managed = append(gs.Managed, h)
		} else {
			gs.Unmanaged = append(gs.Unmanaged, p)
		}
	}
	return gs, nil
}

// Add offers a new availability. The target is checked first: an entry
// pointing at nothing, or at a flavour Nova keeps no access list for, would
// fail on every reconcile pass in every project it is granted to.
func (a *Admin) Add(ctx context.Context, in NewEntry, actor string) (Entry, error) {
	if a.OpenStack != nil {
		if err := a.OpenStack.CheckGrantTarget(in.Grant); err != nil {
			if errors.Is(err, ErrUnavailable) {
				return Entry{}, err
			}
			return Entry{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	e, err := a.Catalog.add(ctx, in, actor)
	if err != nil {
		return Entry{}, err
	}
	return e, a.Tree.AdoptAvailability(ctx, e.ID)
}

func (a *Admin) Update(ctx context.Context, id string, in NewEntry, actor string) (Entry, error) {
	return a.Catalog.update(ctx, id, in.Name, in.Group, in.Message, actor)
}

// Withdraw takes the availability away everywhere. The state changes first,
// so nothing can be granted while the tree is rewritten; then every node is
// set to 0 and the reconciler revokes it in OpenStack on its next pass.
// Repeating it is harmless, and finishes one that failed half way.
func (a *Admin) Withdraw(ctx context.Context, id, actor string) (Entry, int, error) {
	e, err := a.Catalog.setState(ctx, id, []string{StateActive}, StateWithdrawn, actor)
	if err != nil {
		return Entry{}, 0, err
	}
	n, err := a.Tree.WithdrawAvailability(ctx, id)
	return e, n, err
}

// Restore offers a withdrawn availability again. Nothing is granted back:
// the root holds it again, everyone else starts from 0.
func (a *Admin) Restore(ctx context.Context, id, actor string) (Entry, error) {
	e, err := a.Catalog.setState(ctx, id, []string{StateWithdrawn}, StateActive, actor)
	if err != nil {
		return Entry{}, err
	}
	return e, a.Tree.AdoptAvailability(ctx, id)
}

// Remove takes a withdrawn availability out of the catalogue for good. Only
// once nothing holds it and OpenStack grants it to no managed project: from
// then on the reconciler never looks at it again, so a grant left now would
// stay forever.
func (a *Admin) Remove(ctx context.Context, id, actor string) error {
	e, ok := a.Catalog.Get(id)
	if !ok || e.State == StateRemoved {
		return ErrNotFound
	}
	if e.State != StateWithdrawn {
		return fmt.Errorf("%w: %q has to be withdrawn first", ErrConflict, id)
	}
	h, err := a.Tree.Holders(ctx, id)
	if err != nil {
		return err
	}
	if len(h) > 0 {
		return fmt.Errorf("%w: %d node(s) still hold %q (e.g. %s) — withdraw it again", ErrConflict, len(h), id, h[0].Name)
	}
	if a.OpenStack != nil {
		gs, err := a.grantState(ctx, e.Grant)
		if err != nil {
			return fmt.Errorf("cannot tell whether OpenStack still grants %q: %w", id, err)
		}
		if len(gs.Managed) > 0 {
			names := make([]string, 0, len(gs.Managed))
			for _, m := range gs.Managed {
				names = append(names, m.Name)
			}
			slices.Sort(names)
			return fmt.Errorf("%w: OpenStack still grants %q to %d project(s) (%s) — the reconciler has not revoked it yet, or OpenStack refuses because it is in use",
				ErrConflict, id, len(names), names[0])
		}
	}
	if _, err := a.Tree.ForgetResource(ctx, id); err != nil {
		return err
	}
	_, err = a.Catalog.setState(ctx, id, []string{StateWithdrawn}, StateRemoved, actor)
	return err
}
