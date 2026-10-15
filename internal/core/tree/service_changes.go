package tree

import (
	"context"
	"errors"
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// ProviderSync is a provider's view of this tree. Its Changes fire when the service changed what is wanted, so a change made through the API (REST or MCP) is carried out right away instead of at the next interval. Each view has its own channel: with two providers, both hear of every change. A provider's own reports go to the plain store and do not fire, or every run would start the next one.
func (s *Service) ProviderSync(provider string) *ProviderSync {
	ch := make(chan struct{}, 1)
	s.changesMu.Lock()
	s.changes = append(s.changes, ch)
	s.changesMu.Unlock()
	return &ProviderSync{store: s.plain, provider: provider, changes: ch, resources: s.resources}
}

// signalChange marks that the wanted state changed, for every provider. Non-blocking with room for one each: changes that arrive while a provider runs fold into one follow-up run, and the interval catches anything else.
func (s *Service) signalChange() {
	s.changesMu.Lock()
	defer s.changesMu.Unlock()
	for _, ch := range s.changes {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// signallingStore is the store as the service writes to it: every write that happened signals a change. Kept at the store rather than at each operation so that no operation can forget it. Every write counts, including ones without a consequence for the provider (a pending request): telling them apart would repeat the reconciler's decisions here, and a run that finds nothing to do is cheap.
type signallingStore struct {
	nodestore.Store
	changed func()
}

// internal marks a failure of the store itself as common.ErrInternal: the request was fine, the service was not. Wrapped here, where every store call of the service passes, so no operation can return a database error as if it were a validation message.
func internal(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", common.ErrInternal, err)
}

func (s signallingStore) IsEmpty(ctx context.Context) (bool, error) {
	empty, err := s.Store.IsEmpty(ctx)
	return empty, internal(err)
}

func (s signallingStore) ListIdentities(ctx context.Context) ([]common.Identity, error) {
	out, err := s.Store.ListIdentities(ctx)
	return out, internal(err)
}

func (s signallingStore) ListParticipants(ctx context.Context) ([]string, error) {
	out, err := s.Store.ListParticipants(ctx)
	return out, internal(err)
}

func (s signallingStore) GetNode(ctx context.Context, id string) (*model.Node, error) {
	n, err := s.Store.GetNode(ctx, id)
	return n, internal(err)
}

func (s signallingStore) ListNodes(ctx context.Context, q nodestore.NodeQuery, limit, offset int) ([]model.Node, error) {
	out, err := s.Store.ListNodes(ctx, q, limit, offset)
	return out, internal(err)
}

func (s signallingStore) CountNodes(ctx context.Context, q nodestore.NodeQuery) (int, error) {
	n, err := s.Store.CountNodes(ctx, q)
	return n, internal(err)
}

func (s signallingStore) CountChildren(ctx context.Context, parentIDs []string) (map[string]nodestore.ChildCounts, error) {
	out, err := s.Store.CountChildren(ctx, parentIDs)
	return out, internal(err)
}

func (s signallingStore) UpsertNode(ctx context.Context, n model.Node) error {
	err := s.Store.UpsertNode(ctx, n)
	if err == nil {
		s.changed()
	}
	return internal(err)
}

func (s signallingStore) DeleteNodes(ctx context.Context, ids []string) error {
	err := s.Store.DeleteNodes(ctx, ids)
	if err == nil && len(ids) > 0 {
		s.changed()
	}
	return internal(err)
}

// UpdateNode passes on what fn returned as it is: that is the operation's own verdict on the node (a conflict, a validation error), not a failure of the store.
func (s signallingStore) UpdateNode(ctx context.Context, id string, fn func(n *model.Node) error) (bool, error) {
	var fnErr error
	wrote, err := s.Store.UpdateNode(ctx, id, func(n *model.Node) error {
		fnErr = fn(n)
		return fnErr
	})
	if err == nil && wrote {
		s.changed()
	}
	if err != nil && (fnErr == nil || !errors.Is(err, fnErr)) {
		err = internal(err)
	}
	return wrote, err
}

func (s signallingStore) DeleteNodeIf(ctx context.Context, id string, pred func(n model.Node) bool) (bool, error) {
	deleted, err := s.Store.DeleteNodeIf(ctx, id, pred)
	if err == nil && deleted {
		s.changed()
	}
	return deleted, internal(err)
}

func (s signallingStore) Seed(ctx context.Context, identities []common.Identity, nodes []model.Node) error {
	err := s.Store.Seed(ctx, identities, nodes)
	if err == nil {
		s.changed()
	}
	return internal(err)
}
