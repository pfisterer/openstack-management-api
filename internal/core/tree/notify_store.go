package tree

import (
	"context"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// NotifyOnWrite wraps a store so that notify is called after every node write
// that happened. The service gets the wrapped store, the reconciler the plain
// one: a change made through the API (REST or MCP) then starts a reconciler run
// right away instead of waiting for the next interval, and the reconciler's own
// writes do not start another run of themselves.
//
// Every write notifies, including ones without an OpenStack consequence (a
// pending request): telling them apart would repeat the reconciler's own
// decisions here, and a run that finds nothing to do is cheap. The reconciler
// folds notifications that arrive during a run into one follow-up run.
func NotifyOnWrite(store Store, notify func()) Store {
	return notifyingStore{Store: store, notify: notify}
}

type notifyingStore struct {
	Store
	notify func()
}

func (s notifyingStore) UpsertNode(ctx context.Context, n Node) error {
	err := s.Store.UpsertNode(ctx, n)
	if err == nil {
		s.notify()
	}
	return err
}

func (s notifyingStore) DeleteNodes(ctx context.Context, ids []string) error {
	err := s.Store.DeleteNodes(ctx, ids)
	if err == nil && len(ids) > 0 {
		s.notify()
	}
	return err
}

func (s notifyingStore) UpdateNode(ctx context.Context, id string, fn func(n *Node) error) (bool, error) {
	wrote, err := s.Store.UpdateNode(ctx, id, fn)
	if err == nil && wrote {
		s.notify()
	}
	return wrote, err
}

func (s notifyingStore) DeleteNodeIf(ctx context.Context, id string, pred func(n Node) bool) (bool, error) {
	deleted, err := s.Store.DeleteNodeIf(ctx, id, pred)
	if err == nil && deleted {
		s.notify()
	}
	return deleted, err
}

func (s notifyingStore) Seed(ctx context.Context, identities []common.Identity, nodes []Node) error {
	err := s.Store.Seed(ctx, identities, nodes)
	if err == nil {
		s.notify()
	}
	return err
}
