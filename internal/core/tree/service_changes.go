package tree

import (
	"context"

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

func (s signallingStore) UpsertNode(ctx context.Context, n model.Node) error {
	err := s.Store.UpsertNode(ctx, n)
	if err == nil {
		s.changed()
	}
	return err
}

func (s signallingStore) DeleteNodes(ctx context.Context, ids []string) error {
	err := s.Store.DeleteNodes(ctx, ids)
	if err == nil && len(ids) > 0 {
		s.changed()
	}
	return err
}

func (s signallingStore) UpdateNode(ctx context.Context, id string, fn func(n *model.Node) error) (bool, error) {
	wrote, err := s.Store.UpdateNode(ctx, id, fn)
	if err == nil && wrote {
		s.changed()
	}
	return wrote, err
}

func (s signallingStore) DeleteNodeIf(ctx context.Context, id string, pred func(n model.Node) bool) (bool, error) {
	deleted, err := s.Store.DeleteNodeIf(ctx, id, pred)
	if err == nil && deleted {
		s.changed()
	}
	return deleted, err
}

func (s signallingStore) Seed(ctx context.Context, identities []common.Identity, nodes []model.Node) error {
	err := s.Store.Seed(ctx, identities, nodes)
	if err == nil {
		s.changed()
	}
	return err
}
