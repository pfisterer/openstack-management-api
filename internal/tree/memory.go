package tree

import (
	"context"
	"sort"
	"sync"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// Ensure InMemoryStore implements the Store interface.
var _ Store = (*InMemoryStore)(nil)

// InMemoryStore is a test-friendly Store implementation.
type InMemoryStore struct {
	mu         sync.RWMutex
	identities []common.Identity
	nodes      []Node
	log        *zap.SugaredLogger
}

func NewInMemoryStore(log *zap.SugaredLogger) *InMemoryStore {
	return &InMemoryStore{
		identities: []common.Identity{},
		nodes:      []Node{},
		log:        log,
	}
}

func (s *InMemoryStore) IsEmpty(_ context.Context) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.identities) == 0 && len(s.nodes) == 0, nil
}

func (s *InMemoryStore) Seed(_ context.Context, identities []common.Identity, nodes []Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identities = identities
	s.nodes = nodes
	return nil
}

func (s *InMemoryStore) ListIdentities(_ context.Context) ([]common.Identity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]common.Identity, len(s.identities))
	copy(out, s.identities)
	return out, nil
}

func (s *InMemoryStore) ListParticipants(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ParticipantEmails(s.nodes), nil
}

func (s *InMemoryStore) GetNode(_ context.Context, id string) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy, not a pointer into the live slice, so callers cannot mutate
	// stored state without a lock.
	for i := range s.nodes {
		if s.nodes[i].ID == id {
			n := s.nodes[i]
			return &n, nil
		}
	}
	return nil, nil
}

func (s *InMemoryStore) ListNodes(_ context.Context, q NodeQuery, limit, offset int) ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Node
	for _, n := range s.nodes {
		if matchesQuery(n, q) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return paginateInMemory(out, limit, offset), nil
}

func (s *InMemoryStore) CountNodes(_ context.Context, q NodeQuery) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := 0
	for _, node := range s.nodes {
		if matchesQuery(node, q) {
			n++
		}
	}
	return n, nil
}

func (s *InMemoryStore) UpsertNode(_ context.Context, n Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.nodes {
		if s.nodes[i].ID == n.ID {
			s.nodes[i] = n
			return nil
		}
	}
	s.nodes = append(s.nodes, n)
	return nil
}

// CountChildren counts direct children per parent in a single pass.
func (s *InMemoryStore) CountChildren(_ context.Context, parentIDs []string) (map[string]int, error) {
	if len(parentIDs) == 0 {
		return map[string]int{}, nil
	}
	wanted := make(map[string]struct{}, len(parentIDs))
	for _, id := range parentIDs {
		wanted[id] = struct{}{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := make(map[string]int, len(parentIDs))
	for _, n := range s.nodes {
		if n.ParentID == nil {
			continue
		}
		if _, ok := wanted[*n.ParentID]; ok {
			counts[*n.ParentID]++
		}
	}
	return counts, nil
}

func (s *InMemoryStore) DeleteNodes(_ context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		remove[id] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		if _, ok := remove[n.ID]; !ok {
			filtered = append(filtered, n)
		}
	}
	s.nodes = filtered
	return nil
}

func paginateInMemory[T any](items []T, limit, offset int) []T {
	if limit <= 0 {
		limit = len(items)
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return []T{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	out := make([]T, end-offset)
	copy(out, items[offset:end])
	return out
}
